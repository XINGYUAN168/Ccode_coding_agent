// Package extractor 实现后台记忆提取子 Agent。
//
// Extractor 使用结构体和 sync.Mutex 封装状态，使每个调用方拥有独立实例，测试也能
// 安全替换 Deps。TUI 把调用 (*Extractor).Execute 的闭包设置到
// agent.Agent.OnLoopComplete；Agent 循环在每次 LoopComplete 后以即发即弃方式触发。
// Extractor 再通过 runExtraction 启动后台 goroutine，因此 Execute 可以快速返回。
package extractor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccode/internal/agent"
	"ccode/internal/agents"
	"ccode/internal/conversation"
	"ccode/internal/llm"
	"ccode/internal/memory"
	"ccode/internal/tools"
)

// Deps 保存 Extractor 所需的外部协作者。TUI 启动时构造一个 Deps，包装为 Extractor，
// 再把其 Execute 方法挂接到 agent.Agent.OnLoopComplete。
//
// AppendSystem 用于在提取成功后向用户显示 "Memory saved: foo.md" 通知。
type Deps struct {
	MemoryDir     string                           // <wd>/.ccode/memory/：项目/参考记忆（末尾带分隔符）
	UserMemoryDir string                           // ~/.ccode/memory/：用户/反馈记忆（末尾带分隔符）；无法解析用户目录时可能为 ""
	ProjectRoot   string                           // 项目根目录的绝对路径
	Client        llm.Client                       // fork 提取 Agent 的 LLM 客户端
	ToolRegistry  *tools.Registry                  // 父工具注册表，后续会过滤
	Protocol      string                           // "anthropic" / "openai"
	Conversation  *conversation.Manager            // 父对话引用
	AppendSystem  func(string)                     // 可选：通知 TUI 已保存的记忆
	DebugLogf     func(format string, args ...any) // 可选：调试日志
}

// Extractor 是后台记忆提取器。mu 保护所有可变字段；每个实例彼此独立，测试可以
// 使用模拟 Deps 构造实例而不影响全局状态。lastMemoryMessageIdx 记录上次成功提取时
// 父会话的消息索引，pendingContext 表示当前提取结束后还需执行一次尾随提取。
type Extractor struct {
	deps Deps

	mu                       sync.Mutex
	inFlight                 map[*sync.WaitGroup]struct{}
	lastMemoryMessageIdx     int
	hasLoggedGateFailure     bool
	inProgress               bool
	turnsSinceLastExtraction int
	pendingContext           *pendingExtractionCtx
}

// pendingExtractionCtx 是尾随提取的占位状态。全部数据已保存在 Extractor 中，因此
// 无需携带字段；非 nil 仅表示“当前提取结束后再执行一次”。
type pendingExtractionCtx struct{}

// InitExtractMemories 使用给定 Deps 构造新的 Extractor。
func InitExtractMemories(deps Deps) *Extractor {
	return &Extractor{
		deps:     deps,
		inFlight: make(map[*sync.WaitGroup]struct{}),
	}
}

