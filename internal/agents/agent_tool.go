package agents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"ccode/internal/agent"
	"ccode/internal/conversation"
	"ccode/internal/llm"
	"ccode/internal/permissions"
	"ccode/internal/teams"
	"ccode/internal/tools"
	"ccode/internal/worktree"
)

// sanitizeSlugSegment 把 [a-zA-Z0-9._-] 以外的字符替换为连字符，并清理多余的
// 分隔符，使结果可安全用作 Git 分支名。
var unsafeSlugChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeSlugSegment(s string) string {
	clean := unsafeSlugChars.ReplaceAllString(s, "-")
	clean = strings.Trim(clean, "-_.")
	if clean == "" {
		clean = "subagent"
	}
	if len(clean) > 40 {
		clean = clean[:40]
	}
	return clean
}

type SubAgentProgress struct {
	AgentDesc string
	AgentType string
	ToolName  string
	ToolArgs  map[string]any
	Elapsed   float64
	IsError   bool
	Done      bool
	ToolCount int
	TotalTime float64
}

const ForkBoilerplateTag = "<fork_boilerplate>"

// ForkAgentType 是 fork 子 Agent 的类型名，拼进 QuerySource 用来识别 fork 出来的会话。
const ForkAgentType = "fork"

// GeneralPurposeAgentType 是 fork 关闭时，省略 subagent_type 的回退目标。
const GeneralPurposeAgentType = "general-purpose"

// ForkQuerySource 是 fork 子 Agent 的来源标记，形如 `agent:builtin:fork`。
// 它是判断「当前身处 fork 子 Agent」的首选信号，拿不到时再退回扫描对话历史里的
// ForkBoilerplateTag。
const ForkQuerySource = "agent:builtin:" + ForkAgentType

type AgentTool struct {
	Client        llm.Client
	ModelResolver func(string) (llm.Client, error)
	Registry      *tools.Registry
	Protocol      string
	TaskMgr       *TaskManager
	ProgressCh    chan<- SubAgentProgress
	Loader        *AgentLoader
	Conversation  *conversation.Manager // 父对话，Fork 时需要
	TeamMgr       *teams.TeamManager    // 可选；启用 team_name 参数

	// ParentChecker 是父 Agent 的权限检查器。Sandbox 和 RuleEngine 会被复用；只有
	// 子 Agent 定义或调用设置了不同 permissionMode 时才覆盖 Mode。该字段可选，
	// 为 nil 时子 Agent 不继承检查器，适用于早期启动阶段和测试。
	ParentChecker *permissions.Checker

	// QuerySource 标识创建当前实例的 Agent，用于检测嵌套 fork。主线程中为空；
	// AgentTool 位于已创建的子 Agent 中时设为 ForkQuerySource 或
	// "agent:builtin:<type>"。该标记不依赖会话历史，即使 fork 模板被压缩出历史仍有效。
	QuerySource string

	// ForkDisabled 为真时，省略 subagent_type 不再 fork，而是回退到通用 agent。
	// 用「关闭」而不是「开启」语义，是为了让零值就是默认行为（fork 可用），
	// 每个构造点不必都显式赋值。
	ForkDisabled bool
}

func (t *AgentTool) Name() string                 { return "Agent" }
func (t *AgentTool) Category() tools.ToolCategory { return tools.CategoryCommand }

func (t *AgentTool) Description() string {
	desc := `Launch a sub-agent to handle a complex task. Each sub-agent runs independently with its own context. The sub-agent cannot see the current conversation.

This is ONE tool with multiple roles. Roles are NOT separate tools — you pick one by passing its name in the "subagent_type" parameter. Do not search for a tool named after a role; call THIS tool ("Agent") and set "subagent_type".

Available roles for the "subagent_type" parameter:`

	if t.Loader != nil {
		for _, name := range t.Loader.ListNames() {
			def := t.Loader.Get(name)
			desc += "\n- " + name + ": " + def.WhenToUse
		}
	} else {
		desc += "\n- general-purpose: Full tool access for multi-step tasks (default)"
		desc += "\n- plan: Read-only tools for designing implementation plans"
		desc += "\n- explore: Read-only search agent for locating code"
	}

	desc += `

Example call shape:
{
  "name": "Agent",
  "input": {
    "subagent_type": "<role from the list above>",
    "description": "Short task label",
    "prompt": "Detailed instructions — the sub-agent has zero prior context"
  }
}

Write a detailed prompt explaining what the sub-agent should do and why — it has no prior context.
When tasks are independent, launch multiple sub-agents in parallel by making multiple Agent tool calls in a single response.`
	return desc
}

