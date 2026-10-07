package memory

import (
	"fmt"
	"os"
	"strings"
)

// 构造类型化记忆行为提示词和 MEMORY.md 入口内容，供系统提示词引用。

const (
	// MaxEntrypointLines 限制载入上下文的 MEMORY.md 内容量。每行约 125 个字符 × 200 行，
	// 对应当前观测到的 p97。
	MaxEntrypointLines = 200
	// MaxEntrypointBytes 限制绕过行数上限的超长行索引；观测到的 p100 是不足 200 行但达 197KB。
	MaxEntrypointBytes = 25_000

	autoMemDisplayName = "auto memory"

	// DirExistsGuidance 用于避免模型在写入前浪费轮次执行 `ls` 或 `mkdir -p`。
	// 运行框架通过 EnsureMemoryDirExists 保证目录存在，因此提示词可以作出该保证。
	DirExistsGuidance = "This directory already exists — write to it directly with the Write tool (do not run mkdir or check for its existence)."
)

// EntrypointTruncation 是限制 MEMORY.md 内容后的结果。
type EntrypointTruncation struct {
	Content          string
	LineCount        int
	ByteCount        int
	WasLineTruncated bool
	WasByteTruncated bool
}

// TruncateEntrypointContent 同时按行数和字节数限制 MEMORY.md，并追加说明触发了哪项上限的警告。
// 它先按自然边界截断行，再在字节上限前的最后一个换行处截断，避免切断一行。
func TruncateEntrypointContent(raw string) EntrypointTruncation {
	trimmed := strings.TrimSpace(raw)
	contentLines := strings.Split(trimmed, "\n")
	lineCount := len(contentLines)
	byteCount := len(trimmed)

	wasLineTruncated := lineCount > MaxEntrypointLines
	wasByteTruncated := byteCount > MaxEntrypointBytes

	if !wasLineTruncated && !wasByteTruncated {
		return EntrypointTruncation{
			Content:          trimmed,
			LineCount:        lineCount,
			ByteCount:        byteCount,
			WasLineTruncated: wasLineTruncated,
			WasByteTruncated: wasByteTruncated,
		}
	}

	truncated := trimmed
	if wasLineTruncated {
		truncated = strings.Join(contentLines[:MaxEntrypointLines], "\n")
	}

	if len(truncated) > MaxEntrypointBytes {
		cutAt := strings.LastIndex(truncated[:MaxEntrypointBytes], "\n")
		if cutAt > 0 {
			truncated = truncated[:cutAt]
		} else {
			// 整段没有换行只能硬切，此时要回退到字符边界：UTF-8 的后续字节高两位
			// 固定是 10，从截断点往前跳过它们才不会把一个汉字劈成两半
			end := MaxEntrypointBytes
			for end > 0 && truncated[end]&0xC0 == 0x80 {
				end--
			}
			truncated = truncated[:end]
		}
	}

	var reason string
	switch {
	case wasByteTruncated && !wasLineTruncated:
		reason = fmt.Sprintf("%s (limit: %s) — index entries are too long",
			formatFileSize(byteCount), formatFileSize(MaxEntrypointBytes))
	case wasLineTruncated && !wasByteTruncated:
		reason = fmt.Sprintf("%d lines (limit: %d)", lineCount, MaxEntrypointLines)
	default:
		reason = fmt.Sprintf("%d lines and %s", lineCount, formatFileSize(byteCount))
	}

	return EntrypointTruncation{
		Content: truncated + fmt.Sprintf(
			"\n\n> WARNING: %s is %s. Only part of it was loaded. Keep index entries to one line under ~200 chars; move detail into topic files.",
			AutoMemEntrypointName, reason),
		LineCount:        lineCount,
		ByteCount:        byteCount,
		WasLineTruncated: wasLineTruncated,
		WasByteTruncated: wasByteTruncated,
	}
}

func formatFileSize(bytes int) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(bytes)/1024/1024)
	}
}

// EnsureMemoryDirExists 创建记忆目录及其父目录，使模型无需先运行 ls/mkdir 即可写入。
// 此操作可重复执行。失败不是致命错误，提示词构造器仍会继续；如果确有权限问题，
// 模型的 Write 操作会显示真实错误。
func EnsureMemoryDirExists(memoryDir string) error {
	if memoryDir == "" {
		return nil
	}
	return os.MkdirAll(memoryDir, 0o755)
}

