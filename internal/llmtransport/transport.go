// Package llmtransport 提供所有模型适配器共享的轻量、无依赖 HTTP 管道。它有两个用途：
//
//  1. 完整 endpoint 覆盖。厂商 SDK 总会向可配置 base URL 追加固定资源 path（例如
//     /v1/messages 或 /v1/chat/completions）。Ccode 则把 base_url 当作完整请求 URL：
//     设置后，出站请求的 scheme、host、path、raw path 和 query 会精确重写为配置 URL，
//     不追加任何内容。endpointTransport 在克隆请求上重写，绝不修改 SDK 原始请求对象。
//
//  2. 适合长流的客户端。聊天补全流可能持续数分钟，因此 http.Client.Timeout 必须保持为零，
//     流生命周期由请求 context 管理。具体传输仍限制拨号、TLS 握手、响应头和空闲连接时间，
//     并拒绝跨源重定向，使 Authorization 材料绝不会重放到其它 host。
//
// 本包有意只导入标准库，不了解 Provider、SDK、配置类型或秘密。
package llmtransport

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrCrossOriginRedirect 在响应要求客户端重定向到与实际线上请求不同的源时返回
// （由 net/url 包装）。它是固定且不含秘密的句子，不插入 URL、host 或请求头值，
// 因而凭证无法通过错误字符串泄漏。
var ErrCrossOriginRedirect = errors.New("llmtransport: refusing redirect to a different origin")

// endpointTransport 是 http.RoundTripper；配置 target 时，它会先把每个出站请求 URL
// 重写为该完整目标，再交给底层传输。target 为 nil 时禁用重写，包装器原样透传请求。
//
// 它绝不修改收到的请求，而是先克隆，使构造请求的 SDK 保留原始对象（相同 method、body、
// headers 和 context）；只重写克隆对象的 URL。
type endpointTransport struct {
	target *url.URL
	base   http.RoundTripper
}

// NewEndpointTransport 包装 base，使 target 非空时每个通过它发送的请求都重写为 target 的
// 完整 URL。target 为 nil（或没有 host）时不应用包装，原样返回 base。base 为 nil 时
// 替换为调优的 DefaultTransport。
//
// 重写会整体替换 scheme、host、path、raw path、raw query 和 force-query，绝不追加 SDK
// 资源路径。因此 SDK 为 https://api.anthropic.com/v1/messages 等地址构建的请求最终会
// 精确指向配置 URL，不残留 /v1/messages。
func NewEndpointTransport(target *url.URL, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = DefaultTransport()
	}
	if target == nil || target.Host == "" {
		return base
	}
	// 防御性复制：不能让调用方之后修改 *url.URL，从而改变已创建传输固定的目标。
	t := *target
	t.User = nil // 绝不把用户信息或凭证重新附加到出站 URL
	return &endpointTransport{target: &t, base: base}
}

// RoundTrip 克隆 req，把克隆的 URL 重写为固定目标后转发。原始 req 逐字节保持不变，
// 请求 context、method、body 和 headers 会带到克隆对象。
func (t *endpointTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = t.target.Scheme
	out.URL.Host = t.target.Host
	out.URL.Path = t.target.Path
	out.URL.RawPath = t.target.RawPath
	out.URL.RawQuery = t.target.RawQuery
	out.URL.ForceQuery = t.target.ForceQuery
	out.URL.User = nil
	out.URL.Fragment = ""
	// 请求现在属于目标 host：将 Host 头（以及虚拟主机选择）路由到它，而不是 SDK 原本寻址的
	// host。req.Header 本身保持不变。
	out.Host = t.target.Host
	return t.base.RoundTrip(out)
}

// New 构建针对可选完整 endpoint 调优、适合长时间 SSE 流的 *http.Client。
//
//   - baseURL 是可选完整请求 URL。为空时请求保留调用方（SDK）构建的 URL；非空时每个请求
//     都精确重写到该 URL（见 NewEndpointTransport）。
//   - base 注入底层 RoundTripper；测试借此观察出站请求并桩替响应。base 为 nil 时使用
//     DefaultTransport。
//
// 返回客户端的 Timeout == 0：流式轮次可能运行数分钟，其生命周期完全由请求 context 管理。
// 离开首跳源的重定向会被拒绝。
func New(baseURL string, base http.RoundTripper) *http.Client {
	if base == nil {
		base = DefaultTransport()
	}
	rt := NewEndpointTransport(parseTarget(baseURL), base)
	return &http.Client{
		Transport:     rt,
		Timeout:       0, // 长流由 context 管理截止时间
		CheckRedirect: checkRedirect,
	}
}

// DefaultTransport 返回新的 *http.Transport，为建连、TLS 握手和响应头设置具体超时，
// 并复用空闲连接。它从不设置客户端级读取截止时间；流式正文自身无固定上限，
// 由请求 context 管理。
//
// 每次调用都返回新实例，使调用方可共享或修改它，而不会与包级默认值发生竞态。
func DefaultTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// 只限制等待响应头的时间，不限制流式正文。
		ResponseHeaderTimeout: 60 * time.Second,
	}
}

// checkRedirect 允许同源重定向，但拒绝任何改变 scheme 或 host 的跳转，使为配置 endpoint
// 设置的 Authorization 头绝不会重放到其它服务端。
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if sameOrigin(via[0].URL, req.URL) {
		return nil
	}
	return ErrCrossOriginRedirect
}

// sameOrigin 报告 a 和 b 是否共享 scheme 与 host（即源）。此比较中端口属于 host，
// 因此 :8080 -> :9000 视为不同源。
func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Scheme == b.Scheme && a.Host == b.Host
}

// parseTarget 将原始配置 base_url 转换为固定重写目标。空值或仅空白值产生 nil，从而禁用
// 重写。非法 URL 视为“无覆盖”：上游配置会校验它，但传输层绝不能 panic 或拒绝构建客户端。
func parseTarget(raw string) *url.URL {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}
	return u
}
