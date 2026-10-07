package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccode/internal/compact"
	"ccode/internal/conversation"
	"ccode/internal/filehistory"
	"ccode/internal/hooks"
	"ccode/internal/llm"
	"ccode/internal/permissions"
	"ccode/internal/planfile"
	"ccode/internal/prompt"
	"ccode/internal/session"
	"ccode/internal/toolresult"
	"ccode/internal/tools"
)

const (
	maxTokensCeiling          = 64000
	maxOutputTokensRecoveries = 3
)

type Agent struct {
	Client        llm.Client
	Registry      *tools.Registry
	Protocol      string
	WorkDir       string
	MaxIterations int
	ContextWindow int
	// MaxOutputTokens 是模型的最大输出预算，第二层压缩用它计算有效窗口和压缩阈值。
	// 零值会回退到 compact 包内的 summaryOutputReserve 默认值。
	MaxOutputTokens int
	Checker         *permissions.Checker
	Hooks           *hooks.Engine
	// SessionID 标识当前 Agent 追加写入的磁盘会话日志。设置后，第二层压缩会向
	// 该会话写入 compact_boundary 记录，使后续恢复可重建压缩后的状态，无需重放
	// 压缩前的完整记录。为空时关闭边界持久化，适用于测试和单次调用方。
	SessionID      string
	NotificationFn func() []string
	// ToolNameFilter 非 nil 时，从发送给 LLM 的 schema 中移除名称判断为 false 的工具。
	// 每轮开头都会查询过滤器，使调用方无需重启 Agent 就能在团队创建或解散时切换
	// Coordinator 模式。
	Instructions  string
	MemoryContent string
	// SkillSection 是可用 Skill 的清单文本。它跟着项目走，所以不进系统提示词，
	// 而是和指令、记忆一起放进首条 system-reminder。
	SkillSection string
	// SkillDeltaFn 返回本轮新出现的 Skill 清单（已通知过的不再返回）。
	// 会话中途装了新 Skill 时只补这几条，不重发整份清单，也不动系统提示词。
	SkillDeltaFn func() string
	// MemoryRecallCh 非阻塞 memory recall：prefetch 与主 LLM 调用并行，
	// 工具执行后从 channel 读取并注入
	MemoryRecallCh <-chan RecallResult
	ToolNameFilter func(name string) bool
	// CoordinatorActiveFn 非 nil 时报告 Coordinator 模式当前是否生效。每轮都会与
	// ToolNameFilter 一同查询，使调度指引只在工具集收窄时出现，并在团队解散后消失。
	CoordinatorActiveFn func() bool
	// OnLoopComplete 非 nil 时，在 Agent 到达 LoopComplete（最终助手消息且没有剩余工具
	// 调用）后以即发即弃方式调用，供后台记忆提取使用。它替代 stopHooks 分发器；
	// 失败保持静默且不能阻塞主循环。回调收到活动会话，不能从其他 goroutine 修改它。
	OnLoopComplete  func(conv *conversation.Manager)
	FileHistory     *filehistory.History
	compactTracking compact.AutoCompactTrackingState
	// RecoveryState 保存第二层把会话压成摘要后重建工作上下文所需的快照，包括最近
	// 读取的文件和调用的 Skill。该结构并发安全，流式执行器可从多个 goroutine 写入。
	RecoveryState *compact.RecoveryState
	eventCh       chan AgentEvent
	// activeSkills 记录本次会话已激活的 Skill 操作说明（名称到正文），供 /skills
	// 展示和 RecoveryState 跨压缩保存。正文只作为消息注入会话一次，不会每轮重复注入。
	activeSkills map[string]string
	// announcedDeferred 是上一次告诉模型的延迟工具清单，按字典序。跟当前清单一比
	// 就知道工具池有没有变，没变就不重发那条提醒。
	announcedDeferred []string
	// recallMu 保护下面两个字段：工具执行分散在多个 goroutine 里写，记忆召回的
	// prefetch 又在自己的 goroutine 里读写。
	recallMu sync.Mutex
	// recentToolNames 是最近调用过的工具名，去重后按调用顺序保留。传给记忆召回
	// 的选择器，让它跳过这些工具的用法说明类记忆，但涉及坑和警告的仍然要选。
	recentToolNames []string
	// surfacedMemPaths 记录本次会话已经注入过的记忆文件路径，召回前做预过滤，
	// 避免同一条记忆每轮重新占用选择器的名额。
	surfacedMemPaths map[string]struct{}
}

