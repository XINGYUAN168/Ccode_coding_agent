package agent

import (
	"context"
	"sync"

	"ccode/internal/tools"
)

// toolBatch 是 partitionToolCalls 的输出：一批工具调用和是否可以并发执行的标记。
type toolBatch struct {
	concurrent bool
	calls      []toolCallEntry
}

type toolCallEntry struct {
	tc    toolCallInfo
	index int // 在原始 toolCalls 列表中的位置，保证结果顺序
}

type toolCallInfo struct {
	toolID    string
	toolName  string
	arguments map[string]any
}

// StreamingExecutor 按安全性分批执行工具调用：
// 连续的只读工具（category == "read"）合并为一批并发执行，
// 写/命令工具各自独占一批串行执行。
type StreamingExecutor struct {
	registry *tools.Registry
	eventCh  chan AgentEvent

	mu      sync.Mutex
	calls   []toolCallInfo
	results []toolExecResult
}

func NewStreamingExecutor(registry *tools.Registry, eventCh chan AgentEvent) *StreamingExecutor {
	return &StreamingExecutor{
		registry: registry,
		eventCh:  eventCh,
	}
}

// Submit 收集一个待执行的工具调用（不立即执行）。
func (se *StreamingExecutor) Submit(tc toolCallInfo) {
	se.mu.Lock()
	se.calls = append(se.calls, tc)
	se.mu.Unlock()
}

// ExecuteAll 对收集到的工具调用做分批，然后按批次顺序执行：
// 只读批并发，写/命令批串行。结果按原始提交顺序返回。
func (se *StreamingExecutor) ExecuteAll(ctx context.Context, agent *Agent) []toolExecResult {
	se.mu.Lock()
	calls := append([]toolCallInfo(nil), se.calls...)
	se.mu.Unlock()

	results := make([]toolExecResult, len(calls))

	// 为每个 call 记录原始索引，分批后能按原顺序回填
	var entries []toolCallEntry
	for i, c := range calls {
		entries = append(entries, toolCallEntry{tc: c, index: i})
	}

	batches := partitionToolCalls(entries, se.registry)

	for _, batch := range batches {
		if batch.concurrent && len(batch.calls) > 1 {
			// 只读批：并发执行
			var wg sync.WaitGroup
			for _, entry := range batch.calls {
				wg.Add(1)
				go func(e toolCallEntry) {
					defer wg.Done()
					r := agent.executeSingleTool(ctx, se.eventCh, e.tc)
					se.mu.Lock()
					results[e.index] = r
					se.mu.Unlock()
				}(entry)
			}
			wg.Wait()
		} else {
			// 写/命令批：串行执行
			for _, entry := range batch.calls {
				r := agent.executeSingleTool(ctx, se.eventCh, entry.tc)
				results[entry.index] = r
			}
		}
	}

	se.results = results
	return results
}

// partitionToolCalls 将工具调用按相邻性分批：
// 连续的并发安全的调用归为一个并发批次，其余各自独占一个串行批次。
//
// 安不安全按这一次调用的实际参数算，不是只看工具类别。ls 和 rm 都是 Bash，
// 前者可以跟 ReadFile 一起并发，后者必须独占。
func partitionToolCalls(entries []toolCallEntry, registry *tools.Registry) []toolBatch {
	var batches []toolBatch
	for _, entry := range entries {
		tool := registry.Get(entry.tc.toolName)
		safe := tool != nil && tools.IsConcurrencySafe(tool, entry.tc.arguments)

		if safe && len(batches) > 0 && batches[len(batches)-1].concurrent {
			batches[len(batches)-1].calls = append(batches[len(batches)-1].calls, entry)
		} else {
			batches = append(batches, toolBatch{
				concurrent: safe,
				calls:      []toolCallEntry{entry},
			})
		}
	}
	return batches
}

// HasPending 表示是否已经提交了工具调用。
func (se *StreamingExecutor) HasPending() bool {
	se.mu.Lock()
	defer se.mu.Unlock()
	return len(se.calls) > 0
}

// Reset 清空执行器状态，为下一轮调用做准备。
func (se *StreamingExecutor) Reset() {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.calls = nil
	se.results = nil
}
