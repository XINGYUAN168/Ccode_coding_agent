package extractor

import (
	"strings"
	"testing"
)

func TestBuildExtractAutoOnlyPromptMarkers(t *testing.T) {
	got := BuildExtractAutoOnlyPrompt(7, "", false, "/home/test/.ccode/memory/", "/tmp/proj/.ccode/memory/")
	for _, expect := range []string{
		"memory extraction subagent",
		"most recent ~7 messages",
		"Available tools: ReadFile, Grep, Glob, read-only Bash",
		"EditFile/WriteFile",
		"## Types of memory",
		"## What NOT to save in memory",
		"## How to save memories",
		"**Step 1**",
		"**Step 2** — add a pointer",
		"MEMORY.md",
	} {
		if !strings.Contains(got, expect) {
			t.Errorf("missing %q in prompt", expect)
		}
	}
}

func TestBuildExtractAutoOnlyPromptSkipIndex(t *testing.T) {
	got := BuildExtractAutoOnlyPrompt(5, "", true, "/home/test/.ccode/memory/", "/tmp/proj/.ccode/memory/")
	if strings.Contains(got, "**Step 2** — add a pointer") {
		t.Errorf("skipIndex=true should remove Step 2 / MEMORY.md update section")
	}
	if !strings.Contains(got, "Write each memory to its own file") {
		t.Errorf("skipIndex=true should still include single-step write instructions")
	}
}

func TestBuildExtractAutoOnlyPromptIncludesExistingManifest(t *testing.T) {
	manifest := "- [user] foo.md (2026-05-22T01:00:00.000Z): existing note"
	got := BuildExtractAutoOnlyPrompt(3, manifest, false, "/home/test/.ccode/memory/", "/tmp/proj/.ccode/memory/")
	if !strings.Contains(got, "## Existing memory files") {
		t.Errorf("manifest section header missing")
	}
	if !strings.Contains(got, "foo.md") {
		t.Errorf("manifest content not embedded: %s", got)
	}
	if !strings.Contains(got, "update an existing file rather than creating a duplicate") {
		t.Errorf("dedup guidance missing")
	}
}

func TestBuildExtractAutoOnlyPromptNoTeamSection(t *testing.T) {
	// 双路径模式使用 <scope> 标签路由用户级和项目级记忆，但自动记忆提示词绝不能出现
	// “团队记忆”或“私有/团队”说明；Ccode 只保留简单的用户/项目划分，不引入团队记忆概念。
	got := BuildExtractAutoOnlyPrompt(3, "", false, "/home/test/.ccode/memory/", "/tmp/proj/.ccode/memory/")
	for _, banned := range []string{
		"team memor",
		"private or team",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("dual-path prompt must not contain %q", banned)
		}
	}
}

func TestBuildExtractAutoOnlyPromptIncludesGuardrails(t *testing.T) {
	got := BuildExtractAutoOnlyPrompt(3, "", false, "/home/test/.ccode/memory/", "/tmp/proj/.ccode/memory/")
	for _, expect := range []string{
		"MCP, Agent, write-capable Bash, etc — will be denied",
		"Do not interleave reads and writes",
		"no grepping source files",
		"forget something, find and remove",
	} {
		if !strings.Contains(got, expect) {
			t.Errorf("missing guardrail %q", expect)
		}
	}
}
