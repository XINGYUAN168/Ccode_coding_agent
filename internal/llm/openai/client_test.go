package openai

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

func writeOpenAITextStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"secret-reasoning"},"finish_reason":null}]}`+"\n\n")
	_, _ = io.WriteString(w, `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`+"\n\n")
	_, _ = io.WriteString(w, `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	_, _ = io.WriteString(w, `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":2}}}`+"\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func TestConstructionDoesNotUseNetwork(t *testing.T) {
	var calls atomic.Int32
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, context.Canceled
	})}
	if _, err := New("provider", "model", "key", "", true, "system", httpClient); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("构造阶段发生 %d 次网络请求", calls.Load())
	}
}

func TestReasoningContentStreamDoesNotChangeRequest(t *testing.T) {
	for _, thinking := range []bool{false, true} {
		t.Run(map[bool]string{false: "false", true: "true"}[thinking], func(t *testing.T) {
			var body []byte
			var auth, requestURI string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				requestURI = r.URL.RequestURI()
				body, _ = io.ReadAll(r.Body)
				writeOpenAITextStream(w)
			}))
			defer srv.Close()
			client, err := New("provider", "model", "explicit-key", srv.URL+"/custom/chat/completions?q=1", thinking, "system", nil)
			if err != nil {
				t.Fatal(err)
			}
			conv := conversation.NewManager()
			conv.AddUserMessage("hello")
			events, errs := client.Stream(context.Background(), conv, nil)
			var text, reasoning, completed string
			var end *llm.StreamEnd
			for event := range events {
				switch value := event.(type) {
				case llm.TextDelta:
					text += value.Text
				case llm.ThinkingDelta:
					reasoning += value.Text
				case llm.ThinkingComplete:
					completed = value.Thinking
				case llm.StreamEnd:
					copy := value
					end = &copy
				}
			}
			for streamErr := range errs {
				if streamErr != nil {
					t.Fatal(streamErr)
				}
			}
			if auth != "Bearer explicit-key" || requestURI != "/custom/chat/completions?q=1" {
				t.Fatalf("auth=%q uri=%q", auth, requestURI)
			}
			var request map[string]any
			_ = json.Unmarshal(body, &request)
			encoded := string(body)
			if strings.Contains(encoded, "reasoning") || strings.Contains(encoded, "thinking") {
				t.Fatalf("请求包含推理字段: %s", encoded)
			}
			if text != "hello" || reasoning != "secret-reasoning" || completed != reasoning || end == nil {
				t.Fatalf("text=%q reasoning=%q completed=%q end=%+v request=%+v", text, reasoning, completed, end, request)
			}
			if end.Usage.InputTokens != 10 || end.Usage.CacheReadTokens != 2 || end.Usage.OutputTokens != 3 {
				t.Fatalf("用量 = %+v", end.Usage)
			}
		})
	}
}

