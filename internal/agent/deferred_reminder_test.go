package agent

import (
	"context"
	"strings"
	"testing"

	"ccode/internal/conversation"
	"ccode/internal/llm"
	"ccode/internal/tools"
)

// deferredMockTool 是带延迟标记的占位工具，用来把 registry 推进「有延迟工具」的状态。
type deferredMockTool struct{ name string }

func (t *deferredMockTool) Name() string                 { return t.name }
func (t *deferredMockTool) Description() string          { return "deferred mock" }
func (t *deferredMockTool) Category() tools.ToolCategory { return tools.CategoryRead }
func (t *deferredMockTool) ShouldDefer() bool            { return true }
func (t *deferredMockTool) Schema() map[string]any {
	return map[string]any{"name": t.name, "input_schema": map[string]any{"type": "object"}}
}
func (t *deferredMockTool) Execute(context.Context, map[string]any) tools.ToolResult {
	return tools.ToolResult{Output: "ok"}
}

func countDeferredReminders(conv *conversation.Manager) int {
	n := 0
	for _, m := range conv.GetMessages() {
		if m.Role == "user" && strings.Contains(m.Content, deferredReminderMarker) {
			n++
		}
	}
	return n
}

// 延迟工具清单提醒的注入时机，跑的是真实主循环。
//
// 这条提醒是 append 进历史的，发一次就一直在上下文里，所以每轮重发只是拿相同内容
// 占窗口——六十来个 MCP 工具一份清单五百多 token，四十轮下来两万多。
//
// 该长什么样：一场多轮的工具调用里只出现一次。
func TestDeferredReminderNotRepeatedAcrossIterations(t *testing.T) {
	// 三轮工具调用 + 一轮收尾，主循环一共转四次
	toolTurn := func(id string) []llm.StreamEvent {
		return []llm.StreamEvent{
			llm.ToolCallStart{ToolName: "Glob", ToolID: id},
			llm.ToolCallComplete{ToolID: id, ToolName: "Glob", Arguments: map[string]any{"pattern": "*"}},
			llm.StreamEnd{StopReason: "tool_use"},
		}
	}
	client := &mockClient{responses: [][]llm.StreamEvent{
		toolTurn("t1"), toolTurn("t2"), toolTurn("t3"),
		{llm.TextDelta{Text: "done"}, llm.StreamEnd{StopReason: "end_turn"}},
	}}

	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "Glob", result: "ok"})
	reg.Register(&deferredMockTool{name: "mcp__linear__create_issue"})
	reg.Register(&deferredMockTool{name: "mcp__sentry__resolve_issue"})

	ag := New(client, reg, "anthropic")
	conv := conversation.NewManager()
	runConversationRound(ag, conv, "do three things")

	if client.callIdx != 4 {
		t.Fatalf("主循环该转 4 次，实际 %d", client.callIdx)
	}
	if got := countDeferredReminders(conv); got != 1 {
		t.Errorf("四轮下来清单该只注入 1 次，实际 %d 次", got)
	}
}

// 工具池变了要补一次：MCP 是异步连上的，第二个回合才出现的服务器得让模型知道。
func TestDeferredReminderReannouncedWhenPoolChanges(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{
		{llm.TextDelta{Text: "one"}, llm.StreamEnd{StopReason: "end_turn"}},
		{llm.TextDelta{Text: "two"}, llm.StreamEnd{StopReason: "end_turn"}},
	}}
	reg := tools.NewRegistry()
	reg.Register(&deferredMockTool{name: "mcp__linear__create_issue"})

	ag := New(client, reg, "anthropic")
	conv := conversation.NewManager()

	runConversationRound(ag, conv, "第一个回合")
	if got := countDeferredReminders(conv); got != 1 {
		t.Fatalf("首个回合该注入 1 次，实际 %d", got)
	}

	// MCP 服务器姗姗来迟，池子多出一个工具
	reg.Register(&deferredMockTool{name: "mcp__infra__scale_service"})
	runConversationRound(ag, conv, "第二个回合")
	if got := countDeferredReminders(conv); got != 2 {
		t.Errorf("池子变化后该补 1 次，实际总数 %d", got)
	}
}

// compact 把历史压成摘要之后，原来那条提醒也没了，得重新宣告。
// 靠回扫历史发现，所以不用在 compact 那边额外挂钩子。
func TestDeferredReminderReannouncedAfterHistoryWiped(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{
		{llm.TextDelta{Text: "one"}, llm.StreamEnd{StopReason: "end_turn"}},
		{llm.TextDelta{Text: "two"}, llm.StreamEnd{StopReason: "end_turn"}},
	}}
	reg := tools.NewRegistry()
	reg.Register(&deferredMockTool{name: "mcp__linear__create_issue"})

	ag := New(client, reg, "anthropic")
	conv := conversation.NewManager()

	runConversationRound(ag, conv, "第一个回合")
	if got := countDeferredReminders(conv); got != 1 {
		t.Fatalf("首个回合该注入 1 次，实际 %d", got)
	}

	// 模拟 compact：历史被压成一条摘要，那条提醒随之消失
	conv.TruncateTo(0)
	conv.AddUserMessage("summary of earlier conversation")

	runConversationRound(ag, conv, "第二个回合")
	if got := countDeferredReminders(conv); got != 1 {
		t.Errorf("历史被压掉后该重新宣告 1 次，实际 %d", got)
	}
}

// 排序必须稳定，否则调用方没法靠比较判断池子变没变。tools 是 map，不排就是随机序。
func TestDeferredToolNamesSorted(t *testing.T) {
	reg := tools.NewRegistry()
	for _, n := range []string{"mcp__z__b", "mcp__a__c", "mcp__m__a"} {
		reg.Register(&deferredMockTool{name: n})
	}
	first := reg.GetDeferredToolNames()
	want := []string{"mcp__a__c", "mcp__m__a", "mcp__z__b"}
	if strings.Join(first, ",") != strings.Join(want, ",") {
		t.Fatalf("该按字典序，得到 %v", first)
	}
	// 连续多次调用顺序不能变
	for i := 0; i < 20; i++ {
		if strings.Join(reg.GetDeferredToolNames(), ",") != strings.Join(want, ",") {
			t.Fatalf("第 %d 次调用顺序变了", i)
		}
	}
}

// eager 模式下没有延迟工具，这条提醒完全不该出现。
func TestNoDeferredReminderWithoutDeferredTools(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{
		{llm.TextDelta{Text: "hi"}, llm.StreamEnd{StopReason: "end_turn"}},
	}}
	ag := New(client, tools.NewRegistry(), "anthropic")
	conv := conversation.NewManager()
	runConversationRound(ag, conv, "hi")
	if got := countDeferredReminders(conv); got != 0 {
		t.Errorf("没有延迟工具时不该注入，实际 %d 次", got)
	}
}