// RecallResult 是一次记忆召回的产出：渲染好的 system-reminder 正文，以及
// 选中的记忆文件路径。路径要等正文真正注入对话时才记为已注入。
type RecallResult struct {
	Reminder string
	Paths    []string
}

// maxRecentTools 是传给记忆召回选择器的最近工具名上限。
const maxRecentTools = 10

// RecordRecentTool 记下刚执行完的工具名。重复调用同一个工具时把它移到末尾，
// 这样列表反映的是最近使用顺序而不是首次使用顺序。
func (a *Agent) RecordRecentTool(name string) {
	if name == "" {
		return
	}
	a.recallMu.Lock()
	defer a.recallMu.Unlock()
	if i := slices.Index(a.recentToolNames, name); i >= 0 {
		a.recentToolNames = slices.Delete(a.recentToolNames, i, i+1)
	}
	a.recentToolNames = append(a.recentToolNames, name)
	if len(a.recentToolNames) > maxRecentTools {
		a.recentToolNames = a.recentToolNames[1:]
	}
}

// RecallHints 返回记忆召回要用的两份状态的快照：最近用过的工具名，以及本次会话
// 已经注入过的记忆路径。返回的是副本，调用方可以在自己的 goroutine 里随便用。
func (a *Agent) RecallHints() ([]string, map[string]struct{}) {
	a.recallMu.Lock()
	defer a.recallMu.Unlock()
	tools := slices.Clone(a.recentToolNames)
	surfaced := make(map[string]struct{}, len(a.surfacedMemPaths))
	for p := range a.surfacedMemPaths {
		surfaced[p] = struct{}{}
	}
	return tools, surfaced
}

// MarkMemoriesSurfaced 记下这一轮注入了哪些记忆，下一轮召回时先把它们排除掉。
func (a *Agent) MarkMemoriesSurfaced(paths []string) {
	if len(paths) == 0 {
		return
	}
	a.recallMu.Lock()
	defer a.recallMu.Unlock()
	if a.surfacedMemPaths == nil {
		a.surfacedMemPaths = make(map[string]struct{}, len(paths))
	}
	for _, p := range paths {
		a.surfacedMemPaths[p] = struct{}{}
	}
}

// deferredReminderMarker 是延迟工具清单提醒的固定开头。用它在历史里回认这条提醒
// 还在不在：compact 把历史压成摘要之后原来那条就没了，得重发一遍。
const deferredReminderMarker = "The following deferred tools are available via ToolSearch."

// ActivateSkill 记录一次 Skill 激活。正文供 /skills 列表和压缩恢复使用，但不会
// 每轮重复注入，因为它已经作为普通消息存在于会话中。
func (a *Agent) ActivateSkill(name, body string) {
	if a.activeSkills == nil {
		a.activeSkills = make(map[string]string)
	}
	a.activeSkills[name] = body
}

// ClearActiveSkills 清除所有固定的操作说明。/clear 会调用它，避免新会话携带旧任务的
// 操作说明；从未激活 Skill 时调用也安全。
func (a *Agent) ClearActiveSkills() {
	a.activeSkills = nil
}

// GetActiveSkills 返回当前固定操作说明的副本（名称到正文），供测试和 /skills 展示。
func (a *Agent) GetActiveSkills() map[string]string {
	out := make(map[string]string, len(a.activeSkills))
	for k, v := range a.activeSkills {
		out[k] = v
	}
	return out
}

// SetToolFilter 为当前会话安装工具可见性过滤器。每轮开头都会查询它，使调用方无需
// 重启 Agent 就能切换 Coordinator 模式；传入 nil 会清除已有过滤器。
func (a *Agent) SetToolFilter(allow func(name string) bool) {
	a.ToolNameFilter = allow
}

// ToolRegistry 返回活动工具注册表。方法不用与字段同名的 Registry，以避免 Go 禁止的
// 方法与字段冲突，同时满足 skills.SkillHost 契约。
func (a *Agent) ToolRegistry() *tools.Registry {
	return a.Registry
}

