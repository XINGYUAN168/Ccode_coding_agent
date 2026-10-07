// Package compact 实现 Ccode 的第二层上下文管理：当 token 用量达到阈值时，
// 由 LLM 为完整会话生成摘要。它用一条摘要消息和一条继续确认替换原会话，
// 也可通过 ForceCompact（/compact 命令）手动触发。
//
// Layer 1（工具结果预算）在 package toolresult：单条超限和单消息聚合
// 超限都在结果进入对话历史那一刻处理完，消息一进历史就是终态，这里
// 拿到的消息大小即最终大小。
package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ccode/internal/conversation"
	"ccode/internal/llm"
	"ccode/internal/session"
)

const (
	// maxPTLRetries 限制摘要请求因提示词过长而失败后的重试次数；每次重试前
	// 删除最旧的一组 API 轮次。
	maxPTLRetries = 3
	// ptlRetryMarker 在删除旧轮次后放到最前面，确保摘要请求仍以用户角色消息开头。
	ptlRetryMarker = "[earlier conversation truncated for compaction retry]"

	// autoCompactThreshold 是旧版比例门槛，仅留作参考。当前决策使用下方的绝对
	// token 公式：已用 token 接近上下文窗口限制时触发压缩，为下一轮预留空间。
	autoCompactThreshold = 0.80

	// summaryOutputReserve 为摘要响应本身预留空间，因此有效窗口等于
	// contextWindow − min(模型 maxOutput, summaryOutputReserve)。
	summaryOutputReserve = 20000
	// autoCompactSafetyMargin 设置低于有效窗口的自动压缩软触发线。
	autoCompactSafetyMargin = 13000
	// manualCompactSafetyMargin 设置硬阻断线：已用 token 超过
	// effectiveWindow − manualCompactSafetyMargin 后强制压缩，不再依赖软触发。
	manualCompactSafetyMargin = 3000
)

// 压缩时为近期消息保留预算：不摘要整段记录并丢弃全部原消息，而是原样保留
// 最近的尾部消息，只摘要较旧的前缀。
const (
	// keepRecentTokens 是 token 保留下限：从尾部向前累加每条消息的 token，
	// 直到至少保留这个数量。
	keepRecentTokens = 10000
	// minKeepMessages 是不受 token 数影响的近期消息最少保留条数。满足
	// keepRecentTokens 或 minKeepMessages 中任一条件即可停止向前遍历。
	minKeepMessages = 5
	// keepMaxTokens 限制保留尾部的大小：继续累加将超过该值时停止，即使尚未满足
	// 下限也不再向前，避免保留过多内容导致摘要无法节省空间。
	keepMaxTokens = 40000
)

// computeCompactThreshold 返回触发第二层压缩的绝对已用 token 阈值。
// effectiveWindow = contextWindow − min(maxOutput, summaryOutputReserve)；
// 阈值为有效窗口减去安全余量，硬阻断使用手动余量，软触发使用自动余量。
func computeCompactThreshold(contextWindow, maxOutput int, manual bool) int {
	reserve := summaryOutputReserve
	if maxOutput > 0 && maxOutput < reserve {
		reserve = maxOutput
	}
	effectiveWindow := contextWindow - reserve
	margin := autoCompactSafetyMargin
	if manual {
		margin = manualCompactSafetyMargin
	}
	return effectiveWindow - margin
}

// MaxConsecutiveAutoCompactFailures 在上下文无法恢复地超限（如 prompt_too_long）时，
// 限制自动压缩重试，避免 Agent 每轮都向 API 发起必然失败的请求。
const MaxConsecutiveAutoCompactFailures = 3

// AutoCompactTrackingState 在 Agent 循环迭代间传递熔断状态。结构体由调用方持有，
// ManageContext 会就地修改它。
type AutoCompactTrackingState struct {
	// ConsecutiveFailures 记录上次成功后自动压缩连续失败的次数；成功时清零。
	ConsecutiveFailures int
}

