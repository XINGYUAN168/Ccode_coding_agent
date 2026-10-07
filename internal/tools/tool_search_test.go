package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mockDeferredTool 模拟一个会被 defer 的工具
type mockDeferredTool struct {
	name string
	desc string
}

func (t *mockDeferredTool) Name() string           { return t.name }
func (t *mockDeferredTool) Description() string    { return t.desc }
func (t *mockDeferredTool) Category() ToolCategory { return CategoryCommand }
func (t *mockDeferredTool) ShouldDefer() bool      { return true }

func (t *mockDeferredTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.name,
		"description": t.desc,
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"arg1": map[string]any{"type": "string"}},
		},
	}
}
func (t *mockDeferredTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}

// mockNormalTool 模拟一个不会被 defer 的普通工具
type mockNormalTool struct {
	name string
}

func (t *mockNormalTool) Name() string           { return t.name }
func (t *mockNormalTool) Description() string    { return "normal tool" }
func (t *mockNormalTool) Category() ToolCategory { return CategoryRead }
func (t *mockNormalTool) Schema() map[string]any {
	return map[string]any{
		"name":         t.name,
		"description":  "normal tool",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}
func (t *mockNormalTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}

// mockMCPTool 模拟 MCPToolWrapper（没实现 DeferrableTool 接口）
type mockMCPTool struct {
	name string
	desc string
}

func (t *mockMCPTool) Name() string           { return t.name }
func (t *mockMCPTool) Description() string    { return t.desc }
func (t *mockMCPTool) Category() ToolCategory { return CategoryCommand }
func (t *mockMCPTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.name,
		"description": t.desc,
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"expr": map[string]any{"type": "string"}},
		},
	}
}
func (t *mockMCPTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}

func TestDeferredToolsNotInGetAllSchemas(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockNormalTool{name: "ReadFile"})
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})
	reg.Register(&mockDeferredTool{name: "mcp__grafana__search", desc: "Search dashboards"})

	schemas := reg.GetAllSchemas("anthropic")

	// 只应该有 ReadFile，deferred 的不应该出现
	if len(schemas) != 1 {
		t.Errorf("expected 1 schema (only ReadFile), got %d", len(schemas))
	}
	if schemas[0]["name"] != "ReadFile" {
		t.Errorf("expected ReadFile, got %s", schemas[0]["name"])
	}
}

func TestMCPToolIsDeferred(t *testing.T) {
	// mockMCPTool 没实现 DeferrableTool，模拟旧行为（不 defer）
	// 真正的 MCPToolWrapper 现在实现了 ShouldDefer() = true
	reg := NewRegistry()
	reg.Register(&mockNormalTool{name: "ReadFile"})
	reg.Register(&mockMCPTool{name: "mcp__grafana__query", desc: "Query Prometheus"})

	schemas := reg.GetAllSchemas("anthropic")
	// mockMCPTool 没实现 DeferrableTool，所以仍然全量传
	if len(schemas) != 2 {
		t.Errorf("expected 2 schemas (mockMCPTool has no ShouldDefer), got %d", len(schemas))
	}

	// 但如果用 mockDeferredTool 模拟真正的 MCPToolWrapper 行为
	reg2 := NewRegistry()
	reg2.Register(&mockNormalTool{name: "ReadFile"})
	reg2.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})

	schemas2 := reg2.GetAllSchemas("anthropic")
	if len(schemas2) != 1 {
		t.Errorf("expected 1 schema (deferred MCP tool excluded), got %d", len(schemas2))
	}

	deferred := reg2.GetDeferredTools()
	if len(deferred) != 1 {
		t.Errorf("expected 1 deferred tool, got %d", len(deferred))
	}
}

func TestDiscoveredToolsIncludedInSchemas(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockNormalTool{name: "ReadFile"})
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})
	reg.Register(&mockDeferredTool{name: "mcp__grafana__search", desc: "Search dashboards"})

	// 发现前：只有 ReadFile。
	schemas := reg.GetAllSchemas("anthropic")
	if len(schemas) != 1 {
		t.Errorf("before discovery: expected 1 schema, got %d", len(schemas))
	}

	// 发现一个工具。
	reg.MarkDiscovered("mcp__grafana__query")

	// 发现后：ReadFile 加上已发现工具。
	schemas = reg.GetAllSchemas("anthropic")
	if len(schemas) != 2 {
		t.Errorf("after discovery: expected 2 schemas, got %d", len(schemas))
	}

	// GetDeferredToolNames 应只返回尚未发现的工具。
	names := reg.GetDeferredToolNames()
	if len(names) != 1 || names[0] != "mcp__grafana__search" {
		t.Errorf("expected only mcp__grafana__search as deferred, got %v", names)
	}
}