func New(client llm.Client, registry *tools.Registry, protocol string) *Agent {
	wd, _ := os.Getwd()
	return &Agent{
		Client:        client,
		Registry:      registry,
		Protocol:      protocol,
		WorkDir:       wd,
		MaxIterations: 0,
		ContextWindow: 200000,
		RecoveryState: compact.NewRecoveryState(),
	}
}

// SetSessionID 把磁盘会话日志标识接入 Agent，使第二层压缩能把 compact_boundary
// 写入 TUI 正在追加普通消息的同一会话。TUI 会在 Agent 构造后立即调用，恢复流程
// 切换会话后再调用一次。
func (a *Agent) SetSessionID(id string) { a.SessionID = id }

// currentToolSchemas 构建下次 API 调用使用的 schema 列表，并应用当前活动的
// ToolNameFilter，例如团队协调模式。恢复附件和实际 Stream 调用共享此方法，
// 使压缩后仍可用的工具列表与真实调用保持一致。
func (a *Agent) currentToolSchemas() []map[string]any {
	schemas := a.Registry.GetAllSchemas(a.Protocol)
	if a.ToolNameFilter == nil {
		return schemas
	}
	// 过滤器就是唯一依据，不留任何例外分支
	return filterSchemasByName(schemas, a.ToolNameFilter)
}