// Execute 是即发即弃入口，由 TUI 挂接到 agent.Agent.OnLoopComplete。它会快速返回；
// 实际提取工作在调用方 goroutine 中进行。错误按尽力而为原则吞掉，因为调用方
// （Agent 循环）会忽略返回值。
func (e *Extractor) Execute(ctx context.Context) error {
	if e == nil {
		return nil
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	e.mu.Lock()
	e.inFlight[wg] = struct{}{}
	e.mu.Unlock()
	defer func() {
		wg.Done()
		e.mu.Lock()
		delete(e.inFlight, wg)
		e.mu.Unlock()
	}()

	return e.executeImpl(ctx)
}

func (e *Extractor) executeImpl(ctx context.Context) error {
	// 合并进行中的调用：若已有提取任务在运行，把本次调用保存为待执行的尾随任务并立即返回。
	// pendingContext 与 runExtraction 的 finally 逻辑组成尾随执行链。
	e.mu.Lock()
	if e.inProgress {
		e.deps.debugf("[extractMemories] extraction in progress — stashing for trailing run")
		e.pendingContext = &pendingExtractionCtx{}
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()

	return e.runExtraction(ctx, false)
}

func (e *Extractor) runExtraction(ctx context.Context, isTrailingRun bool) error {
	messages := e.deps.Conversation.GetMessages()
	newMessageCount := countModelVisibleMessagesSince(messages, e.lastMemoryMessageIdx)

	// 互斥规则：主 Agent 已自行写入记忆时，无需再运行 fork 提取器。
	// 将游标推进到本段之后并返回。
	if hasMemoryWritesSince(messages, e.lastMemoryMessageIdx, e.deps.ProjectRoot) {
		e.deps.debugf("[extractMemories] skipping — conversation already wrote to memory files")
		e.advanceCursor(len(messages))
		return nil
	}

	// 节流默认值为 1，即每轮运行。Ccode 固定使用 1；尾随任务处理的是已提交工作，
	// 因此绕过节流。
	if !isTrailingRun {
		e.turnsSinceLastExtraction++
		if e.turnsSinceLastExtraction < 1 {
			return nil
		}
	}
	e.turnsSinceLastExtraction = 0

	e.mu.Lock()
	e.inProgress = true
	e.mu.Unlock()
	startTime := time.Now()

	defer func() {
		e.mu.Lock()
		e.inProgress = false
		trailing := e.pendingContext
		e.pendingContext = nil
		e.mu.Unlock()
		if trailing != nil {
			e.deps.debugf("[extractMemories] running trailing extraction for stashed context")
			_ = e.runExtraction(ctx, true)
		}
	}()

	e.deps.debugf("[extractMemories] starting — %d new messages, memoryDir=%s, userMemoryDir=%s",
		newMessageCount, e.deps.MemoryDir, e.deps.UserMemoryDir)

	// 预先注入记忆目录清单，避免提取 Agent 浪费一轮执行 `ls`。同时扫描两个目录，
	// 让用户级与项目级记忆出现在同一份清单中。
	var combinedScan []memory.MemoryHeader
	if e.deps.UserMemoryDir != "" {
		userScan, _ := memory.ScanMemoryFiles(ctx, e.deps.UserMemoryDir, "user")
		combinedScan = append(combinedScan, userScan...)
	}
	projectScan, _ := memory.ScanMemoryFiles(ctx, e.deps.MemoryDir, "project")
	combinedScan = append(combinedScan, projectScan...)
	manifest := memory.FormatMemoryManifest(combinedScan)
	extractionPrompt := BuildExtractAutoOnlyPrompt(newMessageCount, manifest, false, e.deps.UserMemoryDir, e.deps.MemoryDir)

	// 构造 fork 对话：复制父消息，再把提取提示词追加为新的用户消息。这里有意不添加
	// agents.runFork 的 fork 模板；提取器是主对话的“完整 fork”，也不会注入额外系统指令。
	forkedConv := buildExtractorConversation(e.deps.Conversation, extractionPrompt)

	// 工具白名单包括 ReadFile、WriteFile、EditFile、Glob、Grep、Bash、ToolSearch，
	// 由 FilterToolsForAgent 的异步路径过滤；Agent 和 AskUserQuestion 会自动排除。
	subRegistry := agents.FilterToolsForAgent(e.deps.ToolRegistry, nil, nil, true)

	// 严格路径沙箱：文件工具只能访问 memoryDir。该限制比原 createAutoMemCanUseTool 更严格，
	// 后者允许 Read、Grep、Glob 任意读取；不过它符合提示词中禁止搜索源码的明确要求，
	// 因而对行为影响很小，同时显著改善安全性。
	//
	// 使用 ModeBypass，使文件和命令工具不会进入 Ask 路径；提取器在后台运行，
	// 没有 TUI 可以回答权限请求。
	subChecker := memory.NewSubAgentChecker(e.deps.ProjectRoot, e.deps.UserMemoryDir)

	subAgent := agent.New(e.deps.Client, subRegistry, e.deps.Protocol)
	subAgent.MaxIterations = 5
	subAgent.Checker = subChecker
	subAgent.WorkDir = e.deps.ProjectRoot

	// 驱动 fork Agent 运行到完成。排空事件通道以便循环正常退出；不展示流式文本，
	// 这里只关心文件写入。
	ch := subAgent.Run(ctx, forkedConv)
	for range ch {
		// 排空通道，不把子 Agent 事件传播到 UI。
	}

	// 只在运行结束后推进游标；无论是否写入文件，都不应再次考虑“已运行但未选中内容”的轮次。
	e.advanceCursor(len(messages))

	writtenPaths := extractWrittenPaths(forkedConv.GetMessages())
	e.deps.debugf("[extractMemories] finished in %s, %d files written: %v",
		time.Since(startTime), len(writtenPaths), writtenPaths)

	// 索引文件（MEMORY.md）只是机械更新；用户可见的“记忆”是主题文件，而不是索引更新。
	var memoryPaths []string
	for _, p := range writtenPaths {
		if filepath.Base(p) == memory.AutoMemEntrypointName {
			continue
		}
		memoryPaths = append(memoryPaths, p)
	}

	if len(memoryPaths) > 0 && e.deps.AppendSystem != nil {
		var names []string
		for _, p := range memoryPaths {
			names = append(names, filepath.Base(p))
		}
		e.deps.AppendSystem(fmt.Sprintf("Memory saved: %s", strings.Join(names, ", ")))
	}

	return nil
}

// Drain 在软超时范围内等待所有进行中的提取完成，包括待执行的尾随任务。
// TUI 关闭路径应调用它，避免 fork 提取 Agent 在写入途中被终止。
//
// timeoutMs 为 0 时，若仍有任务进行就立即返回；负值按 60000（默认 60 秒）处理。
func (e *Extractor) Drain(timeoutMs int) error {
	if e == nil {
		return nil
	}
	if timeoutMs < 0 {
		timeoutMs = 60000
	}

	e.mu.Lock()
	wgs := make([]*sync.WaitGroup, 0, len(e.inFlight))
	for wg := range e.inFlight {
		wgs = append(wgs, wg)
	}
	e.mu.Unlock()

	if len(wgs) == 0 {
		return nil
	}

	done := make(chan struct{})
	go func() {
		for _, wg := range wgs {
			wg.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil
	}
}

func (e *Extractor) advanceCursor(to int) {
	e.mu.Lock()
	if to > e.lastMemoryMessageIdx {
		e.lastMemoryMessageIdx = to
	}
	e.mu.Unlock()
}

// countModelVisibleMessagesSince 统计 sinceIdx 之后新增的用户和助手消息。sinceIdx 越界时
// （例如对话已压缩，所记录游标无法对应当前位置），回退为统计所有模型可见消息，
// 即 "if !foundStart" 恢复路径。
func countModelVisibleMessagesSince(messages []conversation.Message, sinceIdx int) int {
	if sinceIdx < 0 || sinceIdx > len(messages) {
		return countModelVisible(messages)
	}
	n := 0
	for _, m := range messages[sinceIdx:] {
		if isModelVisibleMessage(m) {
			n++
		}
	}
	return n
}

func countModelVisible(messages []conversation.Message) int {
	n := 0
	for _, m := range messages {
		if isModelVisibleMessage(m) {
			n++
		}
	}
	return n
}

func isModelVisibleMessage(m conversation.Message) bool {
	return m.Role == "user" || m.Role == "assistant"
}

// hasMemoryWritesSince 在 sinceIdx 之后任一助手消息含有指向自动记忆路径的 Write/Edit
// tool_use 时返回 true。此时 runExtraction 会跳过 Agent fork。
func hasMemoryWritesSince(messages []conversation.Message, sinceIdx int, projectRoot string) bool {
	if sinceIdx < 0 {
		sinceIdx = 0
	}
	if sinceIdx >= len(messages) {
		return false
	}
	for _, m := range messages[sinceIdx:] {
		if m.Role != "assistant" {
			continue
		}
		for _, tu := range m.ToolUses {
			fp := getWrittenFilePath(tu)
			if fp == "" {
				continue
			}
			if memory.IsAutoMemPath(fp, projectRoot) {
				return true
			}
		}
	}
	return false
}

// getWrittenFilePath 从 Write/Edit tool_use 块提取 file_path 参数；不是此类调用时返回 ""。
func getWrittenFilePath(tu conversation.ToolUseBlock) string {
	if tu.ToolName != "WriteFile" && tu.ToolName != "EditFile" {
		return ""
	}
	fp, ok := tu.Arguments["file_path"].(string)
	if !ok {
		return ""
	}
	return fp
}

// extractWrittenPaths 收集 fork Agent 助手消息中每个 Write/Edit tool_use 的唯一 file_path 参数，
// 以首次出现为准。
func extractWrittenPaths(messages []conversation.Message) []string {
	var paths []string
	seen := make(map[string]struct{})
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tu := range m.ToolUses {
			fp := getWrittenFilePath(tu)
			if fp == "" {
				continue
			}
			if _, ok := seen[fp]; ok {
				continue
			}
			seen[fp] = struct{}{}
			paths = append(paths, fp)
		}
	}
	return paths
}

// buildExtractorConversation 把父对话消息复制到新的 Manager，并把提取提示词追加为最后一条
// 用户消息。它与 agents.buildForkedConversation 不同，不注入 ForkBoilerplateTag；
// 提取器是完整 fork，而不是“你是子 Agent”式 fork。
func buildExtractorConversation(parent *conversation.Manager, prompt string) *conversation.Manager {
	forked := conversation.NewManager()
	for _, msg := range parent.GetMessages() {
		switch msg.Role {
		case "assistant":
			if len(msg.ToolUses) > 0 {
				forked.AddAssistantMessageWithTools(msg.Content, msg.ToolUses)
			} else {
				forked.AddAssistantMessage(msg.Content)
			}
		default:
			if len(msg.ToolResults) > 0 {
				forked.AddToolResultsMessage(msg.ToolResults)
			} else {
				forked.AddUserMessage(msg.Content)
			}
		}
	}
	forked.AddUserMessage(prompt)
	return forked
}

func (d Deps) debugf(format string, args ...any) {
	if d.DebugLogf != nil {
		d.DebugLogf(format, args...)
	}
}