// BuildMemoryLines 构造类型化记忆行为说明，不包含 MEMORY.md 内容。记忆被限制为封闭的四类：
// user、feedback、project、reference；明确排除可从当前项目状态推导出的代码模式、架构、
// Git 历史等内容。
//
// 双路径变体：user/feedback 记忆写入 userMemDir，随用户跨项目保留；
// project/reference 记忆写入 projectMemDir，与仓库共存。任一目录都可以为空，
// 此时对应的一半记忆类型会被隐式禁用。
func BuildMemoryLines(displayName, userMemDir, projectMemDir string) string {
	howToSave := `## How to save memories

Saving a memory is a two-step process:

**Step 1** — write the memory to its own file (e.g., ` + "`user_role.md`" + `, ` + "`feedback_testing.md`" + `) using this frontmatter format:

` + MemoryFrontmatterExample + `

**Step 2** — add a pointer to that file in the ` + "`" + AutoMemEntrypointName + "`" + ` index in the SAME directory as the memory file. ` + "`" + AutoMemEntrypointName + "`" + ` is an index, not a memory — each entry should be one line, under ~150 characters: ` + "`- [Title](file.md) — one-line hook`" + `. It has no frontmatter. Never write memory content directly into ` + "`" + AutoMemEntrypointName + "`" + `.

- Both ` + "`" + AutoMemEntrypointName + "`" + ` files (user-level and project-level) are always loaded into your conversation context` + fmt.Sprintf(" — lines after %d each will be truncated, so keep each index concise", MaxEntrypointLines) + `
- Keep the name, description, and type fields in memory files up-to-date with the content
- Organize memory semantically by topic, not chronologically
- Update or remove memories that turn out to be wrong or outdated
- Do not write duplicate memories. First check if there is an existing memory you can update before writing a new one.`

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", displayName)
	b.WriteString("You have a persistent, file-based memory system organized into two locations by content type:\n\n")
	if userMemDir != "" {
		fmt.Fprintf(&b, "- **User-level** (`%s`) — memories with `type: user` or `type: feedback`. These follow you across all projects, because they describe the human or how the human likes to work. %s\n", userMemDir, DirExistsGuidance)
	}
	if projectMemDir != "" {
		fmt.Fprintf(&b, "- **Project-level** (`%s`) — memories with `type: project` or `type: reference`. These belong to the current repo, can be committed for team sharing or git-ignored for personal use. %s\n", projectMemDir, DirExistsGuidance)
	}
	b.WriteString("\nThe `type` field in each memory file's frontmatter determines which directory it belongs to — pick the type first, then write to the matching directory.\n\n")
	b.WriteString("You should build up this memory system over time so that future conversations can have a complete picture of who the user is, how they'd like to collaborate with you, what behaviors to avoid or repeat, and the context behind the work the user gives you.\n\n")
	b.WriteString("If the user explicitly asks you to remember something, save it immediately as whichever type fits best (and in whichever directory that type belongs to). If they ask you to forget something, find and remove the relevant entry.\n\n")
	b.WriteString(TypesSectionDualPath)
	b.WriteByte('\n')
	b.WriteString(WhatNotToSaveSection)
	b.WriteString("\n\n")
	b.WriteString(howToSave)
	b.WriteString("\n\n")
	b.WriteString(WhenToAccessSection)
	b.WriteString("\n\n")
	b.WriteString(TrustingRecallSection)
	b.WriteString("\n\n")
	b.WriteString("## Memory and other forms of persistence\n")
	b.WriteString("Memory is one of several persistence mechanisms available to you as you assist the user in a given conversation. The distinction is often that memory can be recalled in future conversations and should not be used for persisting information that is only useful within the scope of the current conversation.\n")
	b.WriteString("- When to use or update a plan instead of memory: If you are about to start a non-trivial implementation task and would like to reach alignment with the user on your approach you should use a Plan rather than saving this information to memory. Similarly, if you already have a plan within the conversation and you have changed your approach persist that change by updating the plan rather than saving a memory.\n")
	b.WriteString("- When to use or update tasks instead of memory: When you need to break your work in current conversation into discrete steps or keep track of your progress use tasks instead of saving to memory. Tasks are great for persisting information about the work that needs to be done in the current conversation, but memory should be reserved for information that will be useful in future conversations.")

	return b.String()
}

// BuildMemoryPrompt 构造类型化记忆提示词，并包含用户级和项目级 MEMORY.md 索引。
// 系统提示词组装器使用它，让 Agent 在同一小节获得行为规则和当前索引。
func BuildMemoryPrompt(displayName, userMemDir, projectMemDir string) string {
	lines := BuildMemoryLines(displayName, userMemDir, projectMemDir)

	var b strings.Builder
	b.WriteString(lines)
	b.WriteString("\n\n")
	if userMemDir != "" {
		writeEntrypointSection(&b, "User-level", userMemDir+AutoMemEntrypointName)
	}
	if projectMemDir != "" {
		if userMemDir != "" {
			b.WriteString("\n\n")
		}
		writeEntrypointSection(&b, "Project-level", projectMemDir+AutoMemEntrypointName)
	}
	return b.String()
}

func writeEntrypointSection(b *strings.Builder, scopeLabel, entrypointPath string) {
	fmt.Fprintf(b, "## %s %s (`%s`)\n\n", scopeLabel, AutoMemEntrypointName, entrypointPath)
	data, err := os.ReadFile(entrypointPath)
	if err == nil && strings.TrimSpace(string(data)) != "" {
		t := TruncateEntrypointContent(string(data))
		b.WriteString(t.Content)
	} else {
		fmt.Fprintf(b, "This %s is currently empty. When you save new %s-level memories, add their pointers here.", AutoMemEntrypointName, strings.ToLower(scopeLabel))
	}
}

// LoadAutoMemoryPrompt 是系统提示词构造器使用的便捷入口。它确保用户级和项目级记忆目录
// 存在，再返回可直接作为系统提示词小节的 `# auto memory` 组装结果。
func LoadAutoMemoryPrompt(projectRoot string) string {
	userDir := GetUserAutoMemPath()
	projectDir := GetAutoMemPath(projectRoot)
	if userDir == "" && projectDir == "" {
		return ""
	}
	if userDir != "" {
		_ = EnsureMemoryDirExists(userDir)
	}
	if projectDir != "" {
		_ = EnsureMemoryDirExists(projectDir)
	}
	return BuildMemoryPrompt(autoMemDisplayName, userDir, projectDir)
}