func (t *AgentTool) Schema() map[string]any {
	agentTypes := []string{"general-purpose", "plan", "explore"}
	if t.Loader != nil {
		agentTypes = t.Loader.ListNames()
	}

	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type":        "string",
					"description": "A short (3-5 word) description of the task",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task for the agent to perform. Be detailed — the agent has no context from this conversation.",
				},
				"subagent_type": map[string]any{
					"type":        "string",
					"enum":        agentTypes,
					"description": "The type of agent to use. If omitted, forks current conversation context.",
				},
				"model": map[string]any{
					"type":        "string",
					"enum":        []string{"sonnet", "opus", "haiku"},
					"description": "Override the model for this agent.",
				},
				"run_in_background": map[string]any{
					"type":        "boolean",
					"description": "Set to true to run the agent in the background.",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Name for the agent, enabling SendMessage communication.",
				},
				"isolation": map[string]any{
					"type":        "string",
					"enum":        []string{"worktree"},
					"description": "Isolation mode. Set to 'worktree' to give the agent its own git worktree so its file edits don't collide with peers or the lead. REQUIRED when spawning two or more teammates in parallel that may write files; STRONGLY RECOMMENDED for any single teammate doing non-trivial file edits while the lead is still working in the same repo. Skip only for read-only tasks (explore/grep/plan) or when you explicitly want the teammate to share your working tree.",
				},
				"plan_mode_required": map[string]any{
					"type":        "boolean",
					"description": "Only meaningful together with team_name. When true, the teammate starts in plan mode: it can read and investigate but cannot modify anything until it submits a plan and you approve it via SendMessage with type='plan_approval_response'. Use it for risky or ambiguous tasks where a wrong direction would cost a lot of rework.",
				},
				"team_name": map[string]any{
					"type":        "string",
					"description": "REQUIRED when creating team members. Spawns the agent as a long-running teammate under this team (created via TeamCreate). Unlike regular sub-agents, team members run in their own terminal, persist after the lead returns, and communicate with each other via SendMessage. Without team_name the agent runs as a one-shot sub-agent that blocks and returns inline.",
				},
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{"default", "acceptEdits", "plan", "bypassPermissions"},
					"description": "Permission mode override for the spawned agent (e.g., 'plan' to require plan approval).",
				},
			},
			"required": []string{"description", "prompt"},
		},
	}
}

func (t *AgentTool) selectClient(specModel, overrideModel string) llm.Client {
	model := overrideModel
	if model == "" {
		model = specModel
	}
	if model == "" || model == "inherit" {
		return t.Client
	}
	if t.ModelResolver != nil {
		if c, err := t.ModelResolver(model); err == nil {
			return c
		}
	}
	return t.Client
}

