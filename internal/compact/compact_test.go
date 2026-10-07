package compact

import (
	"context"
	"strings"
	"testing"

	"ccode/internal/conversation"
	"ccode/internal/llm"
	"ccode/internal/session"
)

// stubSummaryClient 实现 llm.Client 并流式返回固定的 <summary> 块，使 autoCompact
// 的摘要步骤在测试中保持确定性。它会记录收到的摘要提示词，以便测试断言只有前缀
// 被摘要，原样保留的尾部没有进入摘要。
type stubSummaryClient struct {
	summary      string
	lastPrompt   string
	streamCalled bool
}

func (c *stubSummaryClient) SetSystemPrompt(prompt string) {}

func (c *stubSummaryClient) Stream(ctx context.Context, conv *conversation.Manager, tools []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	c.streamCalled = true
	if msgs := conv.GetMessages(); len(msgs) > 0 {
		c.lastPrompt = msgs[len(msgs)-1].Content
	}
	ch := make(chan llm.StreamEvent, 4)
	errCh := make(chan error, 1)
	ch <- llm.TextDelta{Text: "<summary>" + c.summary + "</summary>"}
	ch <- llm.StreamEnd{StopReason: "end_turn"}
	close(ch)
	errCh <- nil
	close(errCh)
	return ch, errCh
}

// 第一层（offload + snip）测试已随实现迁移到 internal/toolresult/budget_test.go。
// compact 现在只负责第二层（autoCompact）和 formatCompactSummary 辅助函数，
// 因此本文件只覆盖这两部分。

