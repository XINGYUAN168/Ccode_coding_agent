package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccode/internal/conversation"
	"ccode/internal/llm"
)

func TestResponsesThinkingAndToolStream(t *testing.T) {
	var body []byte
	var requestURI, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.URL.RequestURI()
		auth = r.Header.Get("Authorization")
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
			`data: {"type":"response.reasoning_summary_text.delta","sequence_number":2,"output_index":0,"item_id":"rs_1","summary_index":0,"delta":"分析"}`,
			`data: {"type":"response.reasoning_summary_text.done","sequence_number":3,"output_index":0,"item_id":"rs_1","summary_index":0,"text":"分析"}`,
			`data: {"type":"response.output_text.delta","sequence_number":4,"output_index":1,"content_index":0,"item_id":"msg_1","delta":"处理中"}`,
			`data: {"type":"response.output_item.added","sequence_number":5,"output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"ReadFile","arguments":""}}`,
			`data: {"type":"response.function_call_arguments.delta","sequence_number":6,"output_index":2,"item_id":"fc_1","delta":"{\"path\":\"a.go\"}"}`,
			`data: {"type":"response.function_call_arguments.done","sequence_number":7,"output_index":2,"item_id":"fc_1","arguments":"{\"path\":\"a.go\"}"}`,
			`data: {"type":"response.completed","sequence_number":8,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-5","output":[],"parallel_tool_calls":true,"tool_choice":"auto","tools":[],"input":[],"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":15}}}`,
		}
		_, _ = io.WriteString(w, strings.Join(lines, "\n\n")+"\n\n")
	}))
	defer srv.Close()

	client, err := NewResponses("openai", "gpt-5", "explicit-key", srv.URL+"/v1/responses?q=1", true, "system", 12345, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("hello")
	events, errs := client.Stream(context.Background(), conv, []map[string]any{{
		"type": "function", "name": "ReadFile", "description": "read", "parameters": map[string]any{"type": "object"},
	}})
	var text, thinking, signature, stop string
	var tool llm.ToolCallComplete
	var usage llm.UsageInfo
	for event := range events {
		switch value := event.(type) {
		case llm.TextDelta:
			text += value.Text
		case llm.ThinkingDelta:
			thinking += value.Text
		case llm.ThinkingComplete:
			signature = value.Signature
		case llm.ToolCallComplete:
			tool = value
		case llm.StreamEnd:
			stop, usage = value.StopReason, value.Usage
		}
	}
	for streamErr := range errs {
		if streamErr != nil {
			t.Fatal(streamErr)
		}
	}

	if requestURI != "/v1/responses?q=1" || auth != "Bearer explicit-key" {
		t.Fatalf("uri=%q auth=%q", requestURI, auth)
	}
	if text != "处理中" || thinking != "分析" || signature != "rs_1" {
		t.Fatalf("text=%q thinking=%q signature=%q", text, thinking, signature)
	}
	if tool.ToolID != "call_1" || tool.ToolName != "ReadFile" || tool.Arguments["path"] != "a.go" {
		t.Fatalf("tool=%+v", tool)
	}
	if stop != "tool_use" || usage.InputTokens != 10 || usage.OutputTokens != 3 || usage.CacheReadTokens != 2 || usage.CacheCreationTokens != 1 {
		t.Fatalf("stop=%q usage=%+v", stop, usage)
	}

	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	reasoning, ok := request["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" || reasoning["summary"] != "detailed" {
		t.Fatalf("reasoning=%#v request=%s", request["reasoning"], body)
	}
	if request["max_output_tokens"] != float64(12345) {
		t.Fatalf("max_output_tokens=%v", request["max_output_tokens"])
	}
	include, _ := request["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include=%#v", include)
	}
}

func TestResponsesThinkingDisabled(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[],"parallel_tool_calls":true,"tool_choice":"auto","tools":[],"input":[],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}}`+"\n\n")
	}))
	defer srv.Close()

	client, err := NewResponses("openai", "gpt-4.1", "key", srv.URL, false, "", 8192, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage("hello")
	events, errs := client.Stream(context.Background(), conv, nil)
	for range events {
	}
	for streamErr := range errs {
		if streamErr != nil {
			t.Fatal(streamErr)
		}
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if _, ok := request["reasoning"]; ok {
		t.Fatalf("thinking=false 时不应发送 reasoning: %s", body)
	}
	if _, ok := request["include"]; ok {
		t.Fatalf("thinking=false 时不应发送 include: %s", body)
	}
}

func TestBuildResponsesInputKeepsReasoningAndToolPairs(t *testing.T) {
	history := []conversation.Message{
		{Role: "user", Content: "开始"},
		{Role: "assistant", Content: "读取", ThinkingBlocks: []conversation.ThinkingBlock{{Thinking: "分析", Signature: "rs_1"}}, ToolUses: []conversation.ToolUseBlock{{ToolUseID: "call_1", ToolName: "ReadFile", Arguments: map[string]any{"path": "a.go"}}}},
		{Role: "user", ToolResults: []conversation.ToolResultBlock{{ToolUseID: "call_1", Content: "ok"}}},
	}
	encoded, err := json.Marshal(buildResponsesInput(history))
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{`"type":"reasoning"`, `"id":"rs_1"`, `"type":"function_call"`, `"call_id":"call_1"`, `"type":"function_call_output"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("Responses 历史缺少 %s: %s", want, text)
		}
	}
}
