package main

import (
	"context"
	"testing"

	"ccode/internal/teams"
)

func TestParseTeammateFlagsAbsent(t *testing.T) {
	cases := [][]string{
		{},
		{"--help"},
		{"--something", "else"},
	}
	for _, args := range cases {
		if _, ok := parseTeammateFlags(args); ok {
			t.Errorf("parseTeammateFlags(%v) returned ok=true, want false", args)
		}
	}
}

func TestParseTeammateFlagsBasic(t *testing.T) {
	args := []string{"--teammate", "--team-name", "alpha", "--agent-name", "ann"}
	got, ok := parseTeammateFlags(args)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.teamName != "alpha" || got.memberName != "ann" {
		t.Errorf("parsed = %+v", got)
	}
}

func TestParseTeammateFlagsMissingValue(t *testing.T) {
	// 末尾标志缺少值时不应触发 panic，只需让字段保持为空，
	// 以便 runTeammate 返回友好的错误信息。
	args := []string{"--teammate", "--team-name"}
	got, ok := parseTeammateFlags(args)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.teamName != "" {
		t.Errorf("expected empty teamName, got %q", got.teamName)
	}
}

func TestParseTeammateFlagsIgnoresUnknown(t *testing.T) {
	args := []string{"--teammate", "--noise", "x", "--team-name", "t", "--agent-name", "m"}
	got, ok := parseTeammateFlags(args)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.teamName != "t" || got.memberName != "m" {
		t.Errorf("parsed = %+v", got)
	}
}

// 队友进程自己组装工具集，和进程内队员那份很容易各改各的，所以这里把清单钉死：
// 协作工具必须在，团队管理和子 Agent 必须不在。
func TestBuildTeammateRegistryToolSet(t *testing.T) {
	reg := buildTeammateRegistry(context.Background(), teammateToolOptions{
		WorkDir:    t.TempDir(),
		Protocol:   "anthropic",
		SessionID:  "sess-1",
		TeamMgr:    teams.NewTeamManager(),
		TeamName:   "alpha",
		MemberName: "ann",
	})

	// 干活的工具、通用能力，以及队友之间协作要用的消息和共享任务板
	mustHave := []string{
		"ReadFile", "WriteFile", "EditFile", "Bash", "Glob", "Grep",
		"ToolSearch", "SyntheticOutput", "EnterWorktree", "ExitWorktree",
		"SendMessage", "TaskCreate", "TaskGet", "TaskList", "TaskUpdate",
	}
	for _, name := range mustHave {
		if reg.Get(name) == nil {
			t.Errorf("队友工具集缺少 %s", name)
		}
	}

	// 派人和建团队是 Lead 的职责，队友拿不到
	mustNotHave := []string{"Agent", "TeamCreate", "TeamDelete"}
	for _, name := range mustNotHave {
		if reg.Get(name) != nil {
			t.Errorf("队友工具集不应包含 %s", name)
		}
	}
}