func (t *AgentTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	description, _ := args["description"].(string)
	prompt, _ := args["prompt"].(string)
	if description == "" || prompt == "" {
		return tools.ToolResult{Output: "Error: description and prompt are required", IsError: true}
	}

	subagentType, _ := args["subagent_type"].(string)
	modelOverride, _ := args["model"].(string)
	runInBackground, _ := args["run_in_background"].(bool)
	agentName, _ := args["name"].(string)
	teamName, _ := args["team_name"].(string)
	modeOverride, _ := args["mode"].(string)
	isolation, _ := args["isolation"].(string)
	planModeRequired, _ := args["plan_mode_required"].(bool)

	if modeOverride != "" && !validPermissionModes[modeOverride] {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: invalid mode '%s'. Valid: default, acceptEdits, plan, bypassPermissions", modeOverride),
			IsError: true,
		}
	}

	// 团队成员路径：显式 team_name 会把本次创建变成该团队后端中的长期队员。
	// 队员启动后 Lead 立即取回控制权，后续通过 SendMessage 和邮箱通知协作。
	if teamName != "" && t.TeamMgr != nil {
		return t.runAsTeammate(ctx, teamName, agentName, description, prompt, modelOverride, subagentType, isolation, planModeRequired)
	}

	// 省略 subagent_type 时的走向由配置决定：fork 开着就继承父对话，关着就当成
	// 没指定类型，回退到通用 agent。这里不报错，模型只是没填一个可选参数，
	// 为此中断一次调用不值得，回退到通用 agent 一样能把活干了。
	if subagentType == "" && t.ForkDisabled {
		subagentType = GeneralPurposeAgentType
	}

	// fork 路径：未指定 subagent_type。
	if subagentType == "" {
		return t.runFork(ctx, description, prompt, modelOverride, agentName)
	}

	// 定义路径：从加载器或内置定义解析 spec。
	var spec SubAgentSpec
	if t.Loader != nil {
		def := t.Loader.Get(subagentType)
		if def == nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error: unknown agent type '%s'. Available: %s", subagentType, strings.Join(t.Loader.ListNames(), ", ")),
				IsError: true,
			}
		}
		spec = def.ToSpec()
	} else {
		s, ok := BuiltinSpecs[subagentType]
		if !ok {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error: unknown agent type '%s'. Available: general-purpose, plan, explore", subagentType),
				IsError: true,
			}
		}
		spec = s
	}

	// 单次调用的模式覆盖优先于定义中的 permissionMode。
	if modeOverride != "" {
		spec.PermissionMode = modeOverride
	}

	// 调用方传 run_in_background，或者 Agent 定义自己标了 background，都走异步派发。
	// 两个条件是或的关系：定义里声明的后台属性不该被调用方漏传而失效。
	if runInBackground || spec.Background {
		return t.runAsync(ctx, spec, description, prompt, modelOverride)
	}
	return t.runSync(ctx, spec, description, prompt, modelOverride, isolation)
}

