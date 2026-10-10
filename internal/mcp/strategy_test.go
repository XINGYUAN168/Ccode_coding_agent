package mcp

import (
	"context"
	"testing"

	"ccode/internal/tools"
)

type stubMCPTool struct {
	name    string
	noDefer bool
}

func (s *stubMCPTool) Name() string                   { return s.name }
func (s *stubMCPTool) Description() string            { return "stub" }
func (s *stubMCPTool) Category() tools.ToolCategory   { return tools.CategoryCommand }
func (s *stubMCPTool) ShouldDefer() bool              { return !s.noDefer }
func (s *stubMCPTool) SetDeferLoading(on bool)        { s.noDefer = !on }
func (s *stubMCPTool) MCPServerName() string          { return "linear" }
func (s *stubMCPTool) MCPInputSchema() map[string]any { return map[string]any{"type": "object"} }

func (s *stubMCPTool) Schema() map[string]any {
	return map[string]any{
		"name":         s.name,
		"description":  "stub",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (s *stubMCPTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	return tools.ToolResult{Output: "ok"}
}

func TestIsOfficialAnthropicEndpoint(t *testing.T) {
	// 空 base_url 表示走 SDK 默认地址，也就是官方
	if !IsOfficialAnthropicEndpoint("anthropic", "") {
		t.Error("空 base_url 应判为官方")
	}
	if !IsOfficialAnthropicEndpoint("anthropic", "https://api.anthropic.com") {
		t.Error("官方域名应判为官方")
	}
	if IsOfficialAnthropicEndpoint("anthropic", "https://api.minimaxi.com/anthropic") {
		t.Error("第三方端点不该判为官方")
	}
	if IsOfficialAnthropicEndpoint("openai", "") {
		t.Error("OpenAI 协议的空 base_url 不该判为 Anthropic 官方端点")
	}
}

func TestDecideMode(t *testing.T) {
	const window = 200000 // 一成是两万 token，约五万字符
	cases := []struct {
		desc     string
		protocol string
		baseURL  string
		chars    int
		want     tools.McpLoadingMode
	}{
		{"schema 很小就全量上", "openai", "https://proxy.example.com", 1000, tools.McpLoadingEager},
		{"没有 MCP 工具也全量上", "openai-compat", "https://proxy.example.com", 0, tools.McpLoadingEager},
		{"Anthropic 官方端点走原生延迟", "anthropic", "", 500000, tools.McpLoadingNative},
		{"Anthropic 第三方端点走 mcp_call", "anthropic", "https://api.minimaxi.com/anthropic", 500000, tools.McpLoadingDispatch},
		{"OpenAI 官方端点也不能走 Anthropic 原生延迟", "openai", "", 500000, tools.McpLoadingDispatch},
		{"OpenAI 兼容端点不能走 Anthropic 原生延迟", "openai-compat", "", 500000, tools.McpLoadingDispatch},
	}
	for _, c := range cases {
		if got := DecideMode(c.protocol, c.baseURL, window, c.chars, DefaultEagerThresholdPercent); got != c.want {
			t.Errorf("%s：得到 %s，期望 %s", c.desc, got, c.want)
		}
	}
}

func TestDecideModeEnvOverride(t *testing.T) {
	t.Setenv(envLoadingOverride, "dispatch")
	// 小配置本该 eager，被环境变量强制成 dispatch
	if got := DecideMode("anthropic", "", 200000, 10, DefaultEagerThresholdPercent); got != tools.McpLoadingDispatch {
		t.Errorf("环境变量该覆盖判定，得到 %s", got)
	}
	t.Setenv(envLoadingOverride, "native")
	if got := DecideMode("openai", "", 200000, 500000, DefaultEagerThresholdPercent); got != tools.McpLoadingDispatch {
		t.Errorf("OpenAI 不能被强制为 Anthropic 原生模式，得到 %s", got)
	}
}

func TestMeasureSchemaCharsCountsOnlyMCPTools(t *testing.T) {
	reg := tools.CreateDefaultRegistry()
	if got := MeasureSchemaChars(reg); got != 0 {
		t.Errorf("只有内建工具时该是 0，得到 %d", got)
	}
	reg.Register(&stubMCPTool{name: "mcp__linear__create_issue"})
	if got := MeasureSchemaChars(reg); got <= 0 {
		t.Errorf("有 MCP 工具时该大于 0，得到 %d", got)
	}
}

func TestApplyMode(t *testing.T) {
	mcpCount := func(reg *tools.Registry) (total, deferred int) {
		for _, s := range reg.GetAllSchemas("anthropic") {
			name, _ := s["name"].(string)
			if len(name) > 5 && name[:5] == "mcp__" {
				total++
				if s["defer_loading"] == true {
					deferred++
				}
			}
		}
		return
	}

	// eager：进 tools[]，不带 defer_loading
	reg := tools.CreateDefaultRegistry()
	reg.Register(&stubMCPTool{name: "mcp__linear__create_issue"})
	ApplyMode(reg, tools.McpLoadingEager)
	if total, deferred := mcpCount(reg); total != 1 || deferred != 0 {
		t.Errorf("eager：期望进数组且不带标记，得到 total=%d deferred=%d", total, deferred)
	}

	// native：进 tools[] 且带 defer_loading，交给服务端决定可见性
	reg = tools.CreateDefaultRegistry()
	reg.Register(&stubMCPTool{name: "mcp__linear__create_issue"})
	ApplyMode(reg, tools.McpLoadingNative)
	if total, deferred := mcpCount(reg); total != 1 || deferred != 1 {
		t.Errorf("native：期望进数组且带标记，得到 total=%d deferred=%d", total, deferred)
	}

	// dispatch：完全不进 tools[]，靠 mcp_call 兜
	reg = tools.CreateDefaultRegistry()
	reg.Register(&stubMCPTool{name: "mcp__linear__create_issue"})
	ApplyMode(reg, tools.McpLoadingDispatch)
	if total, _ := mcpCount(reg); total != 0 {
		t.Errorf("dispatch：MCP 工具不该出现在数组里，得到 %d 个", total)
	}
}

func TestNativeFlagNotSentOnOpenAIProtocol(t *testing.T) {
	// defer_loading 是 Anthropic 的字段，openai 协议下不能带出去
	reg := tools.CreateDefaultRegistry()
	reg.Register(&stubMCPTool{name: "mcp__linear__create_issue"})
	ApplyMode(reg, tools.McpLoadingNative)
	for _, protocol := range []string{"openai", "openai-compat"} {
		for _, s := range reg.GetAllSchemas(protocol) {
			if name, _ := s["name"].(string); len(name) > 5 && name[:5] == "mcp__" {
				t.Errorf("%s 协议下不该带出 MCP 工具：%s", protocol, name)
			}
		}
	}
}

func TestMCPToolNamePrefixSanitizes(t *testing.T) {
	// 服务器名里的横杠要被换成下划线，否则按前缀筛工具会漏
	if got := MCPToolNamePrefix("chrome-devtools"); got != "mcp__chrome_devtools__" {
		t.Errorf("得到 %q", got)
	}
}
