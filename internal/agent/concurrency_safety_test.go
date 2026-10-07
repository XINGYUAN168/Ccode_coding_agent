package agent

import (
	"context"
	"fmt"
	"testing"

	"ccode/internal/tools"
)

// plainT 是只用来测分批的占位工具，类别可配。
type plainT struct {
	n   string
	cat tools.ToolCategory
}

func (t *plainT) Name() string                 { return t.n }
func (t *plainT) Description() string          { return t.n }
func (t *plainT) Category() tools.ToolCategory { return t.cat }
func (t *plainT) Schema() map[string]any       { return map[string]any{"name": t.n} }
func (t *plainT) Execute(context.Context, map[string]any) tools.ToolResult {
	return tools.ToolResult{}
}

// 并发安全按这一次调用的实际参数算，不是只看工具类别。
//
// ls 和 rm 都是 Bash，前者跟 ReadFile 一样不动外部状态、可以一起并发，
// 后者一旦跟别人并发，执行顺序就不再是模型给出的那个顺序。
func TestBashConcurrencySafetyByCommand(t *testing.T) {
	bash := &tools.BashTool{}

	safe := []string{"ls", "ls -la", "cat a.txt", "git status", "wc -l f", "pwd"}
	for _, cmd := range safe {
		if !tools.IsConcurrencySafe(bash, map[string]any{"command": cmd}) {
			t.Errorf("%q 该算并发安全", cmd)
		}
	}

	unsafe := []string{
		"rm -rf build", "mv a b", "npm install", "git commit -m x",
		"echo hi > f", "ls | wc -l", "ls; rm x", "ls && rm x",
		"echo $(rm x)", "ls `rm x`",
	}
	for _, cmd := range unsafe {
		if tools.IsConcurrencySafe(bash, map[string]any{"command": cmd}) {
			t.Errorf("%q 不该算并发安全", cmd)
		}
	}
}

// 参数缺失或类型不对时按不安全处理，宁可串行也不能猜。
func TestBashConcurrencySafetyBadArgs(t *testing.T) {
	bash := &tools.BashTool{}
	for _, args := range []map[string]any{
		{},
		{"command": nil},
		{"command": 123},
	} {
		if tools.IsConcurrencySafe(bash, args) {
			t.Errorf("args=%v 不该算并发安全", args)
		}
	}
}

// 没实现可选接口的工具按类别兜底，行为跟以前一致。
func TestConcurrencySafetyFallsBackToCategory(t *testing.T) {
	cases := []struct {
		name string
		tool tools.Tool
		want bool
	}{
		{"只读", &plainT{n: "ReadFile", cat: tools.CategoryRead}, true},
		{"写", &plainT{n: "WriteFile", cat: tools.CategoryWrite}, false},
		{"命令", &plainT{n: "Other", cat: tools.CategoryCommand}, false},
	}
	for _, c := range cases {
		if got := tools.IsConcurrencySafe(c.tool, nil); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// 只读的 Bash 要能跟 ReadFile 归到同一个并发批，这是这次改动的目的。
func TestReadOnlyBashBatchesWithReadTools(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&plainT{n: "ReadFile", cat: tools.CategoryRead})
	reg.Register(&plainT{n: "Grep", cat: tools.CategoryRead})
	reg.Register(&tools.BashTool{})

	entries := []toolCallEntry{
		{tc: toolCallInfo{toolID: "a", toolName: "ReadFile"}, index: 0},
		{tc: toolCallInfo{toolID: "b", toolName: "Bash",
			arguments: map[string]any{"command": "git status"}}, index: 1},
		{tc: toolCallInfo{toolID: "c", toolName: "Grep"}, index: 2},
	}

	batches := partitionToolCalls(entries, reg)
	if len(batches) != 1 || !batches[0].concurrent || len(batches[0].calls) != 3 {
		t.Fatalf("该合成一个 3 个调用的并发批，实际 %s", describe(batches))
	}
}

// 会改东西的 Bash 必须把批次断开，前后各自成批。
func TestMutatingBashBreaksTheBatch(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&plainT{n: "ReadFile", cat: tools.CategoryRead})
	reg.Register(&plainT{n: "Grep", cat: tools.CategoryRead})
	reg.Register(&tools.BashTool{})

	entries := []toolCallEntry{
		{tc: toolCallInfo{toolID: "a", toolName: "ReadFile"}, index: 0},
		{tc: toolCallInfo{toolID: "b", toolName: "Bash",
			arguments: map[string]any{"command": "rm -rf build"}}, index: 1},
		{tc: toolCallInfo{toolID: "c", toolName: "Grep"}, index: 2},
	}

	batches := partitionToolCalls(entries, reg)
	want := "[并发:a] [串行:b] [并发:c]"
	if got := describe(batches); got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func describe(batches []toolBatch) string {
	out := ""
	for i, b := range batches {
		if i > 0 {
			out += " "
		}
		kind := "串行"
		if b.concurrent {
			kind = "并发"
		}
		ids := ""
		for j, c := range b.calls {
			if j > 0 {
				ids += ","
			}
			ids += c.tc.toolID
		}
		out += fmt.Sprintf("[%s:%s]", kind, ids)
	}
	return out
}