// MCP 工具不再被标记为已发现：它们永远不进 tools[]，调用走 mcp_call。
// 这样 tools 数组在整场会话里字节不变，prompt cache 的前缀不会被打断。
func TestToolSearchDoesNotMarkMCPToolsDiscovered(t *testing.T) {
	reg := NewRegistry()
	reg.McpLoadingMode = McpLoadingDispatch
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})

	ts := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	res := ts.Execute(context.Background(), map[string]any{"query": "select:mcp__grafana__query"})

	if reg.IsDiscovered("mcp__grafana__query") {
		t.Error("MCP 工具不该被标记为已发现")
	}
	if len(reg.GetAllSchemas("anthropic")) != 0 {
		t.Error("MCP 工具不该出现在 tools[] 里")
	}
	// schema 原文要给模型看到，否则它不知道参数怎么填
	if !strings.Contains(res.Output, "mcp__grafana__query") {
		t.Error("输出里应该含工具的 schema")
	}
	// 还要告诉它走哪个入口
	if !strings.Contains(res.Output, "mcp_call") {
		t.Error("输出里应该提示用 mcp_call 调用")
	}
}

// 非 MCP 的延迟工具没有 mcp_call 这条入口，仍走老办法进 tools[]。
func TestToolSearchStillMarksNonMCPDeferredTools(t *testing.T) {
	reg := NewRegistry()
	reg.McpLoadingMode = McpLoadingDispatch
	reg.Register(&mockDeferredTool{name: "SomeDeferredTool", desc: "not an MCP tool"})

	ts := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	ts.Execute(context.Background(), map[string]any{"query": "select:SomeDeferredTool"})

	if !reg.IsDiscovered("SomeDeferredTool") {
		t.Error("非 MCP 的延迟工具仍该被标记为已发现")
	}
	if len(reg.GetAllSchemas("anthropic")) != 1 {
		t.Error("标记之后该出现在 tools[] 里")
	}
}

// 官方端点下回 tool_reference，由服务端展开 schema，tools 数组不动。
func TestToolSearchReturnsToolReferenceInNativeMode(t *testing.T) {
	reg := NewRegistry()
	reg.McpLoadingMode = McpLoadingNative
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})

	ts := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	res := ts.Execute(context.Background(), map[string]any{"query": "select:mcp__grafana__query"})

	if len(res.ContentBlocks) != 1 {
		t.Fatalf("期望 1 个 content block，实际 %d", len(res.ContentBlocks))
	}
	if res.ContentBlocks[0]["type"] != "tool_reference" {
		t.Errorf("block 类型应为 tool_reference，实际 %v", res.ContentBlocks[0]["type"])
	}
	if res.ContentBlocks[0]["tool_name"] != "mcp__grafana__query" {
		t.Errorf("tool_name 不对：%v", res.ContentBlocks[0]["tool_name"])
	}
	// native 下工具带 defer_loading 留在数组里，交给服务端决定可见性
	schemas := reg.GetAllSchemas("anthropic")
	if len(schemas) != 1 || schemas[0]["defer_loading"] != true {
		t.Errorf("native 模式下应带 defer_loading 保留在 tools[] 里，实际 %v", schemas)
	}
}

func TestToolSearchSelect(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})
	reg.Register(&mockDeferredTool{name: "mcp__grafana__search", desc: "Search dashboards"})

	ts := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	result := ts.Execute(context.Background(), map[string]any{
		"query": "select:mcp__grafana__query",
	})

	if result.IsError {
		t.Errorf("ToolSearch returned error: %s", result.Output)
	}
	if !contains(result.Output, "mcp__grafana__query") {
		t.Errorf("expected output to contain tool name, got: %s", result.Output)
	}
}

func TestToolSearchKeyword(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus metrics"})
	reg.Register(&mockDeferredTool{name: "mcp__github__issues", desc: "List GitHub issues"})

	ts := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	result := ts.Execute(context.Background(), map[string]any{
		"query": "prometheus",
	})

	if result.IsError {
		t.Errorf("ToolSearch returned error: %s", result.Output)
	}
	if !contains(result.Output, "mcp__grafana__query") {
		t.Errorf("expected to find grafana query tool, got: %s", result.Output)
	}
}

func TestToolSearchNoMatch(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})

	ts := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	result := ts.Execute(context.Background(), map[string]any{
		"query": "nonexistent_xyz",
	})

	if result.IsError {
		t.Errorf("should not be error, got: %s", result.Output)
	}
	if !contains(result.Output, "No matching") {
		t.Errorf("expected 'No matching' message, got: %s", result.Output)
	}
}

// mockLargeDeferredTool 模拟具有大型 schema 的真实 MCP 工具；每个工具约有 500 多个
// JSON 字符，接近真实 Grafana 或 Playwright 工具。
type mockLargeDeferredTool struct {
	name string
	desc string
}

func (t *mockLargeDeferredTool) Name() string           { return t.name }
func (t *mockLargeDeferredTool) Description() string    { return t.desc }
func (t *mockLargeDeferredTool) Category() ToolCategory { return CategoryCommand }
func (t *mockLargeDeferredTool) ShouldDefer() bool      { return true }
func (t *mockLargeDeferredTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.name,
		"description": t.desc,
		"input_schema": map[string]any{
			"type":     "object",
			"required": []string{"query", "datasource"},
			"properties": map[string]any{
				"query":       map[string]any{"type": "string", "description": "The query expression to execute against the datasource"},
				"datasource":  map[string]any{"type": "string", "description": "Name or UID of the target datasource to query"},
				"start_time":  map[string]any{"type": "string", "description": "Start of the time range in RFC3339 or relative format"},
				"end_time":    map[string]any{"type": "string", "description": "End of the time range in RFC3339 or relative format"},
				"step":        map[string]any{"type": "string", "description": "Query resolution step width in duration format"},
				"max_results": map[string]any{"type": "integer", "description": "Maximum number of results to return from the query"},
				"format":      map[string]any{"type": "string", "description": "Output format: table, timeseries, or json"},
				"labels":      map[string]any{"type": "object", "description": "Additional label matchers to filter results"},
			},
		},
	}
}

