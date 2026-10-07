package conversation

import "testing"

func TestAssistantFullPreservesThinkingAndTools(t *testing.T) {
	manager := NewManager()
	manager.AddAssistantFull("done", []ThinkingBlock{{
		Thinking: "分析过程", Signature: "signed-thinking",
	}}, []ToolUseBlock{{
		ToolUseID: "call-1", ToolName: "ReadFile", Arguments: map[string]any{"path": "a.go"},
	}})
	messages := manager.GetMessages()
	if len(messages) != 1 || messages[0].Role != "assistant" || messages[0].Content != "done" {
		t.Fatalf("助手历史不正确: %+v", messages)
	}
	if len(messages[0].ToolUses) != 1 || messages[0].ToolUses[0].ToolUseID != "call-1" {
		t.Fatalf("工具调用未保存: %+v", messages[0].ToolUses)
	}
	if len(messages[0].ThinkingBlocks) != 1 || messages[0].ThinkingBlocks[0].Thinking != "分析过程" || messages[0].ThinkingBlocks[0].Signature != "signed-thinking" {
		t.Fatalf("思考块未保存: %+v", messages[0].ThinkingBlocks)
	}
}

func TestGetMessagesReturnsDeepCopy(t *testing.T) {
	manager := NewManager()
	manager.AddAssistantFull("done", []ThinkingBlock{{
		Thinking: "原始思考", Signature: "原始签名",
	}}, []ToolUseBlock{{
		ToolUseID: "call-1",
		ToolName:  "ReadFile",
		Arguments: map[string]any{"options": map[string]any{"path": "a.go"}},
	}})
	manager.AddToolResultsMessage([]ToolResultBlock{{
		ToolUseID: "call-1",
		Content:   "ok",
		ContentBlocks: []map[string]any{{
			"type": "tool_reference",
			"meta": map[string]any{"name": "ReadFile"},
		}},
	}})

	copyOfHistory := manager.GetMessages()
	copyOfHistory[0].ThinkingBlocks[0].Thinking = "已修改"
	copyOfHistory[0].ToolUses[0].Arguments["options"].(map[string]any)["path"] = "b.go"
	copyOfHistory[1].ToolResults[0].ContentBlocks[0]["meta"].(map[string]any)["name"] = "WriteFile"

	original := manager.GetMessages()
	if got := original[0].ThinkingBlocks[0].Thinking; got != "原始思考" {
		t.Fatalf("思考块被外部修改: %q", got)
	}
	if got := original[0].ToolUses[0].Arguments["options"].(map[string]any)["path"]; got != "a.go" {
		t.Fatalf("工具参数被外部修改: %v", got)
	}
	if got := original[1].ToolResults[0].ContentBlocks[0]["meta"].(map[string]any)["name"]; got != "ReadFile" {
		t.Fatalf("结构化工具结果被外部修改: %v", got)
	}
}
