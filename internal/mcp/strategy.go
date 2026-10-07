package mcp

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"

	"ccode/internal/tools"
)

// 决定 MCP 工具怎么进上下文。三条路，会话启动连上 MCP 之后定一次：
//
//	eager    —— MCP schema 总量小于上下文的一成，全量放进 tools[]，不延迟。
//	            省下来的那点上下文不值得为它承担任何额外风险。
//	native   —— 官方 Anthropic 端点。工具带 defer_loading 留在 tools[] 里但服务端
//	            不给模型看，ToolSearch 回 tool_reference 让服务端展开 schema。
//	native 之外的端点不支持这两样，只能自己模拟：MCP 工具完全不进 tools[]，
//	走 mcp_call 统一入口。
//
// 为什么要分这三条：tools 渲染在 system 之后、messages 之前，数组一变，它后面
// 的整段对话历史缓存全部失效。实测 2 万 token 历史下，往 tools 末尾加一个工具
// 的命中率从 99.4% 掉到 9.5%，等于把整段历史重算一遍。
const (
	// DefaultEagerThresholdPercent 低于上下文窗口这个比例就不延迟
	DefaultEagerThresholdPercent = 10

	// CharsPerToken 是拿不到真实 token 数时的估算比例。MCP 的 schema 是 JSON，
	// 符号密度高，每 token 的字符数比自然语言低。
	CharsPerToken = 2.5

	// NativeToolSearchBeta 是官方端点用的 beta header，defer_loading 和
	// tool_reference 都靠它开。
	NativeToolSearchBeta = "advanced-tool-use-2025-11-20"

	envLoadingOverride = "CCODE_MCP_LOADING"
)

var officialHosts = map[string]bool{"api.anthropic.com": true}

// IsOfficialAnthropicEndpoint 判断是不是官方端点。baseURL 为空表示走 SDK 默认
// 地址，也就是官方。
func IsOfficialAnthropicEndpoint(baseURL string) bool {
	if baseURL == "" {
		return true
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return officialHosts[strings.ToLower(u.Hostname())]
}

// EstimateSchemaTokens 按字符数估算 token。
func EstimateSchemaTokens(schemaChars int) int {
	return int(float64(schemaChars) / CharsPerToken)
}

// DecideMode 定加载模式。
func DecideMode(baseURL string, contextWindow, mcpSchemaChars, thresholdPercent int) tools.McpLoadingMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envLoadingOverride))) {
	case "eager":
		return tools.McpLoadingEager
	case "native":
		return tools.McpLoadingNative
	case "dispatch":
		return tools.McpLoadingDispatch
	}

	// 没有 MCP 工具，走哪条都一样，eager 最省事
	if mcpSchemaChars <= 0 {
		return tools.McpLoadingEager
	}

	budget := float64(contextWindow) * float64(thresholdPercent) / 100
	if float64(EstimateSchemaTokens(mcpSchemaChars)) < budget {
		return tools.McpLoadingEager
	}
	if IsOfficialAnthropicEndpoint(baseURL) {
		return tools.McpLoadingNative
	}
	return tools.McpLoadingDispatch
}

// MeasureSchemaChars 统计 MCP 工具 schema 序列化后的字符数，用来跟阈值比。
func MeasureSchemaChars(registry *tools.Registry) int {
	total := 0
	for _, t := range registry.ListTools() {
		if !strings.HasPrefix(t.Name(), tools.MCPToolPrefix) {
			continue
		}
		if b, err := json.Marshal(t.Schema()); err == nil {
			total += len(b)
		} else {
			total += len(t.Name()) + len(t.Description())
		}
	}
	return total
}

// ApplyMode 把决定落到 registry 上。
//
// eager 下要把 MCP 工具的延迟标记摘掉，它们才会出现在 tools[] 里；另外两条路
// 保持延迟。mcp_call 不在这里注册——它必须在 MCP 连接之前就在 tools[] 里，
// 否则连上之后再加就是一次中途改动 tools 数组，缓存照样断。
func ApplyMode(registry *tools.Registry, mode tools.McpLoadingMode) {
	registry.McpLoadingMode = mode
	eager := mode == tools.McpLoadingEager
	for _, t := range registry.ListTools() {
		if mt, ok := t.(tools.MCPTool); ok {
			mt.SetDeferLoading(!eager)
		}
	}

	// 检索和分发按模式决定发不发。eager 下所有工具都在 tools[] 里，没有可搜的
	// 对象、也不需要分发入口。这两个开关在这里算一次就固定下来，整场会话不变，
	// 不会造成 tools[] 中途抖动。
	registry.ExposeToolSearch = !eager
	registry.ExposeMcpCall = mode == tools.McpLoadingDispatch
}

// DecideAndApply 是连上 MCP 之后调一次的入口。
func DecideAndApply(registry *tools.Registry, baseURL string, contextWindow int) tools.McpLoadingMode {
	mode := DecideMode(baseURL, contextWindow, MeasureSchemaChars(registry), DefaultEagerThresholdPercent)
	ApplyMode(registry, mode)
	return mode
}
