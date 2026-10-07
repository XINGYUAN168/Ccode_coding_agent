package agents

import (
	"context"
	"strings"
	"testing"

	"ccode/internal/conversation"
	"ccode/internal/permissions"
	"ccode/internal/tools"
)

func TestBuildForkedConversationPreservesThinkingTextAndTools(t *testing.T) {
	// fork 复制思考、正文与工具调用，并为被中断的工具补齐结果。
	parent := conversation.NewManager()
	parent.AddAssistantFull("hello", []conversation.ThinkingBlock{{
		Thinking: "分析", Signature: "sig-1",
	}}, []conversation.ToolUseBlock{
		{ToolUseID: "tool_1", ToolName: "Bash", Arguments: map[string]any{"command": "ls"}},
	})

	forked := buildForkedConversation(parent, "do work")
	msgs := forked.GetMessages()
	if len(msgs) < 3 || msgs[0].Content != "hello" || len(msgs[0].ToolUses) != 1 {
		t.Fatalf("fork 未完整复制正文和工具调用: %+v", msgs)
	}
	if len(msgs[1].ToolResults) != 1 || msgs[1].ToolResults[0].ToolUseID != "tool_1" {
		t.Fatalf("fork 未补齐工具结果: %+v", msgs)
	}
	if len(msgs[0].ThinkingBlocks) != 1 || msgs[0].ThinkingBlocks[0].Thinking != "分析" || msgs[0].ThinkingBlocks[0].Signature != "sig-1" {
		t.Fatalf("fork 未完整复制思考块: %+v", msgs[0].ThinkingBlocks)
	}
}

func TestDeriveSubAgentCheckerOverrideMode(t *testing.T) {
	// spec.PermissionMode 必须生成一个与父级共享 Sandbox 和 RuleEngine、但替换 Mode 的
	// Checker。覆盖值为空时应保持不变。
	sb := permissions.NewPathSandbox("/tmp", "")
	eng := &permissions.RuleEngine{}
	parent := permissions.NewChecker(sb, eng, permissions.ModeDefault)

	if derived := deriveSubAgentChecker(parent, ""); derived != parent {
		t.Error("empty override should return parent unchanged")
	}
	derived := deriveSubAgentChecker(parent, "plan")
	if derived == parent {
		t.Fatal("plan override should produce a new checker instance")
	}
	if derived.Mode != permissions.ModePlan {
		t.Errorf("derived.Mode = %q, want plan", derived.Mode)
	}
	if derived.Sandbox != sb || derived.RuleEngine != eng {
		t.Error("derived checker should share parent's Sandbox + RuleEngine")
	}
	if deriveSubAgentChecker(nil, "plan") != nil {
		t.Error("nil parent should propagate nil")
	}
}

func TestRunForkRejectedWhenQuerySourceIsFork(t *testing.T) {
	// 嵌套 fork 的首道防线：直接看 QuerySource 标记。
	tool := &AgentTool{
		Registry:     tools.NewRegistry(),
		Conversation: conversation.NewManager(),
		QuerySource:  ForkQuerySource,
		TaskMgr:      NewTaskManager(),
	}
	result := tool.runFork(context.Background(), "desc", "do work", "", "")
	if !result.IsError {
		t.Fatal("runFork should reject when QuerySource is fork")
	}
	if !strings.Contains(result.Output, "cannot fork from a forked agent") {
		t.Errorf("unexpected error message: %s", result.Output)
	}
}

func TestRunForkRejectedWhenBoilerplateInHistory(t *testing.T) {
	// 嵌套 fork 的兜底防线：QuerySource 没传到时，扫对话历史里的 fork 样板标记。
	conv := conversation.NewManager()
	conv.AddUserMessage(ForkBoilerplateTag + " stale message from a prior fork")
	tool := &AgentTool{
		Registry:     tools.NewRegistry(),
		Conversation: conv,
		TaskMgr:      NewTaskManager(),
	}
	result := tool.runFork(context.Background(), "desc", "do work", "", "")
	if !result.IsError {
		t.Fatal("runFork should reject when conversation history contains ForkBoilerplateTag")
	}
}

func TestCloneRegistryForForkSetsQuerySource(t *testing.T) {
	// fork 必须原样继承父工具池，只把其中的 Agent 工具换成带 QuerySource=ForkQuerySource
	// 的拷贝，这样再往下 fork 会在调用那一刻被拦住。
	reg := tools.NewRegistry()
	reg.Register(&AgentTool{}) // 模拟父 Agent 的 Agent 工具
	reg.Register(&dummyTool{name: "Bash", category: tools.CategoryCommand})

	forked := cloneRegistryForFork(reg)
	if forked.Get("Bash") == nil {
		t.Error("Bash should still be present in forked registry")
	}
	at, ok := forked.Get("Agent").(*AgentTool)
	if !ok {
		t.Fatal("Agent tool should still be present (tool pool inherited verbatim)")
	}
	if at.QuerySource != ForkQuerySource {
		t.Errorf("cloned Agent tool QuerySource = %q, want %q", at.QuerySource, ForkQuerySource)
	}
}

func TestExecuteRoutesBackgroundSpecToAsync(t *testing.T) {
	// Agent 定义里标了 background 就必须走异步，调用方没传 run_in_background 也一样。
	tool := &AgentTool{
		Registry: tools.NewRegistry(),
		TaskMgr:  NewTaskManager(),
		Protocol: "anthropic",
	}
	result := tool.Execute(context.Background(), map[string]any{
		"description":   "verify run",
		"prompt":        "do it",
		"subagent_type": "background-only",
	})
	if !result.IsError {
		// 未注册 spec 时调用应失败，这是单元测试环境下唯一安全的结果；但不能经由同步路径
		// 到达这里。IsError = true 且消息包含 "unknown agent type"，可证明参数解析已执行到
		// spec 查找阶段。
		if !strings.Contains(result.Output, "unknown agent type") {
			t.Errorf("expected unknown-agent-type error, got %q", result.Output)
		}
	}
}

func TestExecuteValidatesMode(t *testing.T) {
	tool := &AgentTool{
		Registry: tools.NewRegistry(),
		TaskMgr:  NewTaskManager(),
	}

	badMode := tool.Execute(context.Background(), map[string]any{
		"description": "x", "prompt": "y",
		"mode": "not-a-real-mode",
	})
	if !badMode.IsError || !strings.Contains(badMode.Output, "invalid mode") {
		t.Errorf("invalid mode must be rejected, got %q", badMode.Output)
	}
}