func TestReasoningFallbackIgnoresInvalidExtensions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"reasoning_content":{"bad":true}},"finish_reason":null}]}`,
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"reasoning_content":42},"finish_reason":null}]}`,
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"reasoning_content":"","content":"ok"},"finish_reason":null}]}`,
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		_, _ = io.WriteString(w, strings.Join(lines, "\n\n")+"\n\n")
	}))
	defer srv.Close()

	client, err := New("provider", "model", "key", srv.URL+"/v1/chat/completions", true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("hello")
	events, errs := client.Stream(context.Background(), conv, nil)
	var text string
	var thinkingEvents int
	for event := range events {
		switch value := event.(type) {
		case llm.TextDelta:
			text += value.Text
		case llm.ThinkingDelta, llm.ThinkingComplete:
			thinkingEvents++
		}
	}
	for streamErr := range errs {
		if streamErr != nil {
			t.Fatal(streamErr)
		}
	}
	if text != "ok" || thinkingEvents != 0 {
		t.Fatalf("text=%q thinkingEvents=%d", text, thinkingEvents)
	}
}

func TestReasoningContentHistory(t *testing.T) {
	history := []conversation.Message{
		{Role: "user", Content: "开始"},
		{Role: "assistant", Content: "纯正文"},
		{Role: "user", Content: "继续"},
		{Role: "assistant", ThinkingBlocks: []conversation.ThinkingBlock{{Thinking: "第一段"}, {Thinking: "第二段"}}},
		{
			Role:           "assistant",
			Content:        "读取文件",
			ThinkingBlocks: []conversation.ThinkingBlock{{Thinking: "检查路径"}},
			ToolUses: []conversation.ToolUseBlock{{
				ToolUseID: "call-1",
				ToolName:  "ReadFile",
				Arguments: map[string]any{"path": "a.go"},
			}},
		},
		{Role: "user", ToolResults: []conversation.ToolResultBlock{{ToolUseID: "call-1", Content: "ok"}}},
	}

	encoded, err := json.Marshal(buildMessages("系统", history))
	if err != nil {
		t.Fatal(err)
	}
	var messages []map[string]any
	if err := json.Unmarshal(encoded, &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 7 {
		t.Fatalf("消息数 = %d，want 7：%s", len(messages), encoded)
	}
	if _, exists := messages[2]["reasoning_content"]; exists {
		t.Fatalf("纯正文消息不应包含思考字段: %#v", messages[2])
	}
	if got := messages[4]["reasoning_content"]; got != "第一段第二段" {
		t.Fatalf("仅思考消息 = %#v", messages[4])
	}
	if got := messages[5]["reasoning_content"]; got != "检查路径" {
		t.Fatalf("思考工具消息 = %#v", messages[5])
	}
	toolCalls, ok := messages[5]["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("工具调用历史 = %#v", messages[5]["tool_calls"])
	}
}

func TestParallelToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.Copy(io.Discard, r.Body)
		lines := []string{
			`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"ReadFile","arguments":"{\"path\":"}},{"index":1,"id":"b","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
			`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}},{"index":1,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}
		_, _ = io.WriteString(w, strings.Join(lines, "\n\n")+"\n\n")
	}))
	defer srv.Close()
	client, err := New("p", "m", "key", srv.URL+"/v1/chat/completions", false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("use tools")
	events, errs := client.Stream(context.Background(), conv, nil)
	var complete, ends int
	var stop string
	var usage llm.UsageInfo
	for event := range events {
		switch value := event.(type) {
		case llm.ToolCallComplete:
			complete++
		case llm.StreamEnd:
			ends++
			stop = value.StopReason
			usage = value.Usage
		}
	}
	for streamErr := range errs {
		if streamErr != nil {
			t.Fatal(streamErr)
		}
	}
	if complete != 2 || ends != 1 || stop != "tool_use" || usage != (llm.UsageInfo{}) {
		t.Fatalf("complete=%d ends=%d stop=%q usage=%+v", complete, ends, stop, usage)
	}
}

func TestProviderContract(t *testing.T) {
	tests := []struct {
		name  string
		model string
		path  string
	}{
		{name: "openai", model: "gpt-4.1", path: "/v1/chat/completions"},
		{name: "gemini", model: "gemini-2.5-pro", path: "/v1beta/openai/chat/completions"},
		{name: "kimi", model: "kimi-k2.5", path: "/v1/chat/completions"},
		{name: "glm", model: "glm-4.5", path: "/api/paas/v4/chat/completions"},
		{name: "deepseek", model: "deepseek-chat", path: "/v1/chat/completions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if r.URL.RequestURI() != tt.path || r.Header.Get("Authorization") != "Bearer explicit-key" {
					t.Errorf("请求地址或认证错误: uri=%q auth=%q", r.URL.RequestURI(), r.Header.Get("Authorization"))
				}
				body, _ := io.ReadAll(r.Body)
				var request map[string]any
				if err := json.Unmarshal(body, &request); err != nil {
					t.Errorf("请求不是合法 JSON: %v", err)
				}
				if request["model"] != tt.model {
					t.Errorf("model=%#v，want %q", request["model"], tt.model)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if call == 1 {
					lines := []string{
						`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"reasoning_content":"分析","content":"处理中"},"finish_reason":null}]}`,
						`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"ReadFile","arguments":"{\"path\":\"a.go\"}"}},{"index":1,"id":"b","type":"function","function":{"name":"ReadFile","arguments":"{\"path\":\"b.go\"}"}}]},"finish_reason":null}]}`,
						`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
						`data: [DONE]`,
					}
					_, _ = io.WriteString(w, strings.Join(lines, "\n\n")+"\n\n")
					return
				}

				messages, _ := request["messages"].([]any)
				var toolResults int
				var replayedReasoning bool
				for _, raw := range messages {
					message, _ := raw.(map[string]any)
					if message["role"] == "tool" {
						toolResults++
					}
					if message["role"] == "assistant" && message["reasoning_content"] == "分析" {
						replayedReasoning = true
					}
				}
				if toolResults != 2 || !replayedReasoning {
					t.Errorf("工具结果或思考历史未回灌: %s", body)
				}
				lines := []string{
					`data: {"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"完成"},"finish_reason":null}]}`,
					`data: {"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
					`data: {"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
					`data: [DONE]`,
				}
				_, _ = io.WriteString(w, strings.Join(lines, "\n\n")+"\n\n")
			}))
			defer srv.Close()

			client, err := New(tt.name, tt.model, "explicit-key", srv.URL+tt.path, true, "system", nil)
			if err != nil {
				t.Fatal(err)
			}
			conv := conversation.NewManager()
			conv.AddUserMessage("读取两个文件")
			firstEvents, firstErrs := client.Stream(context.Background(), conv, nil)
			var text, reasoning string
			var tools []conversation.ToolUseBlock
			var firstEnd *llm.StreamEnd
			for event := range firstEvents {
				switch value := event.(type) {
				case llm.TextDelta:
					text += value.Text
				case llm.ThinkingDelta:
					reasoning += value.Text
				case llm.ToolCallComplete:
					tools = append(tools, conversation.ToolUseBlock{ToolUseID: value.ToolID, ToolName: value.ToolName, Arguments: value.Arguments})
				case llm.StreamEnd:
					copy := value
					firstEnd = &copy
				}
			}
			for streamErr := range firstErrs {
				if streamErr != nil {
					t.Fatal(streamErr)
				}
			}
			if text != "处理中" || reasoning != "分析" || len(tools) != 2 || firstEnd == nil || firstEnd.StopReason != "tool_use" || firstEnd.Usage != (llm.UsageInfo{}) {
				t.Fatalf("首轮事件不完整: text=%q reasoning=%q tools=%#v end=%+v", text, reasoning, tools, firstEnd)
			}
			conv.AddAssistantFull(text, []conversation.ThinkingBlock{{Thinking: reasoning}}, tools)
			conv.AddToolResultsMessage([]conversation.ToolResultBlock{{ToolUseID: "a", Content: "A"}, {ToolUseID: "b", Content: "B"}})
			secondEvents, secondErrs := client.Stream(context.Background(), conv, nil)
			var finalText string
			var finalUsage llm.UsageInfo
			for event := range secondEvents {
				switch value := event.(type) {
				case llm.TextDelta:
					finalText += value.Text
				case llm.StreamEnd:
					finalUsage = value.Usage
				}
			}
			for streamErr := range secondErrs {
				if streamErr != nil {
					t.Fatal(streamErr)
				}
			}
			if calls.Load() != 2 || finalText != "完成" || finalUsage.InputTokens != 10 || finalUsage.OutputTokens != 2 {
				t.Fatalf("续接结果错误: calls=%d text=%q usage=%+v", calls.Load(), finalText, finalUsage)
			}
		})
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
				_, _ = io.WriteString(w, `{"error":{"message":"`+responseCanary+`","type":"invalid_request_error"}}`)
			},
		},
		{
			name: "损坏 JSON",
			kind: llm.ErrorProtocol,
			serve: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {broken-json}\n\n")
			},
		},
		{
			name: "提前结束",
			kind: llm.ErrorProtocol,
			serve: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"id":"short","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tt.serve(w)
			}))
			defer srv.Close()
			client, err := New("provider", "model", "key", srv.URL+"/v1/chat/completions", false, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			conv := conversation.NewManager()
			conv.AddUserMessage("hello")
			events, errs := client.Stream(context.Background(), conv, nil)
			var ends int
			for event := range events {
				if _, ok := event.(llm.StreamEnd); ok {
					ends++
				}
			}
			var streamErr error
			var errorCount int
			for err := range errs {
				if err != nil {
					streamErr = err
					errorCount++
				}
			}
			if ends != 0 || errorCount != 1 {
				t.Fatalf("ends=%d errors=%d", ends, errorCount)
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
		for i := 0; i < 256; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = io.WriteString(w, `data: {"id":"cancel","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`+"\n\n")
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := New("provider", "model", "key", srv.URL+"/v1/chat/completions", false, "", nil)
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
