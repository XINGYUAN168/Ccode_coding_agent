package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccode/internal/conversation"
	"ccode/internal/llm"
)

func writeAnthropicTextStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: message_start\n")
	_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"test","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}}`+"\n\n")
	_, _ = io.WriteString(w, "event: content_block_start\n")
	_, _ = io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`+"\n\n")
	_, _ = io.WriteString(w, "event: content_block_delta\n")
	_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"secret-thought-canary"}}`+"\n\n")
	_, _ = io.WriteString(w, "event: content_block_delta\n")
	_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"secret-signature-canary"}}`+"\n\n")
	_, _ = io.WriteString(w, "event: content_block_stop\n")
	_, _ = io.WriteString(w, `data: {"type":"content_block_stop","index":0}`+"\n\n")
	_, _ = io.WriteString(w, "event: content_block_start\n")
	_, _ = io.WriteString(w, `data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`+"\n\n")
	_, _ = io.WriteString(w, "event: content_block_delta\n")
	payload, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": text}})
	_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
	_, _ = io.WriteString(w, "event: content_block_stop\n")
	_, _ = io.WriteString(w, `data: {"type":"content_block_stop","index":1}`+"\n\n")
	_, _ = io.WriteString(w, "event: message_delta\n")
	_, _ = io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`+"\n\n")
	_, _ = io.WriteString(w, "event: message_stop\n")
	_, _ = io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
}

func drain(t *testing.T, client llm.Client, conv *conversation.Manager) ([]llm.StreamEvent, error) {
	t.Helper()
	events, errs := client.Stream(context.Background(), conv, nil)
	var got []llm.StreamEvent
	for event := range events {
		got = append(got, event)
	}
	var streamErr error
	for err := range errs {
		if err != nil {
			streamErr = err
		}
	}
	return got, streamErr
}

func TestConstructionDoesNotUseNetwork(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, context.Canceled
	})
	httpClient := &http.Client{Transport: transport}
	if _, err := New("provider", "model", "key", "", false, "system", 8192, httpClient); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("构造阶段发生 %d 次网络请求", calls.Load())
	}
}

func TestThinkingRequestAndStreamEvents(t *testing.T) {
	var captured map[string]any
	var requestURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.URL.RequestURI()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		writeAnthropicTextStream(w, "你好")
	}))
	defer srv.Close()

	client, err := New("provider", "claude-test", "explicit-key", srv.URL+"/custom/messages?region=cn", true, "system", 8192, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("hello")
	events, streamErr := drain(t, client, conv)
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	if requestURI != "/custom/messages?region=cn" {
		t.Fatalf("请求地址 = %q", requestURI)
	}
	thinking, ok := captured["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(8191) {
		t.Fatalf("thinking 配置 = %#v", captured["thinking"])
	}
	var text, thinkingText, signature string
	var completed, ends int
	for _, event := range events {
		switch value := event.(type) {
		case llm.TextDelta:
			text += value.Text
		case llm.ThinkingDelta:
			thinkingText += value.Text
		case llm.ThinkingComplete:
			completed++
			if value.Thinking != "secret-thought-canary" {
				t.Fatalf("完整思考 = %q", value.Thinking)
			}
			signature = value.Signature
		case llm.StreamEnd:
			ends++
			if value.Usage.CacheReadTokens != 3 || value.Usage.CacheCreationTokens != 2 {
				t.Fatalf("缓存用量 = %+v", value.Usage)
			}
		}
	}
	if text != "你好" || thinkingText != "secret-thought-canary" || signature != "secret-signature-canary" || completed != 1 || ends != 1 {
		t.Fatalf("text=%q thinking=%q signature=%q completed=%d ends=%d events=%#v", text, thinkingText, signature, completed, ends, events)
	}
}

func TestThinkingRequestPreservedForToolContinuation(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		writeAnthropicTextStream(w, "done")
	}))
	defer srv.Close()
	client, err := New("provider", "claude-test", "key", srv.URL+"/v1/messages", true, "system", 8192, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("run")
	conv.AddAssistantMessageWithTools("", []conversation.ToolUseBlock{{ToolUseID: "call-1", ToolName: "ReadFile", Arguments: map[string]any{"path": "a"}}})
	conv.AddToolResultMessage("call-1", "ok", false)
	conv.AddUserMessage("continue")
	if _, streamErr := drain(t, client, conv); streamErr != nil {
		t.Fatal(streamErr)
	}
	thinking, exists := captured["thinking"].(map[string]any)
	if !exists || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(8191) {
		t.Fatalf("工具续接请求应保留 thinking: %#v", captured["thinking"])
	}
}