// summarySystemPrompt 要求模型分两段响应：先输出 <analysis> 草稿块，再输出
// <summary> 摘要块。结果写回会话前，formatCompactSummary 会删除草稿，只保留
// 结构化摘要。
const summarySystemPrompt = `Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions.
This summary should be thorough in capturing technical details, code patterns, and architectural decisions that would be essential for continuing development work without losing context.

Before providing your final summary, wrap your analysis in <analysis> tags to organize your thoughts and ensure you've covered all necessary points. In your analysis process:

1. Chronologically analyze each message and section of the conversation. For each section thoroughly identify:
   - The user's explicit requests and intents
   - Your approach to addressing the user's requests
   - Key decisions, technical concepts and code patterns
   - Specific details like:
     - file names
     - full code snippets
     - function signatures
     - file edits
   - Errors that you ran into and how you fixed them
   - Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
2. Double-check for technical accuracy and completeness, addressing each required element thoroughly.

After your analysis, output your final summary wrapped in <summary> tags. Your summary should include the following sections:

1. Primary Request and Intent: Capture all of the user's explicit requests and intents in detail
2. Key Technical Concepts: List all important technical concepts, technologies, and frameworks discussed.
3. Files and Code Sections: Enumerate specific files and code sections examined, modified, or created. Pay special attention to the most recent messages and include full code snippets where applicable and include a summary of why this file read or edit is important.
4. Errors and fixes: List all errors that you ran into, and how you fixed them. Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
5. Problem Solving: Document problems solved and any ongoing troubleshooting efforts.
6. All user messages: List ALL user messages that are not tool results. These are critical for understanding the users' feedback and changing intent.
7. Pending Tasks: Outline any pending tasks that you have explicitly been asked to work on.
8. Current Work: Describe in detail precisely what was being worked on immediately before this summary request, paying special attention to the most recent messages from both user and assistant. Include file names and code snippets where applicable.
9. Optional Next Step: List the next step that you will take that is related to the most recent work you were doing. IMPORTANT: ensure that this step is DIRECTLY in line with the user's most recent explicit requests, and the task you were working on immediately before this summary request. If your last task was concluded, then only list next steps if they are explicitly in line with the users request.
   If there is a next step, include direct quotes from the most recent conversation showing exactly what task you were working on and where you left off. This should be verbatim to ensure there's no drift in task interpretation.

Output structure:

<analysis>
[Your thought process, ensuring all points are covered thoroughly and accurately]
</analysis>

<summary>
1. Primary Request and Intent:
   [Detailed description]

2. Key Technical Concepts:
   - [Concept 1]
   - [Concept 2]

3. Files and Code Sections:
   - [File Name 1]
      - [Summary and important code snippet]

4. Errors and fixes:
   - [Error and fix description]

5. Problem Solving:
   [Description]

6. All user messages:
   - [User message 1]
   - [User message 2]

7. Pending Tasks:
   - [Task 1]

8. Current Work:
   [Precise description]

9. Optional Next Step:
   [Next step if applicable]
</summary>`

// EstimateTokens 使用每 3.5 个字符约一个 token 的方式估算正文、思考、工具参数和工具结果。
func EstimateTokens(messages []conversation.Message) int {
	total := 0
	for _, m := range messages {
		total += int(float64(len(m.Content))/3.5) + 4
		for _, tu := range m.ToolUses {
			argsJSON, _ := json.Marshal(tu.Arguments)
			total += 50 + int(float64(len(argsJSON))/3.5)
		}
		for _, tr := range m.ToolResults {
			total += int(float64(len(tr.Content))/3.5) + 10
		}
		for _, tb := range m.ThinkingBlocks {
			total += int(float64(len(tb.Thinking)) / 3.5)
		}
	}
	return total
}

// UsageAnchor 记录最近一次真实 API 用量，以及报告该用量时的会话长度。
// baselineTokens 是该轮真实的提示词与输出总量，即 input、cache_read、
// cache_creation 和 output 之和；anchorCount 是助手轮结束后的 conv.Len()。
// anchorCount 之后追加的工具结果、下一条用户消息和系统提醒还没有真实用量，
// 因此在基线上增量估算。
//
// 零值锚点（HasUsage 为 false）表示尚未观察到真实用量，通常是首轮；此时调用方
// 回退到完整字符估算。
type UsageAnchor struct {
	BaselineTokens int
	AnchorCount    int
	HasUsage       bool
}

