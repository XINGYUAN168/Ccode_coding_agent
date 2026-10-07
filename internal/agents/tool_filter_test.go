package agents

import (
	"context"
	"testing"

	"ccode/internal/tools"
)

type dummyTool struct {
	name     string
	category tools.ToolCategory
}

func (d *dummyTool) Name() string                 { return d.name }
func (d *dummyTool) Description() string          { return "test tool" }
func (d *dummyTool) Category() tools.ToolCategory { return d.category }

func (d *dummyTool) Schema() map[string]any { return nil }
func (d *dummyTool) Execute(_ context.Context, _ map[string]any) tools.ToolResult {
	return tools.ToolResult{}
}

func makeRegistry(names ...string) *tools.Registry {
	reg := tools.NewRegistry()
	for _, n := range names {
		reg.Register(&dummyTool{name: n, category: tools.CategoryRead})
	}
	return reg
}

func hasToolNamed(reg *tools.Registry, name string) bool {
	return reg.Get(name) != nil
}

func TestFilterRemovesAgentTool(t *testing.T) {
	reg := makeRegistry("ReadFile", "Agent", "Bash")
	filtered := FilterToolsForAgent(reg, nil, nil, false)
	if hasToolNamed(filtered, "Agent") {
		t.Error("Agent tool should be removed from sub-agent registry")
	}
	if !hasToolNamed(filtered, "ReadFile") {
		t.Error("ReadFile should remain")
	}
	if !hasToolNamed(filtered, "Bash") {
		t.Error("Bash should remain")
	}
}

func TestFilterRemovesAskUserQuestion(t *testing.T) {
	reg := makeRegistry("ReadFile", "AskUserQuestion")
	filtered := FilterToolsForAgent(reg, nil, nil, false)
	if hasToolNamed(filtered, "AskUserQuestion") {
		t.Error("AskUserQuestion should be removed from sub-agent registry")
	}
}

func TestAsyncFilterWhitelist(t *testing.T) {
	reg := makeRegistry("ReadFile", "WriteFile", "EditFile", "Glob", "Grep", "Bash", "ToolSearch", "Agent", "AskUserQuestion", "TaskCreate", "TaskList")
	filtered := FilterToolsForAgent(reg, nil, nil, true)

	allowed := []string{"ReadFile", "WriteFile", "EditFile", "Glob", "Grep", "Bash", "ToolSearch"}
	for _, name := range allowed {
		if !hasToolNamed(filtered, name) {
			t.Errorf("%s should be allowed for async agents", name)
		}
	}

	blocked := []string{"Agent", "AskUserQuestion", "TaskCreate", "TaskList"}
	for _, name := range blocked {
		if hasToolNamed(filtered, name) {
			t.Errorf("%s should be blocked for async agents", name)
		}
	}
}

func TestMCPToolsPassThrough(t *testing.T) {
	reg := makeRegistry("mcp__grafana__query", "Agent", "ReadFile")
	filtered := FilterToolsForAgent(reg, nil, nil, true)
	if !hasToolNamed(filtered, "mcp__grafana__query") {
		t.Error("MCP tools should always pass through filter")
	}
}

func TestGlobalDisallowedExpanded(t *testing.T) {
	// 无论定义中的允许清单如何设置，以下工具都必须对所有子 Agent 禁用。
	reg := makeRegistry(
		"ReadFile",
		"TaskOutput",
		"ExitPlanMode",
		"EnterPlanMode",
		"Agent",
		"AskUserQuestion",
		"TaskStop",
		"Workflow",
	)
	filtered := FilterToolsForAgent(reg, []string{"*"}, nil, false)
	for _, blocked := range []string{
		"TaskOutput", "ExitPlanMode", "EnterPlanMode", "Agent", "AskUserQuestion", "TaskStop", "Workflow",
	} {
		if hasToolNamed(filtered, blocked) {
			t.Errorf("%s should be in ALL_AGENT_DISALLOWED_TOOLS", blocked)
		}
	}
	if !hasToolNamed(filtered, "ReadFile") {
		t.Error("ReadFile should remain")
	}
}