func TestSupportsAdaptiveThinking(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{model: "claude-sonnet-4-6", want: true},
		{model: "claude-sonnet-4-6-20250514", want: true},
		{model: "claude-opus-4-7", want: true},
		{model: "claude-sonnet-4-5-20250514", want: false},
		{model: "claude-3-5-sonnet-20241022", want: false},
		{model: "glm-4.7", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := supportsAdaptiveThinking(tt.model); got != tt.want {
				t.Fatalf("supportsAdaptiveThinking(%q) = %v，want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestAdaptiveThinkingRequest(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		writeAnthropicTextStream(w, "ok")
	}))
	defer srv.Close()

	client, err := New("provider", "claude-sonnet-4-6", "key", srv.URL+"/v1/messages", true, "system", 64000, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("hello")
	if _, streamErr := drain(t, client, conv); streamErr != nil {
		t.Fatal(streamErr)
	}
	thinking, ok := captured["thinking"].(map[string]any)
	if !ok || thinking["type"] != "adaptive" {
		t.Fatalf("自适应思考配置 = %#v", captured["thinking"])
	}
	if _, exists := thinking["budget_tokens"]; exists {
		t.Fatalf("自适应思考不应包含固定预算: %#v", thinking)
	}
	if captured["max_tokens"] != float64(64000) {
		t.Fatalf("max_tokens = %#v，want 64000", captured["max_tokens"])
	}
}

func TestThinkingDisabledRequest(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		writeAnthropicTextStream(w, "ok")
	}))
	defer srv.Close()

	client, err := New("provider", "claude-sonnet-4-6", "key", srv.URL+"/v1/messages", false, "system", 8192, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("hello")
	if _, streamErr := drain(t, client, conv); streamErr != nil {
		t.Fatal(streamErr)
	}
	if _, exists := captured["thinking"]; exists {
		t.Fatalf("关闭思考时不应包含 thinking: %#v", captured["thinking"])
	}
	if captured["max_tokens"] != float64(8192) {
		t.Fatalf("max_tokens = %#v，want 8192", captured["max_tokens"])
	}
}

func TestMultipleToolUses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.Copy(io.Discard, r.Body)
		lines := []string{
			"event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_tools","type":"message","role":"assistant","content":[],"model":"test","stop_reason":null,"usage":{"input_tokens":7,"output_tokens":1}}}`,
			"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-a","name":"ReadFile","input":{}}}`,
			"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`,
			"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}`,
			"event: content_block_start\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-b","name":"Bash","input":{}}}`,
			"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`,
			"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":1}`,
			"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
			"event: message_stop\n" + `data: {"type":"message_stop"}`,
		}
		_, _ = io.WriteString(w, strings.Join(lines, "\n\n")+"\n\n")
	}))
	defer srv.Close()

	client, err := New("provider", "model", "key", srv.URL+"/v1/messages", false, "", 8192, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("use tools")
	events, streamErr := drain(t, client, conv)
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	var starts, completes, ends int
	for _, event := range events {
		switch value := event.(type) {
		case llm.ToolCallStart:
			starts++
		case llm.ToolCallComplete:
			completes++
			if value.ToolID == "call-a" && value.Arguments["path"] != "a" {
				t.Fatalf("第一个工具参数 = %#v", value.Arguments)
			}
			if value.ToolID == "call-b" && value.Arguments["command"] != "pwd" {
				t.Fatalf("第二个工具参数 = %#v", value.Arguments)
			}
		case llm.StreamEnd:
			ends++
			if value.StopReason != "tool_use" || value.Usage.InputTokens != 7 || value.Usage.OutputTokens != 5 {
				t.Fatalf("终态 = %+v", value)
			}
		}
	}
	if starts != 2 || completes != 2 || ends != 1 {
		t.Fatalf("starts=%d completes=%d ends=%d events=%#v", starts, completes, ends, events)
	}
}

func TestThinkingStreamSignatureBeforeTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.Copy(io.Discard, r.Body)
		lines := []string{
			"event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_thinking_tool","type":"message","role":"assistant","content":[],"model":"test","stop_reason":null,"usage":{"input_tokens":2,"output_tokens":1}}}`,
			"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先分析"}}`,
			"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-part-1"}}`,
			"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"-part-2"}}`,
			"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}`,
			"event: content_block_start\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":1}`,
			"event: content_block_start\n" + `data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call-1","name":"ReadFile","input":{}}}`,
			"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\"}"}}`,
			"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":2}`,
			"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
			"event: message_stop\n" + `data: {"type":"message_stop"}`,
		}
		_, _ = io.WriteString(w, strings.Join(lines, "\n\n")+"\n\n")
	}))
	defer srv.Close()

	client, err := New("provider", "model", "key", srv.URL+"/v1/messages", false, "", 8192, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("use tool")
	events, streamErr := drain(t, client, conv)
	if streamErr != nil {
		t.Fatal(streamErr)
	}

	wantTypes := []string{"thinking_delta", "thinking_complete", "thinking_complete", "tool_start", "tool_delta", "tool_complete", "end"}
	gotTypes := make([]string, 0, len(events))
	for _, event := range events {
		switch value := event.(type) {
		case llm.ThinkingDelta:
			gotTypes = append(gotTypes, "thinking_delta")
		case llm.ThinkingComplete:
			gotTypes = append(gotTypes, "thinking_complete")
			if len(gotTypes) == 2 && (value.Thinking != "先分析" || value.Signature != "sig-part-1-part-2") {
				t.Fatalf("思考完成事件 = %+v", value)
			}
			if len(gotTypes) == 3 && (value.Thinking != "" || value.Signature != "") {
				t.Fatalf("空思考完成事件 = %+v", value)
			}
		case llm.ToolCallStart:
			gotTypes = append(gotTypes, "tool_start")
		case llm.ToolCallDelta:
			gotTypes = append(gotTypes, "tool_delta")
		case llm.ToolCallComplete:
			gotTypes = append(gotTypes, "tool_complete")
			if value.Arguments["path"] != "a.go" {
				t.Fatalf("工具参数 = %#v", value.Arguments)
			}
		case llm.StreamEnd:
			gotTypes = append(gotTypes, "end")
		}
	}
	if strings.Join(gotTypes, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("事件顺序 = %v，want %v", gotTypes, wantTypes)
	}
}

func TestEarlyEOFReturnsProtocolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"id":"msg_short","type":"message","role":"assistant","content":[],"model":"test","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
	}))
	defer srv.Close()

	client, err := New("provider", "model", "key", srv.URL+"/v1/messages", false, "", 8192, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("hello")
	events, streamErr := drain(t, client, conv)
	if len(events) != 0 {
		t.Fatalf("提前断流不应产生完成事件: %#v", events)
	}
	var modelErr *llm.Error
	if !errors.As(streamErr, &modelErr) || modelErr.Kind != llm.ErrorProtocol {
		t.Fatalf("提前断流错误 = %#v", streamErr)
	}
}

func TestStreamErrorBoundaries(t *testing.T) {
	const responseCanary = "malicious-response-secret"
	tests := []struct {
		name  string
		kind  llm.ErrorKind
		serve func(http.ResponseWriter)
	}{
		{
			name: "恶意认证响应",
			kind: llm.ErrorAuth,
			serve: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"`+responseCanary+`"}}`)
			},
		},
		{
			name: "损坏 JSON",
			kind: llm.ErrorProtocol,
			serve: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: {broken-json}\n\n")
			},
		},
		{
			name: "提前结束",
			kind: llm.ErrorProtocol,
			serve: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\n")
				_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"id":"msg_short","type":"message","role":"assistant","content":[],"model":"test","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tt.serve(w)
			}))
			defer srv.Close()
			client, err := New("provider", "model", "key", srv.URL+"/v1/messages", false, "", 8192, nil)
			if err != nil {
				t.Fatal(err)
			}
			conv := conversation.NewManager()
			conv.AddUserMessage("hello")
			events, streamErr := drain(t, client, conv)
			if len(events) != 0 {
				t.Fatalf("失败流不应产生终态事件: %#v", events)
			}
			var modelErr *llm.Error
			if !errors.As(streamErr, &modelErr) || modelErr.Kind != tt.kind {
				t.Fatalf("错误 = %#v，want kind %s", streamErr, tt.kind)
			}
			if strings.Contains(streamErr.Error(), responseCanary) || strings.Contains(streamErr.Error(), srv.URL) {
				t.Fatalf("错误泄漏响应或地址: %v", streamErr)
			}
		})
	}
}

func TestConsumerCancelStopsStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"id":"msg_cancel","type":"message","role":"assistant","content":[],"model":"test","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\n")
		_, _ = io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
		flusher.Flush()
		for i := 0; i < 256; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = io.WriteString(w, "event: content_block_delta\n")
			_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`+"\n\n")
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := New("provider", "model", "key", srv.URL+"/v1/messages", false, "", 8192, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("hello")
	ctx, cancel := context.WithCancel(context.Background())
	events, errs := client.Stream(ctx, conv, nil)
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("未收到首个事件")
	}
	cancel()
	select {
	case streamErr, ok := <-errs:
		if !ok {
			t.Fatal("取消后错误通道无错误")
		}
		var modelErr *llm.Error
		if !errors.As(streamErr, &modelErr) || modelErr.Kind != llm.ErrorCanceled {
			t.Fatalf("取消错误 = %#v", streamErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("消费者取消后流未及时退出")
	}
	for range events {
	}
	for range errs {
	}
}

func eventText(event llm.StreamEvent) string {
	if text, ok := event.(llm.TextDelta); ok {
		return text.Text
	}
	return ""
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
