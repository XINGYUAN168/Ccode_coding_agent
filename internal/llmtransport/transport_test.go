package llmtransport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// canary 是替代 API key，用于证明它在出站请求中正确携带，同时绝不出现在错误字符串中。
const canary = "sk-canary-secret-ABCDEFGH1234567890"

type ctxKey struct{}

// roundTripFunc 将函数转换为 http.RoundTripper。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// capture 记录到达基础传输的每个请求。
type capture struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (c *capture) add(r *http.Request) {
	c.mu.Lock()
	c.reqs = append(c.reqs, r)
	c.mu.Unlock()
}

func (c *capture) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func (c *capture) last() *http.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqs[len(c.reqs)-1]
}

func okResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}
}

// newSDKRequest 构建形态类似厂商 SDK 传入的请求：POST 到 SDK 默认 endpoint，携带 JSON
// 正文、认证头和 SSE 头，并在 context 中加入追踪值。
func newSDKRequest(t *testing.T) *http.Request {
	t.Helper()
	body := `{"model":"claude-test","stream":true}`
	req, err := http.NewRequest(http.MethodPost,
		"https://api.anthropic.com/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+canary)
	req.Header.Set("x-api-key", canary)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	ctx := context.WithValue(req.Context(), ctxKey{}, "trace-42")
	return req.WithContext(ctx)
}

func readBody(t *testing.T, r io.ReadCloser) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 完整 endpoint 重写测试。
// ---------------------------------------------------------------------------

func TestEndpointRewrite_FullURL(t *testing.T) {
	target := mustParse(t, "https://gateway.example.com:8443/api/v2/chat%2Fcompletions?stream=true&embed=1")

	var cap capture
	rt := NewEndpointTransport(target, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cap.add(r)
		return okResponse(), nil
	}))

	req := newSDKRequest(t)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if cap.len() != 1 {
		t.Fatalf("expected 1 outbound request, got %d", cap.len())
	}
	got := cap.last()

	if got.URL.Scheme != "https" {
		t.Errorf("scheme = %q, want https", got.URL.Scheme)
	}
	if got.URL.Host != "gateway.example.com:8443" {
		t.Errorf("host = %q", got.URL.Host)
	}
	// Path 已解码，RawPath 精确保留 %2F 编码。
	if got.URL.Path != "/api/v2/chat/completions" {
		t.Errorf("path = %q", got.URL.Path)
	}
	if got.URL.RawPath != "/api/v2/chat%2Fcompletions" {
		t.Errorf("rawpath = %q, want /api/v2/%%2Fcompletions", got.URL.RawPath)
	}
	if got.URL.RawQuery != "stream=true&embed=1" {
		t.Errorf("rawquery = %q", got.URL.RawQuery)
	}
	// 请求行必须原样携带已编码 path 和 query。
	if want := "/api/v2/chat%2Fcompletions?stream=true&embed=1"; got.URL.RequestURI() != want {
		t.Errorf("request URI = %q, want %q", got.URL.RequestURI(), want)
	}
	// SDK 自身资源 path 绝不能残留在任何位置。
	if strings.Contains(got.URL.String(), "/v1/messages") {
		t.Errorf("SDK path leaked: %s", got.URL.String())
	}

	// 保留 method、body、headers、host 语义和 context。
	if got.Method != http.MethodPost {
		t.Errorf("method = %q", got.Method)
	}
	if b := readBody(t, got.Body); b != `{"model":"claude-test","stream":true}` {
		t.Errorf("body = %q", b)
	}
	if got.Header.Get("Authorization") != "Bearer "+canary {
		t.Errorf("Authorization header not preserved: %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("x-api-key") != canary {
		t.Errorf("x-api-key header not preserved")
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type not preserved: %q", got.Header.Get("Content-Type"))
	}
	if got.Header.Get("Accept") != "text/event-stream" {
		t.Errorf("Accept not preserved")
	}
	if got.Host != "gateway.example.com:8443" {
		t.Errorf("outgoing Host = %q", got.Host)
	}
	if v := got.Context().Value(ctxKey{}); v != "trace-42" {
		t.Errorf("context value = %v", v)
	}
}

func TestEndpointRewrite_DoesNotMutateOriginal(t *testing.T) {
	target := mustParse(t, "https://gateway.example.com/complete")
	rt := NewEndpointTransport(target, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return okResponse(), nil
	}))

	req := newSDKRequest(t)
	beforePath := req.URL.Path
	beforeRawQuery := req.URL.RawQuery
	beforeAuth := req.Header.Get("Authorization")
	beforeBody, _ := io.ReadAll(req.Body)
	// 恢复正文，使克隆对象可通过 GetBody 回卷
	req.Body = io.NopCloser(strings.NewReader(string(beforeBody)))

	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if req.URL.Path != beforePath || req.URL.Path != "/v1/messages" {
		t.Errorf("original path mutated: %q", req.URL.Path)
	}
	if req.URL.RawQuery != beforeRawQuery {
		t.Errorf("original rawquery mutated")
	}
	if req.Header.Get("Authorization") != beforeAuth {
		t.Errorf("original Authorization mutated")
	}
	if req.URL.Host != "api.anthropic.com" {
		t.Errorf("original host mutated: %q", req.URL.Host)
	}
}

func TestEndpointRewrite_TrailingSlashAndNoAppend(t *testing.T) {
	target := mustParse(t, "https://host.example.com/root/")
	var got *url.URL
	rt := NewEndpointTransport(target, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.URL
		return okResponse(), nil
	}))

	req := newSDKRequest(t)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got.Path != "/root/" {
		t.Errorf("path = %q, want /root/", got.Path)
	}
	if got.RequestURI() != "/root/" {
		t.Errorf("request URI = %q, want /root/", got.RequestURI())
	}
	if strings.Contains(got.String(), "/v1") {
		t.Errorf("SDK path remnant: %s", got.String())
	}
}