func TestAsyncWhitelistExpanded(t *testing.T) {
	// 异步 Agent 只能使用这组白名单工具。
	reg := makeRegistry(
		"ReadFile", "WebSearch", "TodoWrite", "Grep", "WebFetch", "Glob",
		"Bash", "EditFile", "WriteFile", "NotebookEdit", "Skill",
		"SyntheticOutput", "ToolSearch", "EnterWorktree", "ExitWorktree",
	)
	filtered := FilterToolsForAgent(reg, nil, nil, true)
	for _, name := range []string{
		"ReadFile", "WebSearch", "TodoWrite", "Grep", "WebFetch", "Glob",
		"Bash", "EditFile", "WriteFile", "NotebookEdit", "Skill",
		"SyntheticOutput", "ToolSearch", "EnterWorktree", "ExitWorktree",
	} {
		if !hasToolNamed(filtered, name) {
			t.Errorf("%s should be allowed for async agents", name)
		}
	}
}

func TestInProcessTeammateExtraTools(t *testing.T) {
	// 通常会被异步白名单阻止的协作工具，在子 Agent 是进程内队员时必须允许。
	reg := makeRegistry("ReadFile", "TaskCreate", "TaskList", "SendMessage", "Agent")
	asTeammate := FilterToolsForAgentEx(reg, nil, nil, true, false, true)
	for _, name := range []string{"TaskCreate", "TaskList", "SendMessage"} {
		if !hasToolNamed(asTeammate, name) {
			t.Errorf("%s should be allowed for in-process teammates", name)
		}
	}
	// 进程内队员原则上可使用 Agent 工具生成同步子 Agent，但全局
	// ALL_AGENT_DISALLOWED_TOOLS 门禁先于队员检查执行，因此它目前仍会被阻止。
	// 此测试用于记录这一现状。
	notTeammate := FilterToolsForAgentEx(reg, nil, nil, true, false, false)
	for _, name := range []string{"TaskCreate", "TaskList", "SendMessage"} {
		if hasToolNamed(notTeammate, name) {
			t.Errorf("%s should be blocked for plain async agents (not teammates)", name)
		}
	}
}

func TestDisallowedToolsApplied(t *testing.T) {
	reg := makeRegistry("ReadFile", "EditFile", "WriteFile", "Bash")
	filtered := FilterToolsForAgent(reg, nil, []string{"EditFile", "WriteFile"}, false)
	if hasToolNamed(filtered, "EditFile") {
		t.Error("EditFile should be blocked by disallowedTools")
	}
	if hasToolNamed(filtered, "WriteFile") {
		t.Error("WriteFile should be blocked by disallowedTools")
	}
	if !hasToolNamed(filtered, "ReadFile") {
		t.Error("ReadFile should remain")
	}
	if !hasToolNamed(filtered, "Bash") {
		t.Error("Bash should remain")
	}
}

func TestGeneralPurposeNoRecursion(t *testing.T) {
	reg := makeRegistry("ReadFile", "Agent", "Bash", "EditFile", "WriteFile", "Glob", "Grep", "ToolSearch", "AskUserQuestion")
	spec := BuiltinSpecs["general-purpose"]
	filtered := FilterToolsForAgent(reg, spec.Tools, spec.DisallowedTools, false)
	if hasToolNamed(filtered, "Agent") {
		t.Error("general-purpose sub-agent should NOT have Agent tool (prevents infinite recursion)")
	}
	if hasToolNamed(filtered, "AskUserQuestion") {
		t.Error("general-purpose sub-agent should NOT have AskUserQuestion")
	}
	if !hasToolNamed(filtered, "ReadFile") {
		t.Error("ReadFile should remain for general-purpose")
	}
	if !hasToolNamed(filtered, "EditFile") {
		t.Error("EditFile should remain for general-purpose (sync)")
	}
}

// 进程内队友的工具集从 Lead 那份过滤而来，禁用名单要在 spec 自带的基础上再并上
// TeammateDisallowedTools，否则队友会连团队成员管理一起继承过去。
func TestTeammateFilterBlocksTeamManagement(t *testing.T) {
	reg := makeRegistry("ReadFile", "Bash", "EditFile", "Agent", "TeamCreate", "TeamDelete", "SendMessage")
	spec := BuiltinSpecs["general-purpose"]

	disallowed := append(append([]string{}, spec.DisallowedTools...), TeammateDisallowedTools...)
	filtered := FilterToolsForAgent(reg, spec.Tools, disallowed, false)

	for _, name := range []string{"Agent", "TeamCreate", "TeamDelete"} {
		if hasToolNamed(filtered, name) {
			t.Errorf("队友工具集不应包含 %s", name)
		}
	}
	for _, name := range []string{"ReadFile", "Bash", "EditFile"} {
		if !hasToolNamed(filtered, name) {
			t.Errorf("队友工具集缺少 %s", name)
		}
	}
}