func TestFormatCompactSummary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "both blocks present",
			in:   "<analysis>scratch thoughts</analysis>\n<summary>final text</summary>",
			want: "final text",
		},
		{
			name: "summary block unterminated",
			in:   "<analysis>scratch</analysis>\n<summary>tail with no close tag",
			want: "tail with no close tag",
		},
		{
			name: "only analysis block — drop it",
			in:   "prefix <analysis>scratch</analysis> suffix",
			want: "prefix  suffix",
		},
		{
			name: "neither block — return raw",
			in:   "plain text response",
			want: "plain text response",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatCompactSummary(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// EstimateTokens 应覆盖所有内容来源，并能安全处理空消息。
func TestEstimateTokensZeroAndPopulated(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Errorf("empty input should be 0 tokens, got %d", got)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage(strings.Repeat("x", 700))
	got := EstimateTokens(conv.GetMessages())
	if got < 150 || got > 250 {
		t.Errorf("700-char message should estimate ~200 tokens, got %d", got)
	}
}

func TestEstimateTokensIncludesThinking(t *testing.T) {
	withoutThinking := []conversation.Message{{Role: "assistant", Content: "正文"}}
	withThinking := []conversation.Message{{
		Role:           "assistant",
		Content:        "正文",
		ThinkingBlocks: []conversation.ThinkingBlock{{Thinking: strings.Repeat("思", 350)}},
	}}
	if delta := EstimateTokens(withThinking) - EstimateTokens(withoutThinking); delta != 300 {
		t.Fatalf("思考 token 增量 = %d，want 300", delta)
	}
}

// BaselineFromUsage 必须累加四类真实 token 计数，使锚点在缓存命中占主导时
// （input 较小、cache_read 较大）仍能反映真实的提示词与输出总量。
func TestBaselineFromUsage(t *testing.T) {
	u := llm.UsageInfo{
		InputTokens:         100,
		OutputTokens:        40,
		CacheReadTokens:     5000,
		CacheCreationTokens: 200,
	}
	if got, want := BaselineFromUsage(u), 5340; got != want {
		t.Errorf("BaselineFromUsage = %d, want %d", got, want)
	}
	// 兼容端点不报告用量时基线为零，调用方据此不会把它用作锚点。
	if got := BaselineFromUsage(llm.UsageInfo{}); got != 0 {
		t.Errorf("empty usage baseline = %d, want 0", got)
	}
}

// ComputeUsedTokens 在没有锚点（冷启动或首轮）时，必须回退到对全部消息执行完整字符
// 估算，结果与 EstimateTokens 一致。
func TestComputeUsedTokensColdStartFallback(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddUserMessage(strings.Repeat("x", 700))
	conv.AddAssistantMessage(strings.Repeat("y", 700))
	msgs := conv.GetMessages()

	got := ComputeUsedTokens(msgs, UsageAnchor{}) // HasUsage == false
	want := EstimateTokens(msgs)
	if got != want {
		t.Errorf("cold-start ComputeUsedTokens = %d, want full estimate %d", got, want)
	}
}

// ComputeUsedTokens 在存在锚点时，必须返回基线加上 anchorCount 之后新增消息的估算值，
// 不能重新估算整段对话。这正是缓存命中的收益：真实输入远小于锚定前缀的字符估算值。
func TestComputeUsedTokensWithAnchorIncremental(t *testing.T) {
	conv := conversation.NewManager()
	// 三条大型锚定消息的真实 token 开销由基线记录，不能按字符数重新估算。
	conv.AddUserMessage(strings.Repeat("x", 7000))
	conv.AddAssistantMessage(strings.Repeat("y", 7000))
	conv.AddUserMessage(strings.Repeat("z", 7000))
	anchorCount := conv.Len()
	// 在锚点之后追加一条短消息。
	conv.AddAssistantMessage(strings.Repeat("w", 350))
	msgs := conv.GetMessages()

	const baseline = 1500 // 模拟真实 API 返回前缀消耗 1500 个 token
	anchor := UsageAnchor{BaselineTokens: baseline, AnchorCount: anchorCount, HasUsage: true}

	got := ComputeUsedTokens(msgs, anchor)
	wantIncrement := EstimateTokens(msgs[anchorCount:])
	if got != baseline+wantIncrement {
		t.Errorf("anchored ComputeUsedTokens = %d, want baseline+increment %d", got, baseline+wantIncrement)
	}
	// 合理性检查：增量结果必须远低于对高缓存命中对话执行完整字符估算的结果，
	// 以证明没有重新估算前缀。
	if full := EstimateTokens(msgs); got >= full {
		t.Errorf("anchored result %d should be below full estimate %d", got, full)
	}
}

// ComputeUsedTokens 遇到过期锚点时不得 panic，并应回退到完整估算。例如压缩重写对话后，
// AnchorCount 可能超过当前消息数量。
func TestComputeUsedTokensStaleAnchorClamp(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddUserMessage("hi")
	msgs := conv.GetMessages()

	anchor := UsageAnchor{BaselineTokens: 9999, AnchorCount: 50, HasUsage: true}
	got := ComputeUsedTokens(msgs, anchor)
	if want := EstimateTokens(msgs); got != want {
		t.Errorf("stale-anchor ComputeUsedTokens = %d, want full estimate %d", got, want)
	}
}

// bigMsg 返回一条仅正文就约等于指定 token 数的消息（recoveryCharsPerToken 约为
// 3.5 字符/token），让测试可以确定性地驱动 keepRecentTokens 预算遍历。
func bigMsg(tokens int) string {
	return strings.Repeat("x", tokens*4)
}

// containsMsg 判断 msgs 中是否存在正文等于 want 的消息。
func containsMsg(msgs []conversation.Message, want string) bool {
	for _, m := range msgs {
		if m.Content == want {
			return true
		}
	}
	return false
}

// autoCompact 必须原样保留近期尾部，不能用摘要替换。测试构造足够大的旧前缀以确保
// keepStart > 0，并在尾部放入易识别内容；压缩后尾部内容仍必须存在，而不能只剩摘要。
func TestAutoCompactKeepsRecentVerbatim(t *testing.T) {
	conv := conversation.NewManager()
	// 旧前缀：几条应被摘要替换的大消息。
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("OLD-PREFIX " + bigMsg(3000))
		conv.AddAssistantMessage("OLD-REPLY " + bigMsg(3000))
	}
	// 近期尾部：预期原样保留的易识别短消息。
	recent := []string{"RECENT-A unique-marker-A", "RECENT-B unique-marker-B"}
	conv.AddUserMessage(recent[0])
	conv.AddAssistantMessage(recent[1])

	client := &stubSummaryClient{summary: "THE SUMMARY"}
	msg, err := autoCompact(context.Background(), conv, client, "", "", 200000, nil, nil)
	if err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if msg == "" {
		t.Fatalf("expected a compaction message, got empty (degraded to no-op)")
	}
	out := conv.GetMessages()

	// 摘要必须存在。
	var sawSummary bool
	for _, m := range out {
		if strings.Contains(m.Content, "THE SUMMARY") {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Errorf("summary not present after compaction")
	}
	// 近期尾部必须原样保留，不能折叠进摘要。
	for _, r := range recent {
		if !containsMsg(out, r) {
			t.Errorf("recent message %q not preserved verbatim after compaction; messages=%v", r, msgContents(out))
		}
	}
}

// 接入 sessionID 和 workDir 后，autoCompact 必须把 compact_boundary 记录写入会话日志，
// 其中内联摘要及保留尾部的角色和正文。这是恢复往返流程的磁盘侧；随后
// session.FindLastCompactBoundary 会据此重建压缩状态。
func TestAutoCompactPersistsBoundary(t *testing.T) {
	conv := conversation.NewManager()
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("OLD-PREFIX " + bigMsg(3000))
		conv.AddAssistantMessage("OLD-REPLY " + bigMsg(3000))
	}
	conv.AddUserMessage("RECENT-TAIL-USER unique-marker-A")
	conv.AddAssistantMessage("RECENT-TAIL-ASSISTANT unique-marker-B")

	workDir := t.TempDir()
	sid := "compact-roundtrip"
	client := &stubSummaryClient{summary: "PERSISTED-SUMMARY"}

	msg, err := autoCompact(context.Background(), conv, client, workDir, sid, 200000, nil, nil)
	if err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if msg == "" {
		t.Fatalf("expected a compaction message, got empty (degraded to no-op)")
	}

	// 读回会话日志，断言边界已写入且内联了摘要与保留尾部。
	msgs := session.LoadSession(workDir, sid)
	boundary, after, ok := session.FindLastCompactBoundary(msgs)
	if !ok {
		t.Fatalf("expected a compact_boundary record to be persisted")
	}
	if boundary.Summary != "PERSISTED-SUMMARY" {
		t.Fatalf("persisted summary mismatch: got %q", boundary.Summary)
	}
	if len(after) != 0 {
		t.Fatalf("no messages should follow a freshly written boundary, got %d", len(after))
	}
	// 保留尾部必须原样内联进边界。
	var sawTailUser, sawTailAssistant bool
	for _, k := range boundary.Keep {
		if k.Content == "RECENT-TAIL-USER unique-marker-A" {
			sawTailUser = true
		}
		if k.Content == "RECENT-TAIL-ASSISTANT unique-marker-B" {
			sawTailAssistant = true
		}
	}
	if !sawTailUser || !sawTailAssistant {
		t.Fatalf("kept tail not inlined into boundary: %+v", boundary.Keep)
	}
	// 边界中的保留尾部必须与 autoCompact 原样保留的对话尾部完全相等，即角色、正文和
	// 顺序都一致。磁盘只保存纯摘要文本；内存重建后的对话为“摘要用户消息 + 继续确认 +
	// 保留尾部”，因此保留尾部位于重建对话末尾。
	rebuilt := conv.GetMessages()
	tail := rebuilt[len(rebuilt)-len(boundary.Keep):]
	for i, k := range boundary.Keep {
		if tail[i].Role != k.Role || tail[i].Content != k.Content {
			t.Fatalf("boundary keep[%d]=%+v does not match in-memory tail %+v", i, k, tail[i])
		}
	}
}

// 没有 sessionID 或 workDir 时，autoCompact 不得改动任何会话日志；单次调用方、测试和
// 子 Agent 的行为应保持不变。
func TestAutoCompactNoSessionNoBoundary(t *testing.T) {
	conv := conversation.NewManager()
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("OLD-PREFIX " + bigMsg(3000))
		conv.AddAssistantMessage("OLD-REPLY " + bigMsg(3000))
	}
	conv.AddUserMessage("RECENT-A")
	conv.AddAssistantMessage("RECENT-B")

	workDir := t.TempDir()
	client := &stubSummaryClient{summary: "S"}
	// sessionID 为空时不持久化。
	if _, err := autoCompact(context.Background(), conv, client, workDir, "", 200000, nil, nil); err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	// 任何标识下都不应创建会话文件。
	msgs := session.LoadSession(workDir, "anything")
	if len(msgs) != 0 {
		t.Fatalf("expected no session log written when sessionID empty, got %d", len(msgs))
	}
}

