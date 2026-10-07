package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"ccode/internal/agent"
	"ccode/internal/config"
	"ccode/internal/conversation"
	"ccode/internal/llmfactory"
	"ccode/internal/mcp"
	"ccode/internal/prompt"
	"ccode/internal/session"
	"ccode/internal/skills"
	"ccode/internal/teams"
	"ccode/internal/tools"
	"ccode/internal/worktree"
)

// teammateArgs 保存进程作为队员工作进程启动时，从命令行解析出的参数。
// 这类进程由 teams.BuildTeammateCLI 生成命令，再通过 tmux/iTerm 启动。
type teammateArgs struct {
	teamName   string
	memberName string
}

// parseTeammateFlags 在参数含有 --teammate 时返回解析结果和 true，表示进入队员
// 工作进程模式；否则返回 false，由调用方启动普通 TUI。
//
// teams.BuildTeammateCLI 生成的格式如下：
//
//	ccode --teammate --team-name <t> --agent-name <n>
//
// 解析逻辑有意保持最小化：只识别工作进程需要的三个标志，而且标志和值必须是
// 分开的参数。
func parseTeammateFlags(args []string) (teammateArgs, bool) {
	var out teammateArgs
	if len(args) == 0 || args[0] != "--teammate" {
		return out, false
	}
	i := 1
	for i < len(args) {
		switch args[i] {
		case "--team-name":
			if i+1 < len(args) {
				out.teamName = args[i+1]
				i += 2
				continue
			}
		case "--agent-name":
			if i+1 < len(args) {
				out.memberName = args[i+1]
				i += 2
				continue
			}
		}
		i++
	}
	return out, true
}

