package skills

import (
	"strings"
)

type SkillMeta struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	WhenToUse   string   `yaml:"when_to_use"`
	Tags        []string `yaml:"tags"`
	// Mode 选择执行模式。"inline"（默认）把 Skill 正文注入当前对话；"fork" 在具有
	// 隔离上下文的子 Agent 中运行 Skill 正文。
	Mode string `yaml:"mode"`
	// Model 覆盖此 Skill 使用的 LLM。为空时继承主循环。
	Model string `yaml:"model"`
	// Context 为兼容使用 `context: fork` 表示 Mode=fork 的旧 Skill 而保留。值为
	// "fork" 时按 fork 模式处理。
	Context string `yaml:"context"`
	// ForkContext 控制把多少父对话内容带入 fork 出的子 Agent。仅当 Mode == "fork" 时
	// 有意义。可选值："full"（父对话的 LLM 摘要）、"recent"（最后 5 条消息）、
	// "none"（不带父上下文，默认）。
	ForkContext string `yaml:"fork_context"`
}

// IsFork 报告 Skill 是否应以 fork 模式运行。为保持向后兼容，它会同时检查 Mode 和
// 旧版 Context 字段。
func (m SkillMeta) IsFork() bool {
	return m.Mode == "fork" || m.Context == "fork"
}

type Skill struct {
	Meta       SkillMeta
	PromptBody string
	SourceDir  string
	// IsDirectory 标记 SourceDir 中含有额外资源（references/、scripts/）的 Skill。
	// 对磁盘上与 SKILL.md 放在一起并包含支持文件的目录型 Skill 为 true；仅对运行时
	// 没有真实磁盘目录可访问的内嵌 Skill 为 false。
	IsDirectory bool
	// BodyLoaded 标记是否已从磁盘读取 PromptBody。第一阶段只读取 frontmatter；
	// 在 GetFull 触发读取之前，正文保持为空。
	BodyLoaded bool
}

// Render 返回替换了 $ARGUMENTS 的 Skill 正文。如果正文没有 $ARGUMENTS 占位符且
// args 非空，就把参数追加到 "## User Request" 小节。
//
// 执行方式由 Meta.Mode 决定，不体现在渲染结果里：inline 的正文直接进主对话，
// fork 的正文由调用方交给隔离子 Agent，两条路径拿到的都是这里渲染出的同一份文本。
func (s *Skill) Render(args string) string {
	body := s.PromptBody
	if strings.Contains(body, "$ARGUMENTS") {
		return strings.ReplaceAll(body, "$ARGUMENTS", args)
	}
	if strings.TrimSpace(args) == "" {
		return body
	}
	return body + "\n\n## User Request\n\n" + args
}