// autoCompact 只能摘要 messages[:keepStart]；保留尾部不得出现在交给摘要器的提示词中。
func TestAutoCompactSummaryOnlyCoversPrefix(t *testing.T) {
	conv := conversation.NewManager()
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("PREFIX-ONLY-CONTENT " + bigMsg(3000))
		conv.AddAssistantMessage("PREFIX-ONLY-REPLY " + bigMsg(3000))
	}
	conv.AddUserMessage("TAIL-ONLY-MARKER")
	conv.AddAssistantMessage("TAIL-ONLY-REPLY")

	client := &stubSummaryClient{summary: "S"}
	if _, err := autoCompact(context.Background(), conv, client, "", "", 200000, nil, nil); err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if !client.streamCalled {
		t.Fatalf("summarizer was never called")
	}
	if strings.Contains(client.lastPrompt, "TAIL-ONLY-MARKER") {
		t.Errorf("summary prompt must not include the kept tail, but it did")
	}
	if !strings.Contains(client.lastPrompt, "PREFIX-ONLY-CONTENT") {
		t.Errorf("summary prompt must include the summarized prefix, but it did not")
	}
}

// computeKeepStartIndex 绝不能拆开 tool_use 与 tool_result 配对：若预算边界落在携带
// tool_results 的用户消息上，边界必须前移并包含生成这些结果的助手 tool_use 消息。
func TestComputeKeepStartIndexDoesNotSplitToolPair(t *testing.T) {
	conv := conversation.NewManager()
	// 使用大型前缀确保 keepStart > 0。
	for i := 0; i < 8; i++ {
		conv.AddUserMessage(bigMsg(3000))
		conv.AddAssistantMessage(bigMsg(3000))
	}
	// 在尾部附近放置一组 tool_use/tool_result；tool_result 是大型消息，
	// 使预算边界很可能恰好落在它上面。
	conv.AddToolUseMessage("calling tool", "tu-1", "ReadFile", map[string]any{"path": "/x"})
	conv.AddToolResultMessage("tu-1", bigMsg(9000), false)
	msgs := conv.GetMessages()

	keepStart := computeKeepStartIndex(msgs)
	if keepStart <= 0 || keepStart >= len(msgs) {
		t.Fatalf("keepStart=%d out of expected range (0, %d)", keepStart, len(msgs))
	}
	// 边界消息不得是孤立的 tool_result，而把匹配的 tool_use 留在已摘要前缀中。
	if hasToolResults(msgs[keepStart]) {
		t.Fatalf("keepStart landed on a tool_result message (orphaned); keepStart=%d", keepStart)
	}
	// 验证配对完整保留在尾部：遍历尾部，确保每个 tool_result 前都有对应的 tool_use。
	keep := msgs[keepStart:]
	openUses := map[string]bool{}
	for _, m := range keep {
		for _, tu := range m.ToolUses {
			openUses[tu.ToolUseID] = true
		}
		for _, tr := range m.ToolResults {
			if !openUses[tr.ToolUseID] {
				t.Errorf("tool_result %s in kept tail has no matching tool_use in tail (pair split)", tr.ToolUseID)
			}
		}
	}
}

// 对话太短、没有可摘要前缀时（keepStart <= 0），autoCompact 必须退化为空操作：
// 不执行摘要，也不改动对话。
func TestAutoCompactDegradesWhenTooFewMessages(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddUserMessage("just one")
	conv.AddAssistantMessage("two")
	before := conv.GetMessages()

	client := &stubSummaryClient{summary: "S"}
	msg, err := autoCompact(context.Background(), conv, client, "", "", 200000, nil, nil)
	if err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if msg != "" {
		t.Errorf("expected no-op (empty message) for too-few messages, got %q", msg)
	}
	if client.streamCalled {
		t.Errorf("summarizer should not be called when degrading to no-op")
	}
	after := conv.GetMessages()
	if len(before) != len(after) {
		t.Errorf("conversation changed during no-op: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i].Content != after[i].Content {
			t.Errorf("message %d mutated during no-op", i)
		}
	}
}

func msgContents(msgs []conversation.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		c := m.Content
		if len(c) > 40 {
			c = c[:40] + "..."
		}
		out[i] = c
	}
	return out
}
