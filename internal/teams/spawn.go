package teams

import (
	"context"
	"fmt"
	"os"
	"strings"

	"ccode/internal/agent"
	"ccode/internal/llm"
	"ccode/internal/permissions"
	"ccode/internal/tools"
)

// TeammateSpawnConfig 汇总 SpawnTeammate 所需的全部参数。各字段在不同后端中的用法：
// Team、MemberName、Task、Addendum：始终使用。
// Client、Registry、Protocol：仅供进程内后端使用；外部后端由生成的进程从自身
// 配置中加载这些值。
// Workdir：可选的工作目录覆盖值。非空时，进程内成员的 Agent.WorkDir 会指向该目录，
// tmux/iTerm 也会先 cd 到该路径。它用于隔离工作树，避免并发队员争用文件。
type TeammateSpawnConfig struct {
	Team       *Team
	MemberName string
	Task       string
	Addendum   string

	Client   llm.Client
	Registry *tools.Registry
	Protocol string

	Workdir string

	// Checker 是队友的权限检查器。Lead 派人时标了 plan_mode_required，
	// 这里就是一个 ModePlan 的 checker，队友只能读不能改，直到计划获批。
	Checker *permissions.Checker
}

// SpawnResult 保存 SpawnTeammate 针对不同后端返回的句柄。进程内启动会得到事件通道；
// tmux/iTerm 启动会得到窗格句柄，该句柄也存入 Member.PaneID 供后续关闭使用。
type SpawnResult struct {
	Mode    TeamMode
	EventCh <-chan agent.AgentEvent // 仅用于进程内后端
	PaneID  string                  // 仅用于 tmux/iTerm 后端
}

// SpawnTeammate 创建新团队成员，并使用团队当前选择的后端（Team.Mode）启动它。它是
// Agent 工具 team_name 路径使用的唯一入口；下方逻辑负责按后端分派。
//
// 对于外部后端，队员的初始任务会在新进程启动前通过邮箱投递，使队员在第一次空闲
// 轮询时即可看到任务。
func SpawnTeammate(ctx context.Context, cfg TeammateSpawnConfig) (*SpawnResult, error) {
	if cfg.Team == nil {
		return nil, fmt.Errorf("SpawnTeammate: team is required")
	}
	if cfg.MemberName == "" {
		return nil, fmt.Errorf("SpawnTeammate: member name is required")
	}

	// 把成员名字登记到全局名称注册表，供 SendMessage 按名字解析投递
	GetNameRegistry().Register(cfg.MemberName, cfg.MemberName)

	switch cfg.Team.Mode {
	case ModeInProcess:
		ch := StartInProcessMember(
			ctx,
			cfg.Team,
			cfg.MemberName,
			cfg.Client,
			cfg.Registry,
			cfg.Protocol,
			cfg.Task,
			cfg.Addendum,
		)
		// 把 Workdir 应用于刚注册成员的 Agent，使所有文件和 Bash 工具都相对于隔离
		// 路径解析。
		if m, ok := cfg.Team.Members[cfg.MemberName]; ok && m.AgentRef != nil {
			if cfg.Workdir != "" {
				m.AgentRef.WorkDir = cfg.Workdir
			}
			m.AgentRef.Checker = cfg.Checker
		}
		return &SpawnResult{Mode: ModeInProcess, EventCh: ch}, nil

	case ModeTmux:
		// 外部进程通过邮箱领取任务。启动前先写入初始任务，使新进程第一次轮询时即可
		// 看到工作。
		if cfg.Task != "" {
			_ = cfg.Team.MailBox.Send(cfg.MemberName, FileMailMessage{
				From: LeadName,
				Text: cfg.Task,
			})
		}
		cliCommand, err := BuildTeammateCLI(cfg.Team.Name, cfg.MemberName, cfg.Workdir)
		if err != nil {
			return nil, err
		}
		paneID, err := spawnTmuxTeammate(cfg.Team.Name, cfg.MemberName, cliCommand)
		if err != nil {
			return nil, err
		}
		cfg.Team.recordExternalMember(cfg.MemberName, paneID)
		return &SpawnResult{Mode: ModeTmux, PaneID: paneID}, nil

	case ModeITerm:
		if cfg.Task != "" {
			_ = cfg.Team.MailBox.Send(cfg.MemberName, FileMailMessage{
				From: LeadName,
				Text: cfg.Task,
			})
		}
		cliCommand, err := BuildTeammateCLI(cfg.Team.Name, cfg.MemberName, cfg.Workdir)
		if err != nil {
			return nil, err
		}
		tabID, err := spawnITermTeammate(cfg.Team.Name, cfg.MemberName, cliCommand)
		if err != nil {
			return nil, err
		}
		cfg.Team.recordExternalMember(cfg.MemberName, tabID)
		return &SpawnResult{Mode: ModeITerm, PaneID: tabID}, nil
	}

	return nil, fmt.Errorf("unknown team mode: %s", cfg.Team.Mode)
}

// recordExternalMember 把由 tmux/iTerm 启动的队员注册进内存 Members 映射，使
// StopMember 后续可以准确关闭它。外部队员在当前进程中没有 AgentRef——其 LLM 位于
// 启动出的进程中——因此该条目只是供 Lead 协调工具使用的名称和句柄存根。
func (t *Team) recordExternalMember(name, paneID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Members[name] = &Member{
		Name:   name,
		Active: true,
		PaneID: paneID,
	}
}

// BuildTeammateCLI 返回一条 shell 命令；在新的终端窗格或标签页中运行它，会为指定
// 团队和成员以队员模式启动当前 ccode 二进制文件。workdir 参数控制生成进程的运行
// 目录；传入 "" 时回退到 Lead 的当前目录，使邮箱路径解析保持一致。非空 workdir
// 主要用于工作树隔离。
//
// 输出格式与 cmd/ccode/main.go 队员模式分支所解析的命令行一致。
func BuildTeammateCLI(teamName, memberName, workdir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate ccode binary: %w", err)
	}
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	return fmt.Sprintf(
		"cd %s && %s --teammate --team-name %s --agent-name %s",
		shellQuote(workdir),
		shellQuote(exe),
		shellQuote(teamName),
		shellQuote(memberName),
	), nil
}

// shellQuote 包裹值，使其可安全放入 /bin/sh -c 参数。由于 tmux send-keys 和 osascript
// `write text` 都会通过 shell 解释字符串，因此这里使用单引号转义。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