func (t *AgentTool) runSync(ctx context.Context, spec SubAgentSpec, description, prompt, modelOverride, isolation string) tools.ToolResult {
	subRegistry := FilterToolsForAgent(t.Registry, spec.Tools, spec.DisallowedTools, false)
	client := t.selectClient(spec.Model, modelOverride)

	subAgent := agent.New(client, subRegistry, t.Protocol)
	subAgent.Checker = deriveSubAgentChecker(t.ParentChecker, spec.PermissionMode)
	if spec.MaxTurns > 0 {
		subAgent.MaxIterations = spec.MaxTurns
	} else {
		subAgent.MaxIterations = 200
	}

	// 工作树隔离：为子 Agent 创建独立工作树。
	var wtResult *worktree.AgentWorktreeResult
	if isolation == "worktree" {
		slug := generateAgentSlug(description)
		var err error
		wtResult, err = worktree.CreateAgentWorktree(ctx, slug)
		if err != nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error creating agent worktree: %s", err),
				IsError: true,
			}
		}
		subAgent.WorkDir = wtResult.WorktreePath

		// 把工作树提示注入提示词。
		parentCwd, _ := os.Getwd()
		notice := worktree.BuildWorktreeNotice(parentCwd, wtResult.WorktreePath)
		prompt = notice + "\n\n" + prompt
	}

	conv := conversation.NewManager()
	if spec.SystemPromptOverride != "" {
		conv.AddSystemReminder(spec.SystemPromptOverride)
	}
	// initialPrompt 放在首轮用户消息之前。
	if spec.InitialPrompt != "" {
		conv.AddUserMessage(spec.InitialPrompt)
	}
	conv.AddUserMessage(prompt)

	start := time.Now()
	var output strings.Builder
	toolCount := 0
	ch := subAgent.Run(ctx, conv)

	for ev := range ch {
		switch e := ev.(type) {
		case agent.StreamText:
			output.WriteString(e.Text)
		case agent.PermissionRequestEvent:
			// 子 Agent 没有可供询问的 UI。自动拒绝可避免 executeSingleTool 永久等待
			// respCh；该通道容量为 1，因此发送不会阻塞。
			e.ResponseCh <- agent.PermissionReply{Response: agent.PermDeny}
		case agent.ToolResultEvent:
			toolCount++
			emitProgress(t.ProgressCh, ctx, SubAgentProgress{
				AgentDesc: description,
				AgentType: spec.Name,
				ToolName:  e.ToolName,
				ToolArgs:  map[string]any{"_summary": e.Output},
				Elapsed:   e.Elapsed.Seconds(),
				IsError:   e.IsError,
			})
		case agent.ErrorEvent:
			emitProgress(t.ProgressCh, ctx, SubAgentProgress{
				AgentDesc: description,
				AgentType: spec.Name,
				Done:      true,
				ToolCount: toolCount,
				TotalTime: time.Since(start).Seconds(),
				IsError:   true,
			})
			return tools.ToolResult{
				Output:  fmt.Sprintf("Agent failed: %s", e.Message),
				IsError: true,
			}
		}
	}

	elapsed := time.Since(start)

	emitProgress(t.ProgressCh, ctx, SubAgentProgress{
		AgentDesc: description,
		AgentType: spec.Name,
		Done:      true,
		ToolCount: toolCount,
		TotalTime: elapsed.Seconds(),
	})

	result := output.String()
	if result == "" {
		result = "(agent produced no output)"
	}

	// 清理工作树：子 Agent 使用独立工作树时，干净则自动删除，有改动则保留。
	if wtResult != nil {
		if worktree.HasWorktreeChanges(ctx, wtResult.WorktreePath, wtResult.HeadCommit) {
			result += fmt.Sprintf("\n\nWorktree kept at %s (branch %s) — has uncommitted changes or new commits.",
				wtResult.WorktreePath, wtResult.WorktreeBranch)
		} else {
			worktree.RemoveAgentWorktree(ctx, wtResult.WorktreePath, wtResult.WorktreeBranch, wtResult.GitRoot)
		}
	}

	return tools.ToolResult{
		Output: fmt.Sprintf("Agent \"%s\" completed in %s.\n\n%s", description, elapsed.Round(time.Millisecond), result),
	}
}

func (t *AgentTool) runFork(ctx context.Context, description, prompt, modelOverride, agentName string) tools.ToolResult {
	if t.Conversation == nil {
		return tools.ToolResult{Output: "Error: fork requires parent conversation context", IsError: true}
	}

	// 嵌套 fork 使用两层保护：
	// （1）优先检查 querySource。AgentTool 在 fork 子进程内构造时会设置该字段，
	//      它不受压缩影响，也能覆盖会话历史被重写或摘要的情况。
	// （2）回退扫描消息中的 ForkBoilerplateTag。
	if t.QuerySource == ForkQuerySource {
		return tools.ToolResult{
			Output:  "Error: cannot fork from a forked agent. Use subagent_type to spawn a definition-based agent instead.",
			IsError: true,
		}
	}
	for _, msg := range t.Conversation.GetMessages() {
		if strings.Contains(msg.Content, ForkBoilerplateTag) {
			return tools.ToolResult{
				Output:  "Error: cannot fork from a forked agent. Use subagent_type to spawn a definition-based agent instead.",
				IsError: true,
			}
		}
	}

	// 构建 fork 会话：复制父消息、补齐未完成的 tool_use，再追加任务。
	forkedConv := buildForkedConversation(t.Conversation, prompt)

	client := t.selectClient("", modelOverride)
	// fork 原样继承父 Agent 的工具池，这样发出去的请求前缀和父 Agent 逐字节一致，
	// prompt 缓存才命中得上。其中的 Agent 工具被换成一份 QuerySource=ForkQuerySource
	// 的浅拷贝，于是再往下 fork 会在 runFork 的首道检查那里被拒。
	subRegistry := cloneRegistryForFork(t.Registry)

	subAgent := agent.New(client, subRegistry, t.Protocol)
	subAgent.Checker = t.ParentChecker // fork 原样继承父 Agent 的权限状态
	subAgent.MaxIterations = 200

	// fork 始终在后台运行。
	taskName := "fork"
	if agentName != "" {
		taskName = agentName
	}
	taskID := t.TaskMgr.CreateTask(taskName + ": " + truncate(prompt, 50))
	forkCtx, cancel := context.WithCancel(ctx)
	t.TaskMgr.SetRunning(taskID, cancel)

	go func() {
		var output string
		ch := subAgent.Run(forkCtx, forkedConv)
		for ev := range ch {
			switch e := ev.(type) {
			case agent.StreamText:
				output += e.Text
			case agent.PermissionRequestEvent:
				// 无界面可交互，因此自动拒绝，避免 fork 阻塞在 respCh。
				e.ResponseCh <- agent.PermissionReply{Response: agent.PermDeny}
			case agent.ErrorEvent:
				t.TaskMgr.SetFailed(taskID, e.Message)
				return
			}
		}
		t.TaskMgr.SetCompleted(taskID, output)
	}()

	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Forked agent \"%s\" launched in background (task %s). Results will arrive via task-notification.",
			description, taskID,
		),
	}
}