func TestEndpointRewrite_EmptyTargetPassthrough(t *testing.T) {
	called := false
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		// URL 保持不变。
		if r.URL.Path != "/v1/messages" {
			t.Errorf("passthrough altered path: %q", r.URL.Path)
		}
		return okResponse(), nil
	})

	// nil target 不得包装：返回的传输必须原样分派给 base。通过运行请求并观察 base 看到
	// 原始 SDK URL 来验证。
	rt := NewEndpointTransport(nil, base)
	req := newSDKRequest(t)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !called {
		t.Fatal("base transport not invoked")
	}
	if req.URL.Host != "api.anthropic.com" {
		t.Errorf("passthrough changed host: %q", req.URL.Host)
	}

	// 只有存在 target 时才应引入包装器。
	wrapped := NewEndpointTransport(mustParse(t, "https://other.example.com/x"), base)
	if _, ok := wrapped.(*endpointTransport); !ok {
		t.Fatalf("with target, expected *endpointTransport, got %T", wrapped)
	}
}

// ---------------------------------------------------------------------------
// 长流客户端测试。
// ---------------------------------------------------------------------------

func TestHTTPClient_ZeroTimeoutAndTunedTransport(t *testing.T) {
	client := New("https://example.com", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return okResponse(), nil
	}))
	if client.Timeout != 0 {
		t.Errorf("client.Timeout = %v, want 0 (context owns the stream)", client.Timeout)
	}

	dt := DefaultTransport()
	if dt.DialContext == nil {
		t.Error("DialContext not configured")
	}
	if dt.TLSHandshakeTimeout <= 0 {
		t.Error("TLSHandshakeTimeout not positive")
	}
	if dt.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout not positive")
	}
	if dt.IdleConnTimeout <= 0 {
		t.Error("IdleConnTimeout not positive")
	}
	if dt.DisableKeepAlives {
		t.Error("keep-alive must stay enabled for connection reuse")
	}
}

func TestHTTPClient_InjectedBaseIsUsed(t *testing.T) {
	saw := false
	client := New("https://example.com", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		saw = true
		if r.URL.Host != "example.com" {
			t.Errorf("rewrite target host = %q", r.URL.Host)
		}
		return okResponse(), nil
	}))
	req := newSDKRequest(t)
	if _, err := client.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !saw {
		t.Fatal("injected base RoundTripper was not used")
	}
}

func TestHTTPClient_ContextCancel(t *testing.T) {
	client := New("https://example.com", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	req := newSDKRequest(t).WithContext(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := client.Do(req)
		done <- err
	}()
	// 给 RoundTrip 一点时间阻塞在 ctx.Done
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not stop on cancel")
	}
}

func TestHTTPClient_ContextDeadline(t *testing.T) {
	client := New("https://example.com", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	req := newSDKRequest(t).WithContext(ctx)

	start := time.Now()
	_, err := client.Do(req)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("deadline took %v", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// ---------------------------------------------------------------------------
// 重定向安全。
// ---------------------------------------------------------------------------

func TestRedirect_CrossOriginRejected(t *testing.T) {
	var cap capture
	client := New("https://first.example.com/complete", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cap.add(r)
		// 首跳返回携带认证信息、指向不同源的重定向。
		resp := &http.Response{
			StatusCode: http.StatusFound,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}
		resp.Header.Set("Location", "http://evil.example.com/steal")
		return resp, nil
	}))

	req := newSDKRequest(t)
	_, err := client.Do(req)

	if !errors.Is(err, ErrCrossOriginRedirect) {
		t.Fatalf("err = %v, want ErrCrossOriginRedirect", err)
	}
	if cap.len() != 1 {
		t.Fatalf("cross-origin redirect followed: %d outbound calls", cap.len())
	}
	// 金丝雀绝不能出现在错误字符串中。
	if strings.Contains(err.Error(), canary) {
		t.Errorf("canary leaked into error: %v", err)
	}
}

func TestRedirect_SameOriginFollowed(t *testing.T) {
	var cap capture
	client := New("https://first.example.com/complete", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cap.add(r)
		if cap.len() == 1 {
			resp := &http.Response{
				StatusCode: http.StatusFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}
			resp.Header.Set("Location", "https://first.example.com/other")
			return resp, nil
		}
		return okResponse(), nil
	}))

	req, _ := http.NewRequest(http.MethodGet, "https://first.example.com/v1/messages", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("same-origin redirect should be followed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if cap.len() != 2 {
		t.Fatalf("expected 2 outbound calls (same-origin redirect re-sent), got %d", cap.len())
	}
}

// ---------------------------------------------------------------------------
// 针对真实 localhost 服务的连接复用。
// ---------------------------------------------------------------------------

func TestTransport_ConnectionReuse(t *testing.T) {
	var (
		mu       sync.Mutex
		newConns int
	)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			newConns++
			mu.Unlock()
		}
	}
	srv.Start() // ConnState 须在服务协程启动前装配，避免与 conn.setState 读并发
	defer srv.Close()

	client := New(srv.URL, nil) // 调优的 DefaultTransport，重写到 srv

	for i := 0; i < 2; i++ {
		resp, err := client.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if newConns != 1 {
		t.Errorf("expected 1 reused connection across 2 requests, got %d new conns", newConns)
	}
}

// ---------------------------------------------------------------------------
// 辅助函数。
// ---------------------------------------------------------------------------

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
