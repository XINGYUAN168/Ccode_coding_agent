package tools

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// mockDispatchTool 是够用的 MCP 工具替身：暴露 schema、记录收到的参数。
type mockDispatchTool struct {
	name     string
	server   string
	schema   map[string]any
	received map[string]any
	noDefer  bool
}

func (m *mockDispatchTool) Name() string            { return m.name }
func (m *mockDispatchTool) Description() string     { return "mock" }
func (m *mockDispatchTool) Category() ToolCategory  { return CategoryCommand }
func (m *mockDispatchTool) ShouldDefer() bool       { return !m.noDefer }
func (m *mockDispatchTool) SetDeferLoading(on bool) { m.noDefer = !on }
func (m *mockDispatchTool) MCPServerName() string   { return m.server }
func (m *mockDispatchTool) MCPInputSchema() map[string]any {
	if m.schema == nil {
		return map[string]any{}
	}
	return m.schema
}

func (m *mockDispatchTool) Schema() map[string]any {
	return map[string]any{
		"name":         m.name,
		"description":  "mock",
		"input_schema": m.MCPInputSchema(),
	}
}

func (m *mockDispatchTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	m.received = args
	return ToolResult{Output: "ok"}
}

var testSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"issueId": map[string]any{"type": "string"},
		"limit":   map[string]any{"type": "integer"},
		"ratio":   map[string]any{"type": "number"},
		"flag":    map[string]any{"type": "boolean"},
		"labels":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"ports":   map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
		"config": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"replicas": map[string]any{"type": "integer"},
				"features": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		},
	},
}

// 强转契约：这七条四个语言必须逐条一致。
func TestCoerceBySchemaContract(t *testing.T) {
	cases := []struct {
		desc  string
		given map[string]any
		want  map[string]any
	}{
		{"string ← 整数", map[string]any{"issueId": float64(8891)}, map[string]any{"issueId": "8891"}},
		{"string ← 小数", map[string]any{"issueId": 1.5}, map[string]any{"issueId": "1.5"}},
		{"integer ← 数字串", map[string]any{"limit": "5"}, map[string]any{"limit": int64(5)}},
		{"number ← 数字串带空白", map[string]any{"ratio": " 1.5 "}, map[string]any{"ratio": 1.5}},
		{"boolean ← true", map[string]any{"flag": "true"}, map[string]any{"flag": true}},
		{"boolean ← 大写 FALSE", map[string]any{"flag": "FALSE"}, map[string]any{"flag": false}},
		{
			"array ← 单键对象拆包",
			map[string]any{"labels": map[string]any{"item": []any{"a", "b"}}},
			map[string]any{"labels": []any{"a", "b"}},
		},
		{
			"array ← 逗号串",
			map[string]any{"labels": "a, b"},
			map[string]any{"labels": []any{"a", "b"}},
		},
		{
			"array 按 items 递归",
			map[string]any{"ports": []any{"8080", "9090"}},
			map[string]any{"ports": []any{int64(8080), int64(9090)}},
		},
		{
			"object 按 properties 递归，嵌套层同样适用",
			map[string]any{"config": map[string]any{
				"replicas": "4",
				"features": map[string]any{"item": []any{"x"}},
			}},
			map[string]any{"config": map[string]any{
				"replicas": int64(4),
				"features": []any{"x"},
			}},
		},
	}
	for _, c := range cases {
		got := CoerceBySchema(c.given, testSchema)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: 得到 %#v，期望 %#v", c.desc, got, c.want)
		}
	}
}

func TestCoerceBySchemaLeavesThingsAlone(t *testing.T) {
	// bool 不能被当成数字转成字符串
	if got := CoerceBySchema(map[string]any{"issueId": true}, testSchema); got.(map[string]any)["issueId"] != true {
		t.Errorf("bool 不该被转成字符串，得到 %#v", got)
	}
	// 转不了的原样往下传，交给 MCP 服务器报它自己的错
	if got := CoerceBySchema(map[string]any{"limit": "many"}, testSchema); got.(map[string]any)["limit"] != "many" {
		t.Error("转不了的应原样保留")
	}
	// schema 里没有的键不动
	if got := CoerceBySchema(map[string]any{"extra": 1}, testSchema); got.(map[string]any)["extra"] != 1 {
		t.Error("未知键应原样保留")
	}
	// 空 schema 是 no-op
	if got := CoerceBySchema(map[string]any{"a": "1"}, map[string]any{}); got.(map[string]any)["a"] != "1" {
		t.Error("空 schema 不该改动参数")
	}
}

