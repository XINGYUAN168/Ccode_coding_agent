package compact

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// 以下常量限制追加到摘要消息的恢复附件大小。压缩会清空工作对话；若没有这些快照，
// 模型会忘记刚刚读取的文件以及正在遵循的 Skill SOP。
const (
	RecoveryFileLimit      = 5
	RecoveryTokensPerFile  = 5_000
	RecoverySkillsBudget   = 25_000
	RecoveryTokensPerSkill = 5_000
	recoveryCharsPerToken  = 3.5
)

// FileReadRecord 保存 ReadFile 调用返回给模型的字节快照。压缩后会重新注入，
// 使模型仍能取得触发阈值时正在推理的内容。
type FileReadRecord struct {
	Path      string
	Content   string
	Timestamp time.Time
}

// SkillInvocationRecord 保存调用 Skill 时附带的 SOP 正文。压缩后会重新拼接同一定义，
// 使边界前后的行为保持一致。
type SkillInvocationRecord struct {
	Name      string
	Body      string
	Timestamp time.Time
}

// RecoveryState 跟踪每个 Agent 中需要跨压缩保留的数据。该结构支持并发记录，
// 因为流式执行器的工具回调可能来自并行 goroutine。
type RecoveryState struct {
	mu     sync.Mutex
	files  map[string]FileReadRecord
	skills map[string]SkillInvocationRecord
}

// NewRecoveryState 返回可立即记录数据的空状态。
func NewRecoveryState() *RecoveryState {
	return &RecoveryState{
		files:  map[string]FileReadRecord{},
		skills: map[string]SkillInvocationRecord{},
	}
}

// RecordFileRead 覆盖同一路径的旧记录，确保使用最新快照；nil 接收者调用也是安全的。
func (s *RecoveryState) RecordFileRead(path, content string) {
	if s == nil || path == "" {
		return
	}
	s.mu.Lock()
	s.files[path] = FileReadRecord{Path: path, Content: content, Timestamp: time.Now()}
	s.mu.Unlock()
}

// RecordSkillInvocation 覆盖同名 Skill 的旧记录；nil 接收者调用也是安全的。
func (s *RecoveryState) RecordSkillInvocation(name, body string) {
	if s == nil || name == "" {
		return
	}
	s.mu.Lock()
	s.skills[name] = SkillInvocationRecord{Name: name, Body: body, Timestamp: time.Now()}
	s.mu.Unlock()
}

// snapshotFiles 最多返回 `limit` 条记录，并按从新到旧排序。
func (s *RecoveryState) snapshotFiles(limit int) []FileReadRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FileReadRecord, 0, len(s.files))
	for _, r := range s.files {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// snapshotSkills 返回全部已记录 Skill，并按从新到旧排序。
func (s *RecoveryState) snapshotSkills() []SkillInvocationRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SkillInvocationRecord, 0, len(s.skills))
	for _, r := range s.skills {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out
}

// BuildRecoveryAttachment 把压缩后的恢复小节渲染成单个文本块，包括最近读取的文件、
// Skill 定义、工具清单，以及提醒不要根据摘要猜测的结尾说明。没有值得输出的内容时
// 返回 ""，让调用方保持摘要消息整洁。
func BuildRecoveryAttachment(state *RecoveryState, toolSchemas []map[string]any) string {
	var sb strings.Builder

	if files := state.snapshotFiles(RecoveryFileLimit); len(files) > 0 {
		sb.WriteString("## Recently read files\n\n")
		sb.WriteString("These snapshots are what the file-reading tool last returned. Re-open with the tool if you need the current bytes.\n\n")
		for _, f := range files {
			content := truncateByTokens(f.Content, RecoveryTokensPerFile)
			ts := f.Timestamp.UTC().Format("2006-01-02T15:04:05Z")
			fmt.Fprintf(&sb, "### %s  (read %s)\n\n", f.Path, ts)
			sb.WriteString("```\n")
			sb.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				sb.WriteByte('\n')
			}
			sb.WriteString("```\n\n")
		}
	}

	if skills := state.snapshotSkills(); len(skills) > 0 {
		var section strings.Builder
		section.WriteString("## Active skills\n\n")
		section.WriteString("These skills were invoked earlier in the session. Continue to follow each SOP when its triggering condition applies.\n\n")
		used := 0
		emitted := false
		for _, sk := range skills {
			body := truncateByTokens(sk.Body, RecoveryTokensPerSkill)
			tokens := approxTokens(body) + approxTokens(sk.Name) + 8
			if used+tokens > RecoverySkillsBudget {
				break
			}
			used += tokens
			fmt.Fprintf(&section, "### %s\n\n%s\n\n", sk.Name, body)
			emitted = true
		}
		if emitted {
			sb.WriteString(section.String())
		}
	}

	if len(toolSchemas) > 0 {
		sb.WriteString("## Available tools\n\nYou still have access to the following tools — call them directly when the task needs one:\n\n")
		for _, t := range toolSchemas {
			name, _ := t["name"].(string)
			if name == "" {
				continue
			}
			desc, _ := t["description"].(string)
			desc = firstLine(desc)
			if desc != "" {
				fmt.Fprintf(&sb, "- %s — %s\n", name, desc)
			} else {
				fmt.Fprintf(&sb, "- %s\n", name)
			}
		}
		sb.WriteString("\n")
	}

	if sb.Len() == 0 {
		return ""
	}

	sb.WriteString("## Note\n\nEverything above the divider is reconstructed context. For exact code, error strings, or user-typed text, re-read the source rather than guess from the summary.\n")
	return sb.String()
}

// approxTokens 使用与 EstimateTokens 相同的每 token 字符数启发式规则，
// 使整个包的预算计算保持一致。
func approxTokens(s string) int {
	if s == "" {
		return 0
	}
	return int(float64(len(s)) / recoveryCharsPerToken)
}

// truncateByTokens 在刚好低于 token 预算的字节偏移处截断 s，并追加标记，
// 让模型知道内容已被裁剪。
func truncateByTokens(s string, tokenBudget int) string {
	if tokenBudget <= 0 || s == "" {
		return s
	}
	if approxTokens(s) <= tokenBudget {
		return s
	}
	maxChars := int(float64(tokenBudget) * recoveryCharsPerToken)
	if maxChars <= 0 || maxChars >= len(s) {
		return s
	}
	return s[:maxChars] + "\n… (content truncated)"
}

// firstLine 返回 s 中第一条去除首尾空白后的非空行，用于在说明包含多段时保持工具清单紧凑。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
