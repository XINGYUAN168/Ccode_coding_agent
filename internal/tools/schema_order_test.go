package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type orderPlainTool struct{ n string }

func (t *orderPlainTool) Name() string           { return t.n }
func (t *orderPlainTool) Description() string    { return t.n }
func (t *orderPlainTool) Category() ToolCategory { return CategoryRead }
func (t *orderPlainTool) Schema() map[string]any {
	return map[string]any{"name": t.n, "input_schema": map[string]any{"type": "object"}}
}
func (t *orderPlainTool) Execute(context.Context, map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}

type orderDeferTool struct{ n string }

func (t *orderDeferTool) Name() string           { return t.n }
func (t *orderDeferTool) Description() string    { return t.n }
func (t *orderDeferTool) Category() ToolCategory { return CategoryRead }
func (t *orderDeferTool) ShouldDefer() bool      { return true }
func (t *orderDeferTool) Schema() map[string]any {
	return map[string]any{"name": t.n, "input_schema": map[string]any{"type": "object"}}
}
func (t *orderDeferTool) Execute(context.Context, map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}

func schemaNames(schemas []map[string]any) string {
	var out []string
	for _, s := range schemas {
		out = append(out, s["name"].(string))
	}
	return strings.Join(out, ",")
}

// 工具列表的顺序必须在两次调用之间完全一致。
//
// tools 是 map，Go 的 map 遍历顺序是随机的。工具列表渲染在系统提示词之后、消息
// 之前，顺序一变整个块的字节就变了，它后面的对话历史缓存全部作废——内容没动、光
// 顺序变，代价跟真加了一个工具一样。
func TestGetAllSchemasOrderIsStable(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.Register(&orderPlainTool{n: fmt.Sprintf("Tool%02d", i)})
	}
	for i := 0; i < 10; i++ {
		reg.Register(&orderDeferTool{n: fmt.Sprintf("mcp__srv__t%02d", i)})
	}
	reg.McpLoadingMode = McpLoadingNative
	reg.ExposeToolSearch = true

	want := schemaNames(reg.GetAllSchemas("anthropic"))
	for i := 0; i < 100; i++ {
		if got := schemaNames(reg.GetAllSchemas("anthropic")); got != want {
			t.Fatalf("第 %d 次调用顺序变了\n第一次: %s\n这一次: %s", i, want, got)
		}
	}

	// openai 协议也走同一条排序路径
	wantOpenAI := schemaNames(reg.GetAllSchemas("openai"))
	for i := 0; i < 20; i++ {
		if got := schemaNames(reg.GetAllSchemas("openai")); got != wantOpenAI {
			t.Fatalf("openai 协议下第 %d 次顺序变了", i)
		}
	}
}

// 缓存断点不能落在带 defer_loading 的工具上：官方端点不允许一个工具同时带
// defer_loading 和 cache_control，会直接拒掉整个请求。
//
// 这里断言的是排序之后 tools[] 里必然存在非延迟工具可以当落点，且尾部确实可能
// 是延迟工具（也就是「直接标记最后一个」这种做法会踩雷）。真正打标记的逻辑在
// internal/llm/anthropic.go。
func TestNonDeferredToolExistsForCacheMarker(t *testing.T) {
	reg := NewRegistry()
	for _, n := range []string{"ReadFile", "WriteFile", "Bash", "ToolSearch"} {
		reg.Register(&orderPlainTool{n: n})
	}
	for i := 0; i < 5; i++ {
		reg.Register(&orderDeferTool{n: fmt.Sprintf("mcp__srv__z%02d", i)})
	}
	reg.McpLoadingMode = McpLoadingNative
	reg.ExposeToolSearch = true

	schemas := reg.GetAllSchemas("anthropic")
	lastDeferred, _ := schemas[len(schemas)-1]["defer_loading"].(bool)
	if !lastDeferred {
		t.Fatal("这批工具的排序结果里最后一个该是延迟工具，用例前提不成立了")
	}

	// 从尾部往前必须能找到一个非延迟的落点，这是 anthropic.go 的前提
	found := -1
	for i := len(schemas) - 1; i >= 0; i-- {
		if dl, _ := schemas[i]["defer_loading"].(bool); !dl {
			found = i
			break
		}
	}
	if found < 0 {
		t.Fatal("找不到非延迟工具，缓存断点无处可放")
	}
	if name := schemas[found]["name"].(string); strings.HasPrefix(name, MCPToolPrefix) {
		t.Errorf("落点 %s 是 MCP 工具，不该被选中", name)
	}
}