func (a *Agent) Run(ctx context.Context, conv *conversation.Manager) <-chan AgentEvent {
	ch := make(chan AgentEvent, 32)

	go func() {
		defer close(ch)
		defer a.emitHook(hooks.EventSessionEnd, "", nil)

		a.emitHook(hooks.EventSessionStart, "", nil)

		conv.InjectLongTermMemory(a.Instructions, a.MemoryContent, a.SkillSection)

		var totalInput, totalOutput int
		maxTokensEscalated := false
		outputRecoveries := 0

		for iteration := 1; ; iteration++ {
			if a.MaxIterations > 0 && iteration > a.MaxIterations {
				ch <- ErrorEvent{Message: fmt.Sprintf("Agent reached maximum iterations (%d)", a.MaxIterations)}
				return
			}

			if ctx.Err() != nil {
				return
			}

			// 每轮只计算一次工具 schema 列表，使压缩触发时的恢复附件和下方真实
			// Stream 调用看到相同接线。Skill 过滤器只能在轮次之间变化，不能在轮内变化。
			toolSchemas := a.currentToolSchemas()

			// 计划模式：注入结构化工作流提醒。
			if a.Checker != nil && a.Checker.Mode == permissions.ModePlan {
				planPath := planfile.GetOrCreatePlanPath(a.WorkDir)
				a.Checker.PlanFilePath = planPath
				planExists := planfile.PlanExists(a.WorkDir)
				reminder := prompt.BuildPlanModeReminder(planPath, planExists, iteration)
				conv.AddSystemReminder(reminder)
			}

			// Coordinator 模式：工具集被收窄的同时注入调度指引。
			// 走 system-reminder 而不是系统提示词：长会话里开头那份约束会被淹没，
			// 每轮追加一次才拉得回来，而且系统提示词是缓存前缀，动它整段都要重新计费。
			if a.CoordinatorActiveFn != nil && a.CoordinatorActiveFn() {
				conv.AddSystemReminder(prompt.CoordinatorReminder(iteration))
			}

			if a.NotificationFn != nil {
				for _, note := range a.NotificationFn() {
					conv.AddSystemReminder(note)
				}
			}

			a.emitHook(hooks.EventTurnStart, "", nil)

			// 会话中途新增的 Skill：只补新出现的那几条，不重发整份清单，
			// 更不动系统提示词，避免把缓存前缀顶掉。
			if a.SkillDeltaFn != nil {
				if delta := a.SkillDeltaFn(); delta != "" {
					conv.AddSystemReminder("The following skills became available:\n" + delta)
				}
			}

			// 把延迟工具名称注入 system-reminder，让模型知道 ToolSearch 可找到哪些工具。
			// dispatch 模式下这些工具永远不会进 tools[]，必须额外告诉
			// 模型调用要走 mcp_call，否则它读完 schema 也不知道从哪儿调。
			//
			// 只在需要的时候发，不每轮重发。这条提醒是 append 进历史的，发过一次
			// 就一直在上下文里，之后每轮再发一遍只是拿同样的内容占窗口：六十来个
			// MCP 工具一份清单五百多 token，四十轮下来就是两万多。
			//
			// 两种情况要重发：池子变了（MCP 是异步连上的，服务器也可能掉线重连），
			// 或者历史里那条已经被 compact 压掉了。后者靠回扫历史发现，这样就不用
			// 在 compact 那边额外挂钩子。
			if deferredNames := a.Registry.GetDeferredToolNames(); len(deferredNames) > 0 {
				poolChanged := !slices.Equal(a.announcedDeferred, deferredNames)
				if poolChanged || !conv.HasReminderContaining(deferredReminderMarker) {
					reminder := deferredReminderMarker + " Their schemas are NOT loaded - use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas"
					if a.Registry.McpLoadingMode == tools.McpLoadingDispatch {
						reminder += ", then invoke them with the mcp_call tool"
					} else {
						reminder += " before calling them"
					}
					conv.AddSystemReminder(reminder + ":\n" + strings.Join(deferredNames, "\n"))
					a.announcedDeferred = deferredNames
				}
			}

			a.emitHook(hooks.EventPreSend, "", nil)

			// 第二层：自动压缩。
			// Layer 1（工具结果预算）在结果入历史时已处理完，历史里的
			// 内容就是最终大小，直接用其消息估算 token
			if msg, err := compact.ManageContext(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.MaxOutputTokens, &a.compactTracking, a.RecoveryState, toolSchemas); err == nil && msg != "" {
				ch <- CompactEvent{Message: msg}
				conv.ClearUsageAnchor()
				conv.InjectLongTermMemory(a.Instructions, a.MemoryContent, a.SkillSection)
			}

			events, errs := a.Client.Stream(ctx, conv, toolSchemas)

			var text string
			var toolCalls []llm.ToolCallComplete
			var thinkingBlocks []conversation.ThinkingBlock
			var stopReason string
			var usage llm.UsageInfo

			executor := NewStreamingExecutor(a.Registry, ch)

			for ev := range events {
				switch e := ev.(type) {
				case llm.ThinkingDelta:
					ch <- ThinkingText{Text: e.Text}
				case llm.ThinkingComplete:
					thinkingBlocks = append(thinkingBlocks, conversation.ThinkingBlock{
						Thinking:  e.Thinking,
						Signature: e.Signature,
					})
				case llm.TextDelta:
					text += e.Text
					ch <- StreamText{Text: e.Text}
				case llm.ToolCallStart:
					ch <- ToolUseEvent{ToolID: e.ToolID, ToolName: e.ToolName}
				case llm.ToolCallDelta:
					// 参数增量由完整工具调用事件统一处理，此处忽略。
				case llm.ToolCallComplete:
					toolCalls = append(toolCalls, e)
					ch <- ToolUseEvent{
						ToolID:   e.ToolID,
						ToolName: e.ToolName,
						Args:     e.Arguments,
					}
					// 收集工具调用，等流式结束后按安全性分批执行
					executor.Submit(toolCallInfo{
						toolID:    e.ToolID,
						toolName:  e.ToolName,
						arguments: e.Arguments,
					})
				case llm.StreamEnd:
					stopReason = e.StopReason
					usage = e.Usage
				}
			}
			a.emitHook(hooks.EventPostReceive, text, nil)

			// 处理流错误。
			select {
			case err := <-errs:
				if err != nil {
					if retry, compacted := a.handleStreamError(ctx, ch, conv, err); retry {
						if compacted {
							conv.ClearUsageAnchor()
							conv.InjectLongTermMemory(a.Instructions, a.MemoryContent, a.SkillSection)
						}
						continue // 重试当前轮次
					}
					ch <- ErrorEvent{Message: err.Error()}
					return
				}
			default:
			}

			totalInput += usage.InputTokens
			totalOutput += usage.OutputTokens
			ch <- UsageEvent{InputTokens: totalInput, OutputTokens: totalOutput}

			anchorAfterAssistant := func() {
				conv.RecordUsageAnchor(
					usage.InputTokens,
					usage.OutputTokens,
					usage.CacheReadTokens,
					usage.CacheCreationTokens,
				)
			}

			// 处理 max_tokens 停止原因。
			if stopReason == "max_tokens" {
				if !maxTokensEscalated {
					// 首次遇到时静默提高上限。
					if setter, ok := a.Client.(llm.MaxTokensSetter); ok {
						setter.SetMaxOutputTokens(maxTokensCeiling)
						maxTokensEscalated = true
					}
					if text != "" || len(thinkingBlocks) > 0 {
						conv.AddAssistantFull(text, thinkingBlocks, nil)
						a.persistLastMessage(conv)
						anchorAfterAssistant()
						conv.AddUserMessage("Output token limit hit. Resume directly from where you stopped. Do not apologize or repeat previous content. Pick up mid-thought if needed.")
					}
					ch <- RetryEvent{Reason: "max_tokens escalation", Wait: 0}
					continue
				} else if outputRecoveries < maxOutputTokensRecoveries {
					// 跨轮恢复。
					outputRecoveries++
					conv.AddAssistantFull(text, thinkingBlocks, nil)
					a.persistLastMessage(conv)
					anchorAfterAssistant()
					conv.AddUserMessage("Output token limit hit. Resume directly from where you stopped. Break remaining work into smaller pieces.")
					ch <- RetryEvent{Reason: fmt.Sprintf("max_tokens recovery %d/%d", outputRecoveries, maxOutputTokensRecoveries), Wait: 0}
					continue
				}
				// 恢复次数耗尽后继续走普通完成流程。
			} else {
				// 成功完成一轮后重置恢复计数。
				outputRecoveries = 0
			}

			if len(toolCalls) == 0 {
				conv.AddAssistantFull(text, thinkingBlocks, nil)
				a.persistLastMessage(conv)
				if a.FileHistory != nil {
					summary := text
					if len(summary) > 60 {
						summary = summary[:60] + "..."
					}
					a.FileHistory.MakeSnapshot(conv.Len(), summary)
				}
				ch <- LoopComplete{TotalTurns: iteration}
				if a.OnLoopComplete != nil {
					go a.OnLoopComplete(conv)
				}
				return
			}

			var toolUses []conversation.ToolUseBlock
			for _, tc := range toolCalls {
				toolUses = append(toolUses, conversation.ToolUseBlock{
					ToolUseID: tc.ToolID,
					ToolName:  tc.ToolName,
					Arguments: tc.Arguments,
				})
			}
			conv.AddAssistantFull(text, thinkingBlocks, toolUses)
			a.persistLastMessage(conv)
			// 助手消息写入后，把真实用量锚定到当前会话；后续工具结果和下一条用户消息
			// 在该基线上做增量估算。
			anchorAfterAssistant()

			// 按安全性分批执行：只读工具并发，写/命令工具串行
			results := executor.ExecuteAll(ctx, a)

			// 溢写文件的回读结果豁免溢写：把模型刚读回来的内容再写盘换成
			// 预览，模型就永远看不到全文，还会在「读回、溢写」之间打转。
			exempt := make(map[string]bool)
			for _, tc := range toolCalls {
				if toolresult.IsSpillReadback(tc.ToolName, tc.Arguments, a.WorkDir, a.SessionID) {
					exempt[tc.ToolID] = true
				}
			}

			var toolResults []conversation.ToolResultBlock
			for _, r := range results {
				ch <- ToolResultEvent{
					ToolID:   r.toolID,
					ToolName: r.toolName,
					Output:   r.output,
					IsError:  r.isError,
					Elapsed:  r.elapsed,
				}

				content := r.output
				if len(content) > tools.MaxOutputChars && !exempt[r.toolID] {
					// 单条超限：写盘换预览。写盘失败会原样保留，同一块磁盘
					// 聚合预算也不必再试，所以两种结果都标记豁免。
					content = toolresult.PersistLargeResult(a.WorkDir, a.SessionID, r.toolID, r.output)
					exempt[r.toolID] = true
				}
				toolResults = append(toolResults, conversation.ToolResultBlock{
					ToolUseID:     r.toolID,
					Content:       content,
					IsError:       r.isError,
					ContentBlocks: r.contentBlocks,
				})
			}

			// 聚合预算：一轮并行工具的结果落在同一条消息里，单条阈值管不住
			// 合计超限的情况。进历史前把整批处理完，消息一出生就是终态。
			toolresult.ApplyBudget(toolResults, exempt, a.WorkDir, a.SessionID)

			exitPlanCalled := false
			for _, tc := range toolCalls {
				if tc.ToolName == "ExitPlanMode" {
					exitPlanCalled = true
					break
				}
			}
			conv.AddToolResultsMessage(toolResults)
			a.persistLastMessage(conv)

			// 非阻塞 memory recall：工具执行完后检查 prefetch 是否就绪
			if a.MemoryRecallCh != nil {
				select {
				case recall := <-a.MemoryRecallCh:
					if recall.Reminder != "" {
						conv.AddSystemReminder(recall.Reminder)
						// 真正进了对话才算「已注入」。这一轮没消费掉的召回结果
						// 不留痕，下一轮召回时这些记忆还能参选。
						a.MarkMemoriesSurfaced(recall.Paths)
					}
					a.MemoryRecallCh = nil // 只消费一次
				default:
					// prefetch 还没好，下轮再检查
				}
			}

			if exitPlanCalled {
				ch <- TurnComplete{Turn: iteration}
				ch <- LoopComplete{TotalTurns: iteration}
				return
			}
			ch <- TurnComplete{Turn: iteration}
			a.emitHook(hooks.EventTurnEnd, "", nil)
		}
	}()

	return ch
}