// emitProgress 发送 SubAgentProgress 事件且永不阻塞调用方。消费者（TUI）处理不及时
// 时丢弃事件，因为进度只是尽力提供的界面反馈，不承载关键状态。若在此阻塞，
// ProgressCh 缓冲区填满后会卡死子 Agent 循环，使 ESC 或 ctx 取消无法生效。
func emitProgress(ch chan<- SubAgentProgress, ctx context.Context, p SubAgentProgress) {
	if ch == nil {
		return
	}
	select {
	case ch <- p:
	case <-ctx.Done():
	default:
		// 消费者处理不及时；丢弃事件，避免阻塞子 Agent 的事件循环。
	}
}

// deriveSubAgentChecker 把子 Agent 的 permissionMode 传给新建的 Agent：mode 会覆盖
// Agent 定义中的 permissionMode，随后进入子 Agent 的 ToolPermissionContext。
// Sandbox 和 RuleEngine 保持共享，只替换 Mode，使工具调用使用不同决策矩阵而不分叉
// 权限状态。
//
// 未请求覆盖时原样返回父检查器。
func deriveSubAgentChecker(parent *permissions.Checker, modeOverride string) *permissions.Checker {
	if parent == nil {
		return nil
	}
	if modeOverride == "" || permissions.PermissionMode(modeOverride) == parent.Mode {
		return parent
	}
	return permissions.NewChecker(parent.Sandbox, parent.RuleEngine, permissions.PermissionMode(modeOverride))
}

// cloneRegistryForFork 返回父注册表的完整副本，但把其中每个 *AgentTool 替换为
// QuerySource 设为 ForkQuerySource 的浅拷贝。
// 这样 fork 子 Agent 看到的工具定义和父 Agent 在协议层完全一样（prompt 缓存因此命中），
// 但它再想 fork 时会在调用那一刻被 runFork 里的 QuerySource 检查拦下。
func cloneRegistryForFork(reg *tools.Registry) *tools.Registry {
	forked := tools.NewRegistry()
	for _, tool := range reg.ListTools() {
		if at, ok := tool.(*AgentTool); ok {
			clone := *at
			clone.QuerySource = ForkQuerySource
			forked.Register(&clone)
			continue
		}
		forked.Register(tool)
	}
	return forked
}

const forkBoilerplate = ForkBoilerplateTag + `
You are a forked worker process. You are NOT the main agent.
Rules (non-negotiable):
1. Do NOT fork again.
2. Do NOT converse, ask questions, or request confirmation.
3. Use tools directly: read files, search code, make changes.
4. Stay strictly within your assigned task scope.
5. Final report must be under 500 characters, starting with "Scope:".
` + "</fork_boilerplate>"

