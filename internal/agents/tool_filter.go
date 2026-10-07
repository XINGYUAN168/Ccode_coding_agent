package agents

import (
	"strings"

	"ccode/internal/tools"
)

// AllAgentDisallowedTools 是任何子 Agent 都拿不到的工具，Agent 定义里的白名单也开不了它们。
// 其中 TaskOutput、ExitPlanMode、EnterPlanMode、Workflow 在当前工具集里并没有对应实现，
// 留着是为了把「子 Agent 不该有哪些能力」这份清单写全，过滤时遇到不认识的名字直接跳过，
// 将来补上同名工具就自动生效。
var AllAgentDisallowedTools = map[string]bool{
	"TaskOutput":      true,
	"ExitPlanMode":    true,
	"EnterPlanMode":   true,
	"Agent":           true,
	"AskUserQuestion": true,
	"TaskStop":        true,
	"Workflow":        true,
}

// CustomAgentDisallowedTools 是 AllAgentDisallowedTools 的副本。单独保留此映射，
// 便于以后为自定义 Agent 增加限制而不改动全局清单。
var CustomAgentDisallowedTools = map[string]bool{
	"TaskOutput":      true,
	"ExitPlanMode":    true,
	"EnterPlanMode":   true,
	"Agent":           true,
	"AskUserQuestion": true,
	"TaskStop":        true,
	"Workflow":        true,
}

// AsyncAgentAllowedTools 是异步（后台）Agent 的工具白名单：不含 Agent（禁止嵌套生成）、
// TaskOutput、ExitPlanMode 和 TaskStop。本地工具名映射为 FILE_READ → ReadFile、
// FILE_EDIT → EditFile、FILE_WRITE → WriteFile。
var AsyncAgentAllowedTools = map[string]bool{
	"ReadFile":        true,
	"WebSearch":       true,
	"TodoWrite":       true,
	"Grep":            true,
	"WebFetch":        true,
	"Glob":            true,
	"Bash":            true,
	"EditFile":        true,
	"WriteFile":       true,
	"NotebookEdit":    true,
	"Skill":           true,
	"LoadSkill":       true,
	"SyntheticOutput": true,
	"ToolSearch":      true,
	// ToolSearch 只负责把 schema 读出来，真正调用要靠 mcp_call，
	// 两个得成对放行，否则子 Agent 看得见工具却调不动
	"mcp_call":      true,
	"EnterWorktree": true,
	"ExitWorktree":  true,
}

// InProcessTeammateAllowedTools 是进程内队员在异步工具白名单之外可用的协作工具，
// 使其能够管理共享任务列表并向其他队员发送消息。
var InProcessTeammateAllowedTools = map[string]bool{
	"TaskCreate":  true,
	"TaskGet":     true,
	"TaskList":    true,
	"TaskUpdate":  true,
	"SendMessage": true,
	"CronCreate":  true,
	"CronDelete":  true,
	"CronList":    true,
}

// TeammateDisallowedTools 队友在协作工具之外额外被挡掉的工具。组建和解散团队
// 由 Lead 负责，队友只管干活和相互协调，不参与团队成员管理。
var TeammateDisallowedTools = []string{"TeamCreate", "TeamDelete"}

func IsMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp__")
}

func FilterToolsForAgent(reg *tools.Registry, allowedTools, disallowedTools []string, isAsync bool) *tools.Registry {
	return FilterToolsForAgentEx(reg, allowedTools, disallowedTools, isAsync, false, false)
}

// FilterToolsForAgentEx
//
// 各层按顺序应用：1. MCP 工具（mcp__*）始终放行；2. ALL_AGENT_DISALLOWED_TOOLS
// 执行全局阻止（递归或仅主线程工具）；3. CUSTOM_AGENT_DISALLOWED_TOOLS 只限制自定义
// （非内置）Agent；4. ASYNC_AGENT_ALLOWED_TOOLS 是后台 Agent 白名单，进程内队员还可使用
// IN_PROCESS_TEAMMATE_ALLOWED_TOOLS；5. Agent 定义的 disallowedTools 是定义级黑名单；
// 6. Agent 定义的 tools 与结果取白名单交集（"*" 表示禁用此层）。
//
// isCustom 表示从 .ccode/agents/ 加载而非内置的 Agent；isInProcessTeammate
// 表示通过 TeamCreate 或 SpawnTeammate 创建的进程内队员。
func FilterToolsForAgentEx(reg *tools.Registry, allowedTools, disallowedTools []string, isAsync, isCustom, isInProcessTeammate bool) *tools.Registry {
	disallowed := make(map[string]bool, len(disallowedTools))
	for _, name := range disallowedTools {
		disallowed[name] = true
	}

	allowed := make(map[string]bool, len(allowedTools))
	hasWhitelist := len(allowedTools) > 0 && !(len(allowedTools) == 1 && allowedTools[0] == "*")
	for _, name := range allowedTools {
		allowed[name] = true
	}

	filtered := tools.NewRegistry()
	for _, t := range reg.ListTools() {
		name := t.Name()

		// 第 1 层：始终允许 MCP 工具。
		if IsMCPTool(name) {
			filtered.Register(t)
			continue
		}

		// 第 2 层：应用于所有子 Agent 的全局禁用清单。
		if AllAgentDisallowedTools[name] {
			continue
		}

		// 第 3 层：自定义 Agent 的额外限制。
		if isCustom && CustomAgentDisallowedTools[name] {
			continue
		}

		// 第 4 层：异步 Agent 白名单，并包含进程内队员扩展。
		if isAsync && !AsyncAgentAllowedTools[name] {
			if isInProcessTeammate {
				// 进程内队员还可使用 Agent（仅同步子 Agent，由调用处校验）和协作工具。
				if name == "Agent" || InProcessTeammateAllowedTools[name] {
					filtered.Register(t)
					continue
				}
			}
			continue
		}

		// 第 5 层：定义级禁用清单。
		if disallowed[name] {
			continue
		}

		// 第 6 层：定义级允许清单（与白名单取交集）。
		if hasWhitelist && !allowed[name] {
			continue
		}

		filtered.Register(t)
	}
	return filtered
}