// 各语言的字符串转数字各有各的宽松处：Go 的 ParseFloat 收 inf 和科学计数法，
// Python 的 int() 收下划线。这些形状四个语言必须给出同一个结果。
func TestCoerceNumericShapeParity(t *testing.T) {
	cases := []struct {
		key  string
		in   string
		want any
	}{
		{"limit", "5", int64(5)},
		{"limit", "+5", int64(5)},
		{"limit", "5.7", "5.7"}, // integer 不做截断
		{"limit", "1_000", "1_000"},
		{"limit", "1e3", "1e3"},
		{"limit", "5abc", "5abc"},
		{"ratio", " 1.5 ", 1.5},
		{"ratio", "1e3", 1000.0}, // 科学计数法是合法 JSON 数字，收
		{"ratio", "inf", "inf"},
		{"ratio", "nan", "nan"},
	}
	for _, c := range cases {
		got := CoerceBySchema(map[string]any{c.key: c.in}, testSchema).(map[string]any)[c.key]
		if got != c.want {
			t.Errorf("%s=%q 转成 %#v，期望 %#v", c.key, c.in, got, c.want)
		}
	}
}

// 拆包只认单键对象，多键的猜不出意图，原样传下去让服务器报错
func TestCoerceMultiKeyObjectForArrayLeftAlone(t *testing.T) {
	inner := map[string]any{"item": "metrics", "tracing": ""}
	got := CoerceBySchema(map[string]any{"labels": inner}, testSchema).(map[string]any)["labels"]
	m, ok := got.(map[string]any)
	if !ok || len(m) != 2 || m["item"] != "metrics" {
		t.Errorf("多键对象应原样保留，得到 %#v", got)
	}
}

func newDispatchRegistry() (*Registry, *McpCallTool, *mockDispatchTool) {
	reg := NewRegistry()
	reg.McpLoadingMode = McpLoadingDispatch
	tool := &mockDispatchTool{name: "mcp__linear__create_issue", server: "linear", schema: testSchema}
	reg.Register(tool)
	d := &McpCallTool{Registry: reg}
	reg.Register(d)
	return reg, d, tool
}

func TestMcpCallResolvesFullName(t *testing.T) {
	_, d, tool := newDispatchRegistry()
	res := d.Execute(context.Background(), map[string]any{
		"server": "linear", "tool": "mcp__linear__create_issue",
		"arguments": map[string]any{"issueId": "A"},
	})
	if res.IsError {
		t.Fatalf("不该报错：%s", res.Output)
	}
	if tool.received["issueId"] != "A" {
		t.Errorf("参数没落地：%#v", tool.received)
	}
}

// 模型很常只传短名（实测约三成调用），这里必须容错，否则白白多一轮重试。
func TestMcpCallResolvesShortName(t *testing.T) {
	_, d, tool := newDispatchRegistry()
	res := d.Execute(context.Background(), map[string]any{
		"server": "linear", "tool": "create_issue",
		"arguments": map[string]any{"issueId": "A"},
	})
	if res.IsError {
		t.Fatalf("短名该被解析出来：%s", res.Output)
	}
	if tool.received["issueId"] != "A" {
		t.Errorf("参数没落地：%#v", tool.received)
	}
}

func TestMcpCallResolvesBySuffixWhenServerWrong(t *testing.T) {
	_, d, tool := newDispatchRegistry()
	res := d.Execute(context.Background(), map[string]any{
		"server": "typo", "tool": "create_issue",
		"arguments": map[string]any{"issueId": "A"},
	})
	if res.IsError || tool.received == nil {
		t.Errorf("服务器名写错时应按后缀唯一匹配兜底：%s", res.Output)
	}
}