// BaselineFromUsage 把 API 用量报告合并成一个“实际已传输 token”数，作为锚点基线。
// Anthropic 会把 cache_read、cache_creation 与 input_tokens 分开报告，因此真实
// 提示词大小需要把四个计数器相加。
func BaselineFromUsage(u llm.UsageInfo) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
}

// ComputeUsedTokens 返回用于和压缩阈值比较的当前“已用 token”。存在真实用量锚点
// 时，返回 baselineTokens 加上锚点后新增消息的估算量；没有锚点（冷启动或首轮）
// 时，对全部消息做字符估算，使 Agent 在收到首份用量报告前仍可正常工作。
func ComputeUsedTokens(messages []conversation.Message, anchor UsageAnchor) int {
	if !anchor.HasUsage {
		return EstimateTokens(messages)
	}
	// 防御性限制：若会话回退到锚点之前（如压缩之后），说明锚点已过期，
	// 此时估算全部内容，避免索引越界。
	if anchor.AnchorCount < 0 || anchor.AnchorCount > len(messages) {
		return EstimateTokens(messages)
	}
	return anchor.BaselineTokens + EstimateTokens(messages[anchor.AnchorCount:])
}

// ComputeUsedTokensFromConv 从 ConversationManager 读取锚点状态来计算
// 当前 token 用量。这是 ComputeUsedTokens 的便捷封装，避免调用方
// 手动传递 UsageAnchor 参数。
func ComputeUsedTokensFromConv(conv *conversation.Manager) int {
	baseline, count, has := conv.UsageAnchorState()
	return ComputeUsedTokens(conv.GetMessages(), UsageAnchor{
		BaselineTokens: baseline,
		AnchorCount:    count,
		HasUsage:       has,
	})
}

// ManageContext 在已用 token 达到自动压缩阈值（有效窗口减自动余量）时执行第二层
// autoCompact；超过硬阻断线（有效窗口减手动余量）时强制压缩。详见
// computeCompactThreshold。第一层工具结果预算在写入时执行，因此工具结果进入历史
// 时已经是最终大小，这里无需再裁剪估算值。
//
// tracking 在迭代间携带熔断状态；为 nil 时关闭熔断，适用于测试和单次调用方。
//
// anchor 携带最近一次真实 API 用量，即基线和报告时的会话长度。存在锚点时，
// 已用 token 等于 baselineTokens 加上之后新增消息的增量估算；不存在时（首轮）
// 回退到完整字符估算。这里只改变“当前已用 token”的计算方式，不改变
// effectiveWindow − margin 的阈值公式。
//
// workDir 和 sessionID 用于定位磁盘会话日志；两者均非空时，成功压缩会追加
// compact_boundary 记录，使后续恢复可重建压缩后的状态，无需重放压缩前的完整记录。
// 任一为空时跳过边界持久化，供测试和单次调用使用，其余行为不变。
func ManageContext(
	ctx context.Context,
	conv *conversation.Manager,
	client llm.Client,
	workDir string,
	sessionID string,
	contextWindow int,
	maxOutput int,
	tracking *AutoCompactTrackingState,
	recovery *RecoveryState,
	toolSchemas []map[string]any,
) (string, error) {
	// 历史里的工具结果在入历史时已按预算处理为终态，conv 自身消息
	// 就是实际发送量，直接用它估算。
	baseline, count, has := conv.UsageAnchorState()
	anchor := UsageAnchor{BaselineTokens: baseline, AnchorCount: count, HasUsage: has}
	tokens := ComputeUsedTokens(conv.GetMessages(), anchor)
	// 自动压缩软触发条件：已用 token >= effectiveWindow − auto margin。
	if tokens < computeCompactThreshold(contextWindow, maxOutput, false) {
		return "", nil
	}

	// 硬阻断线：已用 token 超过 effectiveWindow − manual margin 后绕过熔断器
	// 强制压缩，不再走可能跳过的自动软触发路径，因为上下文已过于接近上限。
	if tokens >= computeCompactThreshold(contextWindow, maxOutput, true) {
		return ForceCompact(ctx, conv, client, workDir, sessionID, contextWindow, recovery, toolSchemas)
	}

	// 熔断器：连续失败 N 次后停止重试，避免上下文已无法恢复时每轮都向 API
	// 发起必然失败的压缩请求。
	if tracking != nil && tracking.ConsecutiveFailures >= MaxConsecutiveAutoCompactFailures {
		return "", nil
	}

	msg, err := autoCompact(ctx, conv, client, workDir, sessionID, contextWindow, recovery, toolSchemas)
	if err != nil {
		if tracking != nil {
			tracking.ConsecutiveFailures++
		}
		return "", err
	}
	if tracking != nil {
		tracking.ConsecutiveFailures = 0
	}
	return msg, nil
}

