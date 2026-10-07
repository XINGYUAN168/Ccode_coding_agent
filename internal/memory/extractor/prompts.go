// Package extractor 实现后台记忆提取子 Agent。
//
// 提取 Agent 以主对话的完整 fork 运行，使用相同系统提示词和消息前缀。主 Agent 的系统提示词
// 始终包含完整保存说明；主 Agent 自行写入记忆时，extractor.Execute 会通过
// hasMemoryWritesSince 跳过该轮。因此此提示词只在主 Agent 未写入时触发，与系统提示词中的
// 保存条件重复也不会产生影响。
package extractor

import (
	"fmt"
	"strings"

	"ccode/internal/memory"
)

// 提取提示词使用的工具名称。它们与 Ccode 工具注册表一致，首字母大写且不含下划线；
// 这些字符串会写入提示词，确保模型发出注册表能够识别的调用。
const (
	bashToolName      = "Bash"
	fileEditToolName  = "EditFile"
	fileReadToolName  = "ReadFile"
	fileWriteToolName = "WriteFile"
	globToolName      = "Glob"
	grepToolName      = "Grep"
)

// opener 是两种提取提示词变体共用的开头部分。
func opener(newMessageCount int, existingMemories string) string {
	manifest := ""
	if existingMemories != "" {
		manifest = fmt.Sprintf(
			"\n\n## Existing memory files\n\n%s\n\nCheck this list before writing — update an existing file rather than creating a duplicate.",
			existingMemories)
	}
	return strings.Join([]string{
		fmt.Sprintf("You are now acting as the memory extraction subagent. Analyze the most recent ~%d messages above and use them to update your persistent memory systems.", newMessageCount),
		"",
		fmt.Sprintf("Available tools: %s, %s, %s, read-only %s (ls/find/cat/stat/wc/head/tail and similar), and %s/%s for paths inside the memory directory only. %s rm is not permitted. All other tools — MCP, Agent, write-capable %s, etc — will be denied.",
			fileReadToolName, grepToolName, globToolName, bashToolName,
			fileEditToolName, fileWriteToolName,
			bashToolName, bashToolName),
		"",
		fmt.Sprintf("You have a limited turn budget. %s requires a prior %s of the same file, so the efficient strategy is: turn 1 — issue all %s calls in parallel for every file you might update; turn 2 — issue all %s/%s calls in parallel. Do not interleave reads and writes across multiple turns.",
			fileEditToolName, fileReadToolName, fileReadToolName, fileWriteToolName, fileEditToolName),
		"",
		fmt.Sprintf("You MUST only use content from the last ~%d messages to update your persistent memories. Do not waste any turns attempting to investigate or verify that content further — no grepping source files, no reading code to confirm a pattern exists, no git commands.%s",
			newMessageCount, manifest),
	}, "\n")
}

// BuildExtractAutoOnlyPrompt 构造仅自动记忆、不含团队记忆的提取提示词。
// 四类记忆使用双路径作用域：用户/反馈记忆位于 userMemDir，项目/参考记忆位于 projectMemDir。
//
// userMemDir 可能为空，例如无法解析用户目录时；此时提示词退化为仅项目路由，
// 用户/反馈类型实际上不可用。
func BuildExtractAutoOnlyPrompt(newMessageCount int, existingMemories string, skipIndex bool, userMemDir, projectMemDir string) string {
	var routing strings.Builder
	routing.WriteString("## Memory storage paths\n\n")
	if userMemDir != "" {
		fmt.Fprintf(&routing, "- `user` and `feedback` type → write to `%s` (user-level; follows the human across projects)\n", userMemDir)
	}
	if projectMemDir != "" {
		fmt.Fprintf(&routing, "- `project` and `reference` type → write to `%s` (project-level; lives with this repo)\n", projectMemDir)
	}
	routing.WriteString("\nPick the type first, then write the memory file (and its MEMORY.md pointer) into the matching directory. Never write a `user`/`feedback` memory into the project-level dir or vice versa.")

	var howToSave []string
	if skipIndex {
		howToSave = []string{
			"## How to save memories",
			"",
			"Write each memory to its own file (e.g., `user_role.md`, `feedback_testing.md`) using this frontmatter format:",
			"",
			memory.MemoryFrontmatterExample,
			"",
			"- Organize memory semantically by topic, not chronologically",
			"- Update or remove memories that turn out to be wrong or outdated",
			"- Do not write duplicate memories. First check if there is an existing memory you can update before writing a new one.",
		}
	} else {
		howToSave = []string{
			"## How to save memories",
			"",
			"Saving a memory is a two-step process:",
			"",
			"**Step 1** — write the memory to its own file (e.g., `user_role.md`, `feedback_testing.md`) in the directory that matches its `type` (see Memory storage paths above), using this frontmatter format:",
			"",
			memory.MemoryFrontmatterExample,
			"",
			"**Step 2** — add a pointer to that file in the `MEMORY.md` index in the SAME directory. `MEMORY.md` is an index, not a memory — each entry should be one line, under ~150 characters: `- [Title](file.md) — one-line hook`. It has no frontmatter. Never write memory content directly into `MEMORY.md`.",
			"",
			"- Both `MEMORY.md` files (user-level and project-level) are loaded into context — lines after 200 each will be truncated, so keep each index concise",
			"- Organize memory semantically by topic, not chronologically",
			"- Update or remove memories that turn out to be wrong or outdated",
			"- Do not write duplicate memories. First check if there is an existing memory you can update before writing a new one.",
		}
	}

	parts := []string{
		opener(newMessageCount, existingMemories),
		"",
		"If the user explicitly asks you to remember something, save it immediately as whichever type fits best. If they ask you to forget something, find and remove the relevant entry.",
		"",
		routing.String(),
		"",
		memory.TypesSectionDualPath,
		memory.WhatNotToSaveSection,
		"",
	}
	parts = append(parts, howToSave...)
	return strings.Join(parts, "\n")
}