func buildForkedConversation(parent *conversation.Manager, task string) *conversation.Manager {
	forked := conversation.NewManager()
	msgs := parent.GetMessages()

	// 逐块复制父会话中的思考、正文、工具调用和工具结果，并补齐被中断工具的占位结果。
	for _, msg := range msgs {
		if len(msg.ToolUses) > 0 && len(msg.ToolResults) == 0 {
			forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, msg.ToolUses)
			var placeholders []conversation.ToolResultBlock
			for _, tu := range msg.ToolUses {
				placeholders = append(placeholders, conversation.ToolResultBlock{
					ToolUseID: tu.ToolUseID,
					Content:   "(tool execution interrupted by fork)",
					IsError:   false,
				})
			}
			forked.AddToolResultsMessage(placeholders)
		} else if len(msg.ToolUses) > 0 {
			forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, msg.ToolUses)
		} else if len(msg.ToolResults) > 0 {
			forked.AddToolResultsMessage(msg.ToolResults)
		} else if msg.Role == "assistant" {
			if len(msg.ThinkingBlocks) > 0 {
				forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, nil)
			} else {
				forked.AddAssistantMessage(msg.Content)
			}
		} else {
			forked.AddUserMessage(msg.Content)
		}
	}

	// 把 fork 模板和任务追加为用户消息。
	forked.AddUserMessage(forkBoilerplate + "\n\nYour task:\n" + task)
	return forked
}

func (t *AgentTool) runAsync(ctx context.Context, spec SubAgentSpec, description, prompt, modelOverride string) tools.ToolResult {
	client := t.selectClient(spec.Model, modelOverride)
	taskID := SpawnSubAgent(ctx, t.TaskMgr, client, t.Registry, t.Protocol, spec, prompt, t.ParentChecker)

	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Agent \"%s\" launched in background (task %s). You will be notified when it completes.",
			description, taskID,
		),
	}
}