// runTeammate 把当前进程作为现有团队的工作进程启动。它加载与 TUI 相同的配置，
// 构造工具注册表，再进入 teams.RunInProcessTeammate。首个任务从邮箱读取：Lead
// 会在通过 tmux/iTerm 启动进程前把任务写入邮箱，详见 teams.SpawnTeammate。
func runTeammate(args teammateArgs) error {
	if args.teamName == "" || args.memberName == "" {
		return fmt.Errorf("--teammate requires --team-name and --agent-name")
	}

	cfg, err := config.LoadConfig("")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if len(cfg.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	provider := cfg.Providers[0]
	factory := llmfactory.NewFactory(llmfactory.Options{})
	if _, err := factory.NewAll(cfg.Providers); err != nil {
		return fmt.Errorf("Provider 预检失败: %w", err)
	}

	wd, _ := os.Getwd()
	sessionID := session.NewID()

	// 把 SIGINT/SIGTERM 转成上下文取消，使关闭面板或在标签页按 Ctrl-C 时能
	// 正常退出循环并执行延迟清理。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Skill 清单随首条 system-reminder 注入对话（见下面的 ag.SkillSection），
	// 同时挂到 LoadSkill 工具上供模型按名字加载。
	skillCatalog := skills.LoadCatalog(wd)
	env := prompt.DetectEnvironment(wd)
	env.Model = provider.Model
	systemPrompt := prompt.BuildSystemPrompt(env, prompt.BuildOptions{})

	client, err := factory.New(provider, systemPrompt)
	if err != nil {
		return fmt.Errorf("create LLM client: %w", err)
	}

	// 队员进程直接从磁盘上的 config.json 把团队捞回来，这样它看到的队友名单
	// 和 Lead 那边是同一份。团队目录在用户主目录下，不受 worktree 换工作目录影响。
	teamMgr := teams.NewTeamManager()
	team := teamMgr.GetTeam(args.teamName)
	if team == nil {
		// 配置还没落盘（例如 Lead 刚建完团队就 spawn），退化成本地构造一份，
		// 邮箱目录按同样的约定拼出来，投递仍然对得上。
		team = teams.NewTeam(args.teamName, teams.ModeInProcess)
		teamMgr.CreateTeamWith(team)
	}

	registry := buildTeammateRegistry(ctx, teammateToolOptions{
		WorkDir:    wd,
		Protocol:   provider.Protocol,
		SessionID:  sessionID,
		TeamMgr:    teamMgr,
		TeamName:   args.teamName,
		MemberName: args.memberName,
		MCPServers: cfg.MCPServers,

		BaseURL:       provider.BaseURL,
		ContextWindow: provider.GetContextWindow(),
	})

	member := team.AddMember(args.memberName, client, registry, provider.Protocol)
	member.AgentRef.SkillSection = buildPrintSkillSection(skillCatalog)

	// Skill 工具的宿主由队友自己的 Agent 担任，所以要等 AddMember 建好 Agent
	// 之后再接入。没有 ForkHost，声明 fork 模式的 skill 会退回 inline 执行。
	registry.Register(&skills.LoadSkillTool{Catalog: skillCatalog, Host: member.AgentRef})
	registry.Register(&skills.InstallSkillTool{Catalog: skillCatalog})

	addendum := teams.BuildTeammateAddendum(args.teamName, args.memberName, nil)

	// 不传初始提示词：Lead 已在启动前把首条消息写入邮箱，循环第一次空闲轮询时
	// 会取到它。传入空字符串可避免 RunInProcessTeammate 重复注入用户消息。
	fmt.Fprintf(os.Stderr, "[teammate %s/%s] booted, awaiting tasks\n", args.teamName, args.memberName)
	return teams.RunInProcessTeammate(ctx, team, member, "", addendum, streamEventsToStderr())
}

// teammateToolOptions 汇总组装队友工具集需要的外部依赖。
type teammateToolOptions struct {
	WorkDir    string
	Protocol   string
	SessionID  string
	TeamMgr    *teams.TeamManager
	TeamName   string
	MemberName string
	MCPServers []config.MCPServerConfig
	// BaseURL 和 ContextWindow 供 MCP 加载模式分流用：判官方端点看前者，
	// 判 schema 体量占比看后者。
	BaseURL       string
	ContextWindow int
}

// buildTeammateRegistry 组装队友工具集：文件与命令工具、工具检索、Worktree
// 切换、MCP 扩展，再加上团队协作工具（按自己的名字发消息，以及读写团队共享
// 任务板）。任务板按团队名解析到同一份 tasks.json，所以队友之间看到的是同一
// 张表。
//
// Agent 不在其中，调用树到队友这一层为止，队友不再往下派子 Agent。
// TeamCreate 与 TeamDelete 也不在其中，组建和解散团队是 Lead 的职责。
//
// Skill 工具需要 Agent 实例充当宿主，由调用方在 Agent 建好之后单独接入。
func buildTeammateRegistry(ctx context.Context, opts teammateToolOptions) *tools.Registry {
	registry := tools.CreateDefaultToolsWithWorkDir(opts.WorkDir).Registry

	registry.Register(&tools.ToolSearchTool{Registry: registry, Protocol: opts.Protocol})
	registry.Register(&tools.McpCallTool{Registry: registry})
	registry.Register(&tools.SyntheticOutputTool{})

	gitRoot := worktree.FindCanonicalGitRoot(opts.WorkDir)
	registry.Register(&tools.EnterWorktreeTool{SessionID: opts.SessionID, RepoRoot: gitRoot})
	registry.Register(&tools.ExitWorktreeTool{RepoRoot: gitRoot})

	registry.Register(&teams.SendMessageTool{TeamMgr: opts.TeamMgr, SenderName: opts.MemberName})
	registry.Register(&teams.TaskCreateTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName, AgentName: opts.MemberName})
	registry.Register(&teams.TaskGetTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName})
	registry.Register(&teams.TaskListTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName})
	registry.Register(&teams.TaskUpdateTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName})

	if len(opts.MCPServers) > 0 {
		mgr := mcp.NewManager()
		serverConfigs := make([]mcp.ServerConfig, 0, len(opts.MCPServers))
		for _, c := range opts.MCPServers {
			serverConfigs = append(serverConfigs, mcp.ServerConfig{
				Name:      c.Name,
				Command:   c.Command,
				Args:      c.Args,
				URL:       c.URL,
				Transport: c.Transport,
				Headers:   c.Headers,
				Env:       c.Env,
			})
		}
		mgr.LoadConfigs(serverConfigs)
		mgr.RegisterAllTools(ctx, registry)
		// 工具都在位了才算得准 schema 总量跟上下文窗口的比例
		mcp.DecideAndApply(registry, opts.BaseURL, opts.ContextWindow)
	}

	return registry
}

// streamEventsToStderr 返回一个把每个 Agent 事件以可读形式转发到标准错误的通道。
// 工作进程没有 TUI，因此需要用它在 tmux/iTerm 面板中展示运行情况。
func streamEventsToStderr() chan<- agent.AgentEvent {
	ch := make(chan agent.AgentEvent, 32)
	go func() {
		for ev := range ch {
			switch e := ev.(type) {
			case agent.StreamText:
				fmt.Fprint(os.Stderr, e.Text)
			case agent.ToolUseEvent:
				fmt.Fprintf(os.Stderr, "\n[tool %s]\n", e.ToolName)
			case agent.ToolResultEvent:
				summary := strings.TrimSpace(e.Output)
				if len(summary) > 200 {
					summary = summary[:200] + "..."
				}
				fmt.Fprintf(os.Stderr, "[result] %s\n", summary)
			case agent.ErrorEvent:
				fmt.Fprintf(os.Stderr, "[error] %s\n", e.Message)
			case agent.LoopComplete:
				fmt.Fprintf(os.Stderr, "[turn done after %d steps]\n", e.TotalTurns)
			}
		}
	}()
	return ch
}

// conversation 只通过 Member.Conv 间接引用时，用此声明避免未使用导入错误。
var _ = conversation.NewManager
