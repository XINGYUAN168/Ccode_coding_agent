package skills

import (
	"context"

	"ccode/internal/conversation"
	"ccode/internal/tools"
)

// SkillHost 是 Executor 驱动 inline 模式 Skill 所需的 Agent 状态切面。它由
// *agent.Agent 实现；此处声明为接口，使 skills 包无需导入 agent 包（LoadSkillTool
// 开始引用 skills.Catalog 后，这样做会形成循环依赖）。
type SkillHost interface {
	// ActivateSkill 记录 Skill 激活状态，供追踪（/skills 列表和压缩恢复）使用。
	// 正文不会在每一轮重复注入。
	ActivateSkill(name, body string)
	// ToolRegistry 暴露正在使用的 tools.Registry，使执行器可以注册目录型工具。
	// 此方法命名为 ToolRegistry（而非 Registry），因为 *agent.Agent 已有导出的
	// Registry 字段，而 Go 禁止方法与字段重名。
	ToolRegistry() *tools.Registry
}

// SkillForkHost 在 SkillHost 基础上增加运行隔离子 Agent 的能力。它由持有 LLM
// 客户端和 Agent 构造器的 TUI 层实现，并传给 Executor.RunFork。将它与 SkillHost
// 分开后，单元测试可以只替换 fork 行为，而不用伪造完整的子 Agent 运行时。
type SkillForkHost interface {
	SkillHost
	// RunSubAgent 在以 `seed` 初始化的新对话中，把 `body` 作为第一条用户消息运行，
	// 并返回最终的助手文本。`seed` 已按 ForkContext 策略准备；取消 ctx 应终止子 Agent。
	RunSubAgent(ctx context.Context, body string, seed []conversation.Message, model string) (string, error)
	// SnapshotParentMessages 暴露父对话消息，使执行器可以按 `fork_context` 构建
	// 初始消息。实现可以返回浅拷贝；执行器不得修改该切片。
	SnapshotParentMessages() []conversation.Message
}

// RunInline 在宿主 Agent 上记录 Skill 激活状态，并返回渲染后的提示词正文。调用方
// （斜杠命令处理器）把返回的正文作为用户消息提交到主对话——它会作为普通消息保留，
// 而不会在每一轮重复注入。
func RunInline(_ context.Context, skill *Skill, args string, host SkillHost) (string, error) {
	body := skill.Render(args)
	host.ActivateSkill(skill.Meta.Name, body)
	return body, nil
}

// RunFork 在隔离的子 Agent 中执行 Skill，并返回最终的助手文本。子 Agent 不会修改
// 主对话；调用方（斜杠命令处理器）应把返回的字符串作为助手消息插入主聊天记录。
//
// 历史记录的继承方式由 skill.Meta.ForkContext 选择：
//   - "full"：用父对话的完整消息历史初始化子 Agent
//   - "recent"：用父对话最后 5 条消息初始化
//   - "none"：不提供初始消息（默认；像新会话一样隔离）
func RunFork(ctx context.Context, skill *Skill, args string, host SkillForkHost) (string, error) {
	body := skill.Render(args)
	seed := buildForkSeed(skill.Meta.ForkContext, host.SnapshotParentMessages())
	return host.RunSubAgent(ctx, body, seed, skill.Meta.Model)
}

// buildForkSeed 按 ForkContext 策略截取父对话消息历史。对于 "none" 或未知值返回
// nil，使子 Agent 从全新上下文开始。
//
// "full" 当前不会通过 LLM 生成摘要——它会原样复制父消息切片。如果上下文窗口将来
// 成为问题，可以通过 compact.Summarise 处理；目前采用与 "recent" 相同但上限更高
// 的方式已经足够。
func buildForkSeed(mode string, parent []conversation.Message) []conversation.Message {
	switch mode {
	case "full":
		out := make([]conversation.Message, len(parent))
		copy(out, parent)
		return out
	case "recent":
		if len(parent) <= 5 {
			out := make([]conversation.Message, len(parent))
			copy(out, parent)
			return out
		}
		out := make([]conversation.Message, 5)
		copy(out, parent[len(parent)-5:])
		return out
	default:
		return nil
	}
}