func (t *mockLargeDeferredTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}

func TestDeferredTokenSavings(t *testing.T) {
	reg := NewRegistry()

	// 注册 2 个 schema 较小的普通（非延迟）工具。
	reg.Register(&mockNormalTool{name: "ReadFile"})
	reg.Register(&mockNormalTool{name: "WriteFile"})

	// 注册 50 个具有真实大型 schema 的延迟工具。
	for i := 0; i < 50; i++ {
		reg.Register(&mockLargeDeferredTool{
			name: fmt.Sprintf("mcp__grafana__tool_%03d", i),
			desc: fmt.Sprintf("A realistic MCP tool that queries datasource %d with full parameter set", i),
		})
	}

	// 测量隐藏延迟工具时的大小，这是默认状态。
	schemasDeferred := reg.GetAllSchemas("anthropic")
	bytesDeferred, err := json.Marshal(schemasDeferred)
	if err != nil {
		t.Fatalf("json.Marshal deferred schemas: %v", err)
	}
	sizeDeferred := len(bytesDeferred)

	// 发现全部 50 个延迟工具。
	for i := 0; i < 50; i++ {
		reg.MarkDiscovered(fmt.Sprintf("mcp__grafana__tool_%03d", i))
	}

	// 测量包含全部工具时的大小。
	schemasAll := reg.GetAllSchemas("anthropic")
	bytesAll, err := json.Marshal(schemasAll)
	if err != nil {
		t.Fatalf("json.Marshal all schemas: %v", err)
	}
	sizeAll := len(bytesAll)

	savings := 1 - float64(sizeDeferred)/float64(sizeAll)
	t.Logf("Deferred size: %d bytes, Full size: %d bytes, Savings: %.2f%%", sizeDeferred, sizeAll, savings*100)

	if savings < 0.90 {
		t.Errorf("expected >= 90%% token savings from deferral, got %.2f%%", savings*100)
	}
}

func TestDeferredEndToEndDiscovery(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockNormalTool{name: "Bash"})
	reg.Register(&mockDeferredTool{name: "mcp__playwright__click", desc: "Click an element"})
	reg.Register(&mockDeferredTool{name: "mcp__playwright__fill", desc: "Fill a form field"})

	// 第 1 步：延迟工具不应出现在 GetAllSchemas 中。
	schemas := reg.GetAllSchemas("anthropic")
	for _, s := range schemas {
		name := s["name"].(string)
		if name == "mcp__playwright__click" || name == "mcp__playwright__fill" {
			t.Errorf("deferred tool %q should not appear in GetAllSchemas before discovery", name)
		}
	}
	if len(schemas) != 1 {
		t.Errorf("expected 1 schema (Bash only), got %d", len(schemas))
	}

	// 第 2 步：GetDeferredToolNames 应返回两个延迟工具名称。
	deferredNames := reg.GetDeferredToolNames()
	nameSet := make(map[string]bool)
	for _, n := range deferredNames {
		nameSet[n] = true
	}
	if !nameSet["mcp__playwright__click"] || !nameSet["mcp__playwright__fill"] {
		t.Errorf("expected both deferred tools in GetDeferredToolNames, got %v", deferredNames)
	}

	// 第 3 步：发现一个工具。
	reg.MarkDiscovered("mcp__playwright__click")

	// 第 4 步：已发现工具现在应出现在 GetAllSchemas 中。
	schemas = reg.GetAllSchemas("anthropic")
	foundClick := false
	foundFill := false
	for _, s := range schemas {
		switch s["name"].(string) {
		case "mcp__playwright__click":
			foundClick = true
		case "mcp__playwright__fill":
			foundFill = true
		}
	}
	if !foundClick {
		t.Error("mcp__playwright__click should appear in GetAllSchemas after MarkDiscovered")
	}
	if foundFill {
		t.Error("mcp__playwright__fill should NOT appear in GetAllSchemas (still deferred)")
	}
	if len(schemas) != 2 {
		t.Errorf("expected 2 schemas (Bash + click), got %d", len(schemas))
	}

	// 第 5 步：GetDeferredToolNames 应只返回尚未发现的工具。
	deferredNames = reg.GetDeferredToolNames()
	if len(deferredNames) != 1 {
		t.Errorf("expected 1 deferred tool remaining, got %d: %v", len(deferredNames), deferredNames)
	}
	if len(deferredNames) == 1 && deferredNames[0] != "mcp__playwright__fill" {
		t.Errorf("expected mcp__playwright__fill as only deferred tool, got %q", deferredNames[0])
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