// ForceCompact 是手动 /compact 入口，不考虑当前 token 比例，始终执行第二层完整摘要。
// 完整摘要会覆盖工具结果预算的效果，因此跳过第一层。
func ForceCompact(
	ctx context.Context,
	conv *conversation.Manager,
	client llm.Client,
	workDir string,
	sessionID string,
	contextWindow int,
	recovery *RecoveryState,
	toolSchemas []map[string]any,
) (string, error) {
	return autoCompact(ctx, conv, client, workDir, sessionID, contextWindow, recovery, toolSchemas)
}

// hasToolResults 判断消息是否携带 tool_result 块，即工具执行器产生的用户角色消息。
// 这类消息不能与对应的助手 tool_use 分离，否则 API 会因存在孤立 tool_result
// 而拒绝会话。
func hasToolResults(m conversation.Message) bool {
	return len(m.ToolResults) > 0
}

// hasToolUses 判断消息是否携带 tool_use 块，即调用了工具的助手消息。
func hasToolUses(m conversation.Message) bool {
	return len(m.ToolUses) > 0
}

// computeKeepStartIndex 选择待摘要前缀 messages[:keepStart] 与原样保留的近期尾部
// messages[keepStart:] 之间的边界。
//
// 从尾部向前遍历，用单条消息的 EstimateTokens 累加 token。达到 token 下限
// keepRecentTokens 或消息数下限 minKeepMessages 中任一条件即可停止。但累计尾部
// 不得超过 keepMaxTokens；再加入一条消息会越界时，在该消息之前停止。
//
// 预算遍历后向前调整边界，避免拆开 tool_use 与 tool_result 配对。若 keepStart
// 落在携带 tool_results 的消息上，就越过前面的助手 tool_use 消息，把完整配对
// 保留在尾部，不能留下孤立的 tool_result。
//
// 返回边界索引；调用方把 keepStart <= 0 或前缀过短视为“内容不足以压缩”，
// 并回退到原有的完整摘要行为。
func computeKeepStartIndex(messages []conversation.Message) int {
	n := len(messages)
	if n == 0 {
		return 0
	}

	keptTokens := 0
	keptCount := 0
	keepStart := n
	for i := n - 1; i >= 0; i-- {
		msgTokens := EstimateTokens(messages[i : i+1])
		// 上限：加入该消息会超过限制时停止，把它留在待摘要前缀中。
		if keptCount > 0 && keptTokens+msgTokens > keepMaxTokens {
			break
		}
		keptTokens += msgTokens
		keptCount++
		keepStart = i
		// 满足任一下限即可结束。
		if keptTokens >= keepRecentTokens || keptCount >= minKeepMessages {
			break
		}
	}

	// 不拆分 tool_use 与 tool_result 配对：若边界消息含有 tool_results，就向前
	// 越过对应的助手 tool_use 消息，完整保留这一组配对。
	for keepStart > 0 && hasToolResults(messages[keepStart]) {
		prev := keepStart - 1
		if hasToolUses(messages[prev]) {
			keepStart = prev
			continue
		}
		break
	}

	return keepStart
}

