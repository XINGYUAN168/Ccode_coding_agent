package anthropic

import (
	"testing"

	"ccode/internal/conversation"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

func TestCacheMarkerSkipsDeferredTools(t *testing.T) {
	plain := func(name string) anthropicsdk.ToolUnionParam {
		return anthropicsdk.ToolUnionParam{OfTool: &anthropicsdk.ToolParam{Name: name}}
	}
	deferred := func(name string) anthropicsdk.ToolUnionParam {
		return anthropicsdk.ToolUnionParam{OfTool: &anthropicsdk.ToolParam{Name: name, DeferLoading: param.NewOpt(true)}}
	}
	tools := []anthropicsdk.ToolUnionParam{plain("ReadFile"), plain("ToolSearch"), deferred("remote")}
	markToolsForCache(tools)
	var zero anthropicsdk.CacheControlEphemeralParam
	if tools[1].OfTool.CacheControl == zero || tools[2].OfTool.CacheControl != zero {
		t.Fatal("缓存断点必须落在最后一个非延迟工具上")
	}
}

func TestDeferredToolControlsBeta(t *testing.T) {
	if needsToolSearchBeta(nil) {
		t.Fatal("没有延迟工具时不应发送 beta")
	}
	if !needsToolSearchBeta([]map[string]any{{"name": "remote", "defer_loading": true}}) {
		t.Fatal("存在延迟工具时应发送 beta")
	}
}

func TestThinkingHistoryBeforeTextAndTools(t *testing.T) {
	messages := buildAnthropicMessages([]conversation.Message{{
		Role:    "assistant",
		Content: "完成",
		ThinkingBlocks: []conversation.ThinkingBlock{
			{Thinking: "第一段", Signature: "sig-1"},
			{Thinking: "第二段", Signature: "sig-2"},
		},
		ToolUses: []conversation.ToolUseBlock{
			{ToolUseID: "call-1", ToolName: "ReadFile", Arguments: map[string]any{"path": "a.go"}},
			{ToolUseID: "call-2", ToolName: "ReadFile", Arguments: map[string]any{"path": "b.go"}},
		},
	}})
	if len(messages) != 1 {
		t.Fatalf("消息数 = %d，want 1", len(messages))
	}
	blocks := messages[0].Content
	if len(blocks) != 5 {
		t.Fatalf("内容块数量 = %d，want 5", len(blocks))
	}
	if blocks[0].OfThinking == nil || blocks[0].OfThinking.Thinking != "第一段" || blocks[0].OfThinking.Signature != "sig-1" {
		t.Fatalf("第一个思考块 = %#v", blocks[0].OfThinking)
	}
	if blocks[1].OfThinking == nil || blocks[1].OfThinking.Thinking != "第二段" || blocks[1].OfThinking.Signature != "sig-2" {
		t.Fatalf("第二个思考块 = %#v", blocks[1].OfThinking)
	}
	if blocks[2].OfText == nil || blocks[2].OfText.Text != "完成" {
		t.Fatalf("正文块 = %#v", blocks[2].OfText)
	}
	if blocks[3].OfToolUse == nil || blocks[3].OfToolUse.ID != "call-1" || blocks[4].OfToolUse == nil || blocks[4].OfToolUse.ID != "call-2" {
		t.Fatalf("工具块顺序不正确: %#v", blocks)
	}
}
