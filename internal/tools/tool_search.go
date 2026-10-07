package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type ToolSearchTool struct {
	Registry *Registry
	Protocol string
}

func (t *ToolSearchTool) Name() string { return ToolSearchToolName }

func (t *ToolSearchTool) Description() string {
	return `Search for and load additional tools that are not immediately available. Some tools are deferred (not loaded by default) to save context space. Use this tool to discover and load them.

Query forms:
- "select:ToolName,AnotherTool" — fetch exact tools by name
- "keyword search" — keyword search, returns up to max_results matches

When you need a tool that isn't in your current tool list, use this to find it.`
}

func (t *ToolSearchTool) Category() ToolCategory { return CategoryRead }

func (t *ToolSearchTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": `Query to find deferred tools. Use "select:Name1,Name2" for direct selection, or keywords to search.`,
				},
				"max_results": map[string]any{
					"type":        "integer",
					"description": "Maximum results to return (default: 5)",
					"default":     5,
				},
			},
			"required": []string{"query"},
		},
	}
}

func (t *ToolSearchTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	query, _ := args["query"].(string)
	if query == "" {
		return ToolResult{Output: "Error: query is required", IsError: true}
	}

	maxResults := intArg(args, "max_results", 5)
	if maxResults < 1 {
		maxResults = 5
	}
	if maxResults > 20 {
		maxResults = 20
	}

	var schemas []map[string]any

	if strings.HasPrefix(query, "select:") {
		names := strings.Split(strings.TrimPrefix(query, "select:"), ",")
		for i := range names {
			names[i] = strings.TrimSpace(names[i])
		}
		schemas = t.Registry.FindDeferredByNames(names, t.Protocol)
	} else {
		schemas = t.Registry.SearchDeferred(query, maxResults, t.Protocol)
	}

	if len(schemas) == 0 {
		deferredNames := t.Registry.GetDeferredToolNames()
		if len(deferredNames) == 0 {
			return ToolResult{
				Output: fmt.Sprintf("No deferred tools available for query %q.", query),
			}
		}
		return ToolResult{
			Output: fmt.Sprintf("No matching deferred tools found for query %q. Available deferred tools: %s",
				query, strings.Join(deferredNames, ", ")),
		}
	}

	// 非 MCP 的延迟工具没有 mcp_call 这条入口，只能照旧标记成已发现、让它进
	// 下一轮的 tools[]
	var mcpNames []string
	for _, s := range schemas {
		name, ok := s["name"].(string)
		if !ok {
			continue
		}
		if strings.HasPrefix(name, MCPToolPrefix) {
			mcpNames = append(mcpNames, name)
		} else {
			t.Registry.MarkDiscovered(name)
		}
	}

	// 官方端点：回 tool_reference，让服务端把 schema 展开进上下文。tools 数组
	// 不动，缓存前缀因此不断。
	if len(mcpNames) > 0 && t.Registry.McpLoadingMode == McpLoadingNative && !isOpenAIProtocol(t.Protocol) {
		blocks := make([]map[string]any, 0, len(mcpNames))
		for _, name := range mcpNames {
			blocks = append(blocks, map[string]any{
				"type":      "tool_reference",
				"tool_name": name,
			})
		}
		return ToolResult{
			Output: fmt.Sprintf("Loaded %d tool(s): %s. You can call them directly now.",
				len(mcpNames), strings.Join(mcpNames, ", ")),
			ContentBlocks: blocks,
		}
	}

	// 其他端点：schema 原文给模型看，调用走 mcp_call。这段文本落在 messages
	// 末尾，属于追加，不影响缓存前缀。
	suffix := ""
	if len(mcpNames) > 0 {
		suffix = "\n\nTo invoke any of the tools above, call mcp_call with that tool's " +
			"full name and an `arguments` object matching its input_schema exactly, " +
			"using the same JSON types."
	}
	schemasJSON, _ := json.MarshalIndent(schemas, "", "  ")
	return ToolResult{
		Output: fmt.Sprintf("Found %d tool(s). Their full schemas are below:\n\n%s%s",
			len(schemas), string(schemasJSON), suffix),
	}
}