// emitHook 在配置 Engine 后触发 hook 事件。失败不会终止流程，而是进入 hook 通知队列，
// 并在下一轮作为系统提醒排出。
func (a *Agent) emitHook(event hooks.EventName, message string, args map[string]any) {
	if a.Hooks == nil {
		return
	}
	a.Hooks.RunHooks(hooks.HookContext{
		EventName: event,
		ToolArgs:  args,
		Message:   message,
	})
}

// filterSchemasByName 只保留名称通过 allow 判断的工具 schema。Coordinator 模式用它
// 把 Lead Agent 限制为仅使用协调工具，实际工作交给队员完成。
func filterSchemasByName(schemas []map[string]any, allow func(name string) bool) []map[string]any {
	out := make([]map[string]any, 0, len(schemas))
	for _, s := range schemas {
		name, _ := s["name"].(string)
		if allow(name) {
			out = append(out, s)
		}
	}
	return out
}

// handleStreamError 返回 retry 和 compacted：retry 表示调用方应重跑当前轮；
// compacted 表示 ForceCompact 已重写会话，调用方必须丢弃用量锚点，因为原
// AnchorCount 已无法对应新记录。
func (a *Agent) handleStreamError(ctx context.Context, ch chan AgentEvent, conv *conversation.Manager, err error) (retry, compacted bool) {
	var modelErr *llm.Error
	if errors.As(err, &modelErr) && modelErr.Kind == llm.ErrorContext {
		// 历史里的工具结果在入历史时已按预算处理为终态，直接传 nil
		// 让 ForceCompact 使用 conv 自身消息
		msg, compactErr := compact.ForceCompact(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.RecoveryState, a.currentToolSchemas())
		if compactErr == nil && msg != "" {
			ch <- CompactEvent{Message: "Auto-compacted due to context length: " + msg}
			return true, true // 重试；此时锚点已经失效
		}
		return false, false
	}

	if errors.As(err, &modelErr) && modelErr.Kind == llm.ErrorRateLimit {
		wait := parseRetryAfter(modelErr.RetryAfter)
		ch <- RetryEvent{Reason: "rate limited", Wait: wait}
		select {
		case <-time.After(wait):
			return true, false // 不压缩，直接重试
		case <-ctx.Done():
			return false, false
		}
	}

	return false, false
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 5 * time.Second
	}
	if secs, err := strconv.Atoi(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	return 5 * time.Second
}