// groupMessagesByAPIRound 按 API 往返边界对消息分组。每条紧随 tool_result 的
// 助手消息都会开启新组，即新的 LLM 轮次。这样可保持 tool_use/tool_result 配对，
// 删除整组时不会留下孤立的 tool_result。
func groupMessagesByAPIRound(messages []conversation.Message) [][]conversation.Message {
	var groups [][]conversation.Message
	var current []conversation.Message
	prevHadToolResult := false

	for _, m := range messages {
		if m.Role == "assistant" && prevHadToolResult && len(current) > 0 {
			groups = append(groups, current)
			current = nil
		}
		current = append(current, m)
		prevHadToolResult = len(m.ToolResults) > 0
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// truncateHeadForPTL 从前缀中删除最旧的 API 轮次组，直到估算 token 至少减少
// tokenGap；没有值得摘要的内容时返回 nil。
func truncateHeadForPTL(prefix []conversation.Message, tokenGap int) []conversation.Message {
	groups := groupMessagesByAPIRound(prefix)
	if len(groups) < 2 {
		return nil
	}

	dropCount := 0
	if tokenGap > 0 {
		acc := 0
		for _, g := range groups {
			acc += EstimateTokens(g)
			dropCount++
			if acc >= tokenGap {
				break
			}
		}
	} else {
		dropCount = max(1, len(groups)/5)
	}

	dropCount = min(dropCount, len(groups)-1)
	if dropCount < 1 {
		return nil
	}

	var result []conversation.Message
	for _, g := range groups[dropCount:] {
		result = append(result, g...)
	}
	if len(result) > 0 && result[0].Role != "user" {
		marker := conversation.Message{Role: "user", Content: ptlRetryMarker}
		result = append([]conversation.Message{marker}, result...)
	}
	return result
}

// buildPrefixText 把前缀消息序列化为摘要 LLM 调用所需的文本块。
func buildPrefixText(prefix []conversation.Message) string {
	var sb strings.Builder
	for _, m := range prefix {
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
		for _, tu := range m.ToolUses {
			sb.WriteString(fmt.Sprintf("[tool_use %s]: %s\n", tu.ToolName, tu.ToolUseID))
		}
		for _, tr := range m.ToolResults {
			content := tr.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			sb.WriteString(fmt.Sprintf("[tool_result]: %s\n", content))
		}
	}
	return sb.String()
}

// autoCompact 是第二层压缩：让 LLM 摘要较旧前缀，只用一条摘要消息替换
// messages[:keepStart]，并原样保留近期尾部 messages[keepStart:]。摘要完成后，
// 再追加恢复块，使模型仍能看到刚读取文件的快照、已调用 Skill 的操作说明和当前
// 工具列表。前缀短到不值得摘要时，回退到原有完整摘要行为。
func autoCompact(
	ctx context.Context,
	conv *conversation.Manager,
	client llm.Client,
	workDir string,
	sessionID string,
	contextWindow int,
	recovery *RecoveryState,
	toolSchemas []map[string]any,
) (string, error) {
	messages := conv.GetMessages()
	beforeTokens := EstimateTokens(messages)

	// 选择原样保留的近期尾部，只摘要 keepStart 之前的前缀。若边界前没有内容或
	// 几乎没有内容，就无需压缩，保持会话不变。
	keepStart := computeKeepStartIndex(messages)
	if keepStart <= 0 {
		// 全部内容都在保留尾部中，说明会话太短；退化为空操作，避免无意义压缩。
		return "", nil
	}
	prefix := messages[:keepStart]
	keep := messages[keepStart:]

	// 调用 LLM 生成摘要，带 PTL 重试：如果摘要请求本身超出上下文窗口，
	// 从最老的 API 轮次开始丢弃，最多重试 maxPTLRetries 次。
	finalSummary, err := callSummaryWithPTLRetry(ctx, client, prefix, toolSchemas)
	if err != nil {
		return "", err
	}

	// 持久化 compact_boundary 记录，使后续恢复能重建“摘要加保留尾部”的压缩状态，
	// 而不是重放压缩前的完整记录。日志只追加：原前缀仍留在会话文件中，但恢复时
	// 不会越过该边界重放。保留尾部及其工具块会内联到记录，使恢复后的近期轮次仍有
	// 完整工具调用链。边界只保存纯摘要，不保存恢复附件，因为恢复快照只用于内存重建，
	// 恢复会话时不可用。sessionID 或 workDir 为空时跳过，供测试和单次调用使用。
	if sessionID != "" && workDir != "" {
		keepRecords := make([]session.KeepMessage, 0, len(keep))
		for _, m := range keep {
			keepRecords = append(keepRecords, session.FromConversationKeep(m))
		}
		session.SaveCompactBoundary(workDir, sessionID, finalSummary, keepRecords)
	}

	content := "本次会话延续自之前的对话，因上下文空间不足进行了压缩。以下是早期对话的摘要：\n\n" + finalSummary
	if len(keep) > 0 {
		content += "\n\n近期消息已原样保留。"
	}
	if sessionID != "" && workDir != "" {
		content += fmt.Sprintf("\n\n如果你需要压缩前的具体细节（代码片段、报错信息等），请用 ReadFile 读取完整会话记录：%s", session.SessionFilePath(workDir, sessionID))
	}
	if attachment := BuildRecoveryAttachment(recovery, toolSchemas); attachment != "" {
		content += "\n\n---\n\n" + attachment
	}

	compacted := conversation.NewManager()
	compacted.AddUserMessage(content)
	compacted.AppendMessages(keep)

	*conv = *compacted
	afterTokens := EstimateTokens(conv.GetMessages())
	return fmt.Sprintf("Compacted: %d → %d estimated tokens", beforeTokens, afterTokens), nil
}

// callSummaryWithPTLRetry 请求模型生成摘要。遇到上下文过长错误时，它会删除
// 最旧的完整 API 轮次并重试，最多执行 maxPTLRetries 次。
func callSummaryWithPTLRetry(
	ctx context.Context,
	client llm.Client,
	prefix []conversation.Message,
	toolSchemas []map[string]any,
) (string, error) {
	currentPrefix := prefix
	for attempt := 0; ; attempt++ {
		text := buildPrefixText(currentPrefix)
		summaryConv := conversation.NewManager()
		summaryConv.AddUserMessage(summarySystemPrompt + "\n\n" + text)

		events, errs := client.Stream(ctx, summaryConv, toolSchemas)
		var summary strings.Builder
		for ev := range events {
			if td, ok := ev.(llm.TextDelta); ok {
				summary.WriteString(td.Text)
			}
		}
		var streamErr error
		select {
		case streamErr = <-errs:
		default:
		}

		if streamErr == nil {
			return formatCompactSummary(summary.String()), nil
		}

		var modelErr *llm.Error
		if !errors.As(streamErr, &modelErr) || modelErr.Kind != llm.ErrorContext || attempt >= maxPTLRetries {
			return "", streamErr
		}

		tokenGap := EstimateTokens(currentPrefix) / 5
		truncated := truncateHeadForPTL(currentPrefix, tokenGap)
		if truncated == nil {
			return "", streamErr
		}
		currentPrefix = truncated
	}
}

// formatCompactSummary 从模型的两段式响应中删除 <analysis> 草稿块，只返回
// <summary> 块内容。两个标签都不存在时说明模型未遵循格式，此时回退到原始文本，
// 避免完全丢失摘要。
func formatCompactSummary(raw string) string {
	if start := strings.Index(raw, "<summary>"); start >= 0 {
		body := raw[start+len("<summary>"):]
		if end := strings.Index(body, "</summary>"); end >= 0 {
			return strings.TrimSpace(body[:end])
		}
		return strings.TrimSpace(body)
	}
	// 没有 <summary> 块时，删除可能存在的 <analysis>...</analysis> 块并返回余下内容。
	if start := strings.Index(raw, "<analysis>"); start >= 0 {
		if end := strings.Index(raw, "</analysis>"); end > start {
			return strings.TrimSpace(raw[:start] + raw[end+len("</analysis>"):])
		}
	}
	return strings.TrimSpace(raw)
}