func TestMcpCallAmbiguousSuffixErrors(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockDispatchTool{name: "mcp__linear__create_issue", server: "linear"})
	reg.Register(&mockDispatchTool{name: "mcp__jira__create_issue", server: "jira"})
	d := &McpCallTool{Registry: reg}
	res := d.Execute(context.Background(), map[string]any{
		"server": "nope", "tool": "create_issue", "arguments": map[string]any{},
	})
	if !res.IsError {
		t.Fatal("两个工具同名后缀时应报错而不是猜")
	}
	// 错误信息里要列出可用工具，模型才知道怎么改
	if !strings.Contains(res.Output, "mcp__linear__create_issue") {
		t.Errorf("错误信息应列出可用工具：%s", res.Output)
	}
}

func TestMcpCallCoercesBeforeDispatch(t *testing.T) {
	_, d, tool := newDispatchRegistry()
	d.Execute(context.Background(), map[string]any{
		"server": "linear", "tool": "create_issue",
		"arguments": map[string]any{"issueId": float64(8891), "ports": []any{"1"}},
	})
	if tool.received["issueId"] != "8891" {
		t.Errorf("issueId 该被转成字符串：%#v", tool.received["issueId"])
	}
	ports, _ := tool.received["ports"].([]any)
	if len(ports) != 1 || ports[0] != int64(1) {
		t.Errorf("ports 元素该被转成整数：%#v", tool.received["ports"])
	}
}

func TestMcpCallPermissionContentNormalization(t *testing.T) {
	cases := []struct{ server, tool, want string }{
		{"linear", "mcp__linear__create_issue", "linear__create_issue"},
		{"linear", "create_issue", "linear__create_issue"},
		{"chrome-2", "mcp__chrome_2__click", "chrome_2__click"},
		// 短名和全名必须算出同一个 content，否则规则会漏匹配
		{"chrome-devtools", "click", "chrome_devtools__click"},
		{"chrome-devtools", "mcp__chrome_devtools__click", "chrome_devtools__click"},
	}
	for _, c := range cases {
		if got := McpCallPermissionContent(c.server, c.tool); got != c.want {
			t.Errorf("(%s,%s) 得到 %q，期望 %q", c.server, c.tool, got, c.want)
		}
	}
}

// 检索和分发只在用得上的模式里发给模型。eager 下 MCP 工具全在 tools[] 里，
// 既没有可搜的对象也不需要分发入口，两个都发过去只是白占 token。
func TestToolExposureByMode(t *testing.T) {
	build := func(mode McpLoadingMode) []string {
		reg := NewRegistry()
		reg.Register(&ToolSearchTool{Registry: reg})
		reg.Register(&McpCallTool{Registry: reg})
		mcpTool := &mockDispatchTool{name: "mcp__linear__create_issue", server: "linear", schema: testSchema}
		reg.Register(mcpTool)

		// 模拟 mcp.ApplyMode 的效果，避免 tools 包反向依赖 mcp 包
		reg.McpLoadingMode = mode
		eager := mode == McpLoadingEager
		mcpTool.SetDeferLoading(!eager)
		reg.ExposeToolSearch = !eager
		reg.ExposeMcpCall = mode == McpLoadingDispatch

		var names []string
		for _, s := range reg.GetAllSchemas("anthropic") {
			names = append(names, s["name"].(string))
		}
		sort.Strings(names)
		return names
	}

	cases := []struct {
		mode McpLoadingMode
		want []string
	}{
		{McpLoadingEager, []string{"mcp__linear__create_issue"}},
		{McpLoadingNative, []string{"ToolSearch", "mcp__linear__create_issue"}},
		{McpLoadingDispatch, []string{"ToolSearch", "mcp_call"}},
	}
	for _, c := range cases {
		got := build(c.mode)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s 模式下 tools[] = %v，期望 %v", c.mode, got, c.want)
		}
	}
}

// 没连 MCP 时 ApplyMode 不会被调用，两个开关保持默认关闭。
func TestToolExposureDefaultsOff(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&ToolSearchTool{Registry: reg})
	reg.Register(&McpCallTool{Registry: reg})
	if n := len(reg.GetAllSchemas("anthropic")); n != 0 {
		t.Errorf("没连 MCP 时不该发这两个工具，实得 %d 个", n)
	}
}