type toolExecResult struct {
	toolID   string
	toolName string
	output   string
	isError  bool
	elapsed  time.Duration
	// contentBlocks 让工具把结构化 content block 透到对话历史里。只有官方端点
	// 下的 ToolSearch 会填（tool_reference），其余工具留空走纯文本。
	contentBlocks []map[string]any
}

// extractFilePath 从常见工具参数键中提取代表性路径，使 hook 可以执行路径 glob
// 匹配，例如 file_path =* "**/*.go"。
func extractFilePath(args map[string]any) string {
	for _, key := range []string{"file_path", "path", "pattern", "target"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (a *Agent) executeSingleTool(ctx context.Context, eventCh chan AgentEvent, tc toolCallInfo) toolExecResult {
	tool := a.Registry.Get(tc.toolName)
	start := time.Now()

	if tool == nil {
		// 工具名不存在只回一条错误结果，让模型自己换个工具重来，不打断循环。
		return toolExecResult{
			toolID:   tc.toolID,
			toolName: tc.toolName,
			output:   fmt.Sprintf("Error: unknown tool '%s'", tc.toolName),
			isError:  true,
			elapsed:  time.Since(start),
		}
	}

	if a.Checker != nil {
		decision := a.Checker.Check(tool, tc.arguments)
		if decision.Effect == permissions.Deny {
			return toolExecResult{
				toolID:   tc.toolID,
				toolName: tc.toolName,
				output:   fmt.Sprintf("Permission denied: %s", decision.Reason),
				isError:  true,
				elapsed:  time.Since(start),
			}
		}
		if decision.Effect == permissions.Ask {
			respCh := make(chan PermissionReply, 1)
			desc := permissions.DescribeToolAction(tc.toolName, tc.arguments)
			eventCh <- PermissionRequestEvent{
				ToolName:   tc.toolName,
				Desc:       desc,
				ResponseCh: respCh,
			}
			// 等待期间用户可能直接中断本轮，这时按拒绝处理，不能一直卡在这里
			var reply PermissionReply
			select {
			case reply = <-respCh:
			case <-ctx.Done():
				reply = PermissionReply{Response: PermDeny}
			}
			if reply.Response == PermDeny {
				return toolExecResult{
					toolID:   tc.toolID,
					toolName: tc.toolName,
					output:   conversation.RejectedToolResultWithFeedback(reply.Feedback),
					isError:  true,
					elapsed:  time.Since(start),
				}
			}
			if reply.Response == PermAllowAlways {
				content := permissions.ExtractContent(tc.toolName, tc.arguments)
				pattern := content + "*"
				if len(content) > 60 {
					pattern = content[:60] + "*"
				}
				// 写入本地规则文件，规则引擎每次评估都现读现匹配，本轮之后即刻生效
				a.Checker.RuleEngine.AppendLocalRule(permissions.Rule{
					ToolName: tc.toolName,
					Pattern:  pattern,
					Effect:   permissions.RuleAllow,
				})
			}
		}
	}

	if a.Hooks != nil {
		hctx := hooks.HookContext{
			EventName: hooks.EventPreToolUse,
			ToolName:  tc.toolName,
			ToolArgs:  tc.arguments,
			FilePath:  extractFilePath(tc.arguments),
		}
		if rejected, msg := a.Hooks.RunPreToolHooks(hctx); rejected {
			return toolExecResult{
				toolID:   tc.toolID,
				toolName: tc.toolName,
				output:   "Blocked by hook: " + msg,
				isError:  true,
				elapsed:  time.Since(start),
			}
		}
	}

	result := tool.Execute(ctx, tc.arguments)

	a.RecordRecentTool(tc.toolName)

	if !result.IsError && tc.toolName == "ReadFile" {
		if p, _ := tc.arguments["file_path"].(string); p != "" {
			if data, err := os.ReadFile(p); err == nil {
				a.RecoveryState.RecordFileRead(p, string(data))
			}
		}
	}

	if a.Hooks != nil {
		a.Hooks.RunHooks(hooks.HookContext{
			EventName: hooks.EventPostToolUse,
			ToolName:  tc.toolName,
			ToolArgs:  tc.arguments,
			FilePath:  extractFilePath(tc.arguments),
			Message:   result.Output,
		})
	}

	return toolExecResult{
		toolID:        tc.toolID,
		toolName:      tc.toolName,
		output:        result.Output,
		isError:       result.IsError,
		elapsed:       time.Since(start),
		contentBlocks: result.ContentBlocks,
	}
}

func formatToolArgs(args map[string]any) string {
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, s))
	}
	return strings.Join(parts, ", ")
}

// persistLastMessage 把刚追加进对话历史的那条消息写入会话日志。
//
// 落盘点放在主循环而不是各个前端：TUI 和 Web 共用同一条记录路径，
// 中间轮次的助手文本和完整的工具调用链都会被记下来，恢复会话时才能还原。
// WorkDir 或 SessionID 为空时（一次性调用、子 Agent）跳过，不写盘。
func (a *Agent) persistLastMessage(conv *conversation.Manager) {
	if a.WorkDir == "" || a.SessionID == "" {
		return
	}
	msgs := conv.GetMessages()
	if len(msgs) == 0 {
		return
	}
	session.SaveMessage(a.WorkDir, a.SessionID, session.FromConversation(msgs[len(msgs)-1]))
}
