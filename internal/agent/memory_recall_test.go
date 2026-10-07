package agent

import (
	"strings"
	"testing"

	"ccode/internal/conversation"
	"ccode/internal/llm"
	"ccode/internal/tools"
)

func recallCh(reminder string, paths ...string) <-chan RecallResult {
	ch := make(chan RecallResult, 1)
	ch <- RecallResult{Reminder: reminder, Paths: paths}
	return ch
}

// 有工具调用的一轮：召回结果在工具结果之后注入，同时记为已注入。
func TestMemoryRecallInjectedAfterTools(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{
		{
			llm.ToolCallStart{ToolName: "ReadFile", ToolID: "t1"},
			llm.ToolCallComplete{ToolID: "t1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": "/tmp/x"}},
			llm.StreamEnd{StopReason: "tool_use"},
		},
		{llm.TextDelta{Text: "done"}, llm.StreamEnd{StopReason: "end_turn"}},
	}}
	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "ReadFile", result: "ok"})
	ag := New(client, reg, "anthropic")
	ag.MemoryRecallCh = recallCh("## Memory: a.md", "/mem/a.md")
	conv := conversation.NewManager()
	runConversationRound(ag, conv, "read it")

	found := false
	for _, m := range conv.GetMessages() {
		if strings.Contains(m.Content, "## Memory: a.md") {
			found = true
		}
	}
	if !found {
		t.Fatal("recall reminder should be injected after tool results")
	}
	_, surfaced := ag.RecallHints()
	if _, ok := surfaced["/mem/a.md"]; !ok {
		t.Error("injected memory should be marked surfaced")
	}
}

// 没有工具调用的一轮：召回结果没被消费，对应记忆不能记为已注入，
// 下一轮召回时它们仍然要能参选。
func TestMemoryRecallNotSurfacedWithoutTools(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{{
		llm.TextDelta{Text: "plain answer"},
		llm.StreamEnd{StopReason: "end_turn"},
	}}}
	ag := New(client, tools.NewRegistry(), "anthropic")
	ag.MemoryRecallCh = recallCh("## Memory: a.md", "/mem/a.md")
	conv := conversation.NewManager()
	runConversationRound(ag, conv, "hi")

	for _, m := range conv.GetMessages() {
		if strings.Contains(m.Content, "## Memory: a.md") {
			t.Fatal("recall reminder must not be injected when no tool ran")
		}
	}
	_, surfaced := ag.RecallHints()
	if len(surfaced) != 0 {
		t.Errorf("unconsumed recall must not be marked surfaced, got %v", surfaced)
	}
}