// runAsTeammate 在现有 Team 中注册长期运行的队员。与 runSync/runAsync 不同，
// 此路径不会让 Lead 等待队员输出：Lead 总会立即返回，之后通过 SendMessage 和
// 团队邮箱中的空闲通知协调。teams.SpawnTeammate 根据 Team.Mode 选择进程内、
// tmux 或 iTerm 后端。
//
// isolation 为 worktree 且已配置 WorktreeMgr 时，队员获得独立 Git 工作树，避免
// 文件改动与其他队员冲突。
func (t *AgentTool) runAsTeammate(
	ctx context.Context,
	teamName, memberName, description, prompt, modelOverride, subagentType, isolation string,
	planModeRequired bool,
) tools.ToolResult {
	// 团队不存在就顺手建一个：coordinator 模式下 TeamCreate 不在白名单里，
	// 要求 Lead 先建团队再派人，它会卡在第一步。
	team := t.TeamMgr.GetTeam(teamName)
	if team == nil {
		team = t.TeamMgr.CreateTeamFull(teamName, teams.DetectBackend(), teams.LeadName, description)
	}

	if memberName == "" {
		memberName = sanitizeSlugSegment(description)
	}
	if _, exists := team.Members[memberName]; exists {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: team '%s' already has a member named '%s'", teamName, memberName),
			IsError: true,
		}
	}

	// 设置 subagent_type 时解析 spec，使队员遵守同类型其他子 Agent 的禁用列表；
	// 没有 spec 时把完整注册表交给队员。
	var spec SubAgentSpec
	if subagentType != "" {
		if t.Loader != nil {
			if def := t.Loader.Get(subagentType); def != nil {
				spec = def.ToSpec()
			}
		} else if s, ok := BuiltinSpecs[subagentType]; ok {
			spec = s
		}
	}

	teammateDisallowed := append(append([]string{}, spec.DisallowedTools...), TeammateDisallowedTools...)
	subRegistry := FilterToolsForAgent(t.Registry, spec.Tools, teammateDisallowed, false)
	// 队友协作工具：以队友自己的名字发消息，并注入团队共享任务板工具
	// （覆盖继承来的个人版同名工具，让队友之间共享同一份任务列表）。
	subRegistry.Register(&teams.SendMessageTool{TeamMgr: t.TeamMgr, SenderName: memberName})
	subRegistry.Register(&teams.TaskCreateTool{TeamMgr: t.TeamMgr, TeamName: teamName, AgentName: memberName})
	subRegistry.Register(&teams.TaskGetTool{TeamMgr: t.TeamMgr, TeamName: teamName})
	subRegistry.Register(&teams.TaskListTool{TeamMgr: t.TeamMgr, TeamName: teamName})
	subRegistry.Register(&teams.TaskUpdateTool{TeamMgr: t.TeamMgr, TeamName: teamName})
	client := t.selectClient(spec.Model, modelOverride)

	var otherMembers []string
	for n := range team.Members {
		otherMembers = append(otherMembers, n)
	}
	addendum := teams.BuildTeammateAddendum(teamName, memberName, otherMembers)

	var workdir string
	if isolation == "worktree" {
		slug := generateAgentSlug(description)
		wtResult, err := worktree.CreateAgentWorktree(ctx, slug)
		if err != nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error creating teammate worktree: %s", err),
				IsError: true,
			}
		}
		workdir = wtResult.WorktreePath
		parentCwd, _ := os.Getwd()
		notice := worktree.BuildWorktreeNotice(parentCwd, wtResult.WorktreePath)
		prompt = notice + "\n\n" + prompt
	}

	team.SetMemberMeta(memberName, subagentType, modelOverride, workdir)

	// 标了 plan_mode_required 的队友以计划模式启动：只能读不能改，
	// 写出计划交 Lead 审批，通过后才切回正常权限。
	teammateChecker := t.ParentChecker
	if planModeRequired && t.ParentChecker != nil {
		teammateChecker = permissions.NewChecker(
			t.ParentChecker.Sandbox, t.ParentChecker.RuleEngine, permissions.ModePlan)
	}

	result, err := teams.SpawnTeammate(ctx, teams.TeammateSpawnConfig{
		Team:       team,
		MemberName: memberName,
		Checker:    teammateChecker,
		Task:       prompt,
		Addendum:   addendum,
		Client:     client,
		Registry:   subRegistry,
		Protocol:   t.Protocol,
		Workdir:    workdir,
	})
	if err != nil {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error spawning teammate: %v", err),
			IsError: true,
		}
	}

	// 进程内创建会返回活动事件通道；后台排空它，避免通道填满后阻塞 goroutine。
	// Lead 可见的进度通过邮箱传递，而不是通过这里的排空逻辑。
	if result.Mode == teams.ModeInProcess && result.EventCh != nil {
		go drainTeammateEvents(memberName, result.EventCh, t.ProgressCh)
	}

	backendHint := string(result.Mode)
	if result.PaneID != "" {
		backendHint += " pane=" + result.PaneID
	}
	if workdir != "" {
		backendHint += " worktree=" + workdir
	}
	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Teammate \"%s\" started on team \"%s\" [%s]. Use SendMessage to talk to it; its idle notifications will arrive as system reminders.",
			memberName, teamName, backendHint,
		),
	}
}

// drainTeammateEvents 消费队员事件流，避免生产端因通道填满而阻塞。设置 ProgressCh
// 时会转发工具和错误事件，供父界面展示活动状态。
func drainTeammateEvents(name string, ch <-chan agent.AgentEvent, progressCh chan<- SubAgentProgress) {
	for ev := range ch {
		if progressCh == nil {
			continue
		}
		switch e := ev.(type) {
		case agent.ToolResultEvent:
			emitProgress(progressCh, context.Background(), SubAgentProgress{
				AgentDesc: name,
				AgentType: "teammate",
				ToolName:  e.ToolName,
				Elapsed:   e.Elapsed.Seconds(),
				IsError:   e.IsError,
			})
		case agent.ErrorEvent:
			emitProgress(progressCh, context.Background(), SubAgentProgress{
				AgentDesc: name,
				AgentType: "teammate",
				ToolName:  "error",
				IsError:   true,
			})
		}
	}
}

// generateAgentSlug 为子 Agent 工作树生成符合 ^agent-a[0-9a-f]{7}$ 的短名称。
func generateAgentSlug(description string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "agent-a" + hex.EncodeToString(b)[:7]
}
