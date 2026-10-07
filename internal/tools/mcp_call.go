package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 数字形状：整串都得是合法的 JSON 数字，integer 还不许有小数和指数部分。
// 这两条正则四个语言必须一致——不挡的话各语言的字符串转数字各有各的宽松处，
// Go 的 ParseFloat 收 "inf"、Python 的 int() 收 "1_000"，同一份参数在不同
// 语言下会转出不一样的值。
var (
	intShape = regexp.MustCompile(`^[+-]?\d+$`)
	numShape = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
)

// MCPToolPrefix 是所有 MCP 工具名的公共前缀。
const MCPToolPrefix = "mcp__"

// MCPNameSep 是 MCP 工具名里服务器段和工具段的分隔符。用双下划线是为了让边界
// 可逆——服务器名和工具名自身允许带单下划线。
const MCPNameSep = "__"

// McpCallToolName 是分发工具的名字，权限规则里也用它。
const McpCallToolName = "mcp_call"

// coerceScalar 按 schema 声明的类型做保守强转，转不了就原样返回。
func coerceScalar(value any, want string) any {
	switch want {
	case "string":
		// bool 不能被当成数字转成 "true"
		switch v := value.(type) {
		case float64:
			if v == float64(int64(v)) {
				return strconv.FormatInt(int64(v), 10)
			}
			return strconv.FormatFloat(v, 'f', -1, 64)
		case int:
			return strconv.Itoa(v)
		case int64:
			return strconv.FormatInt(v, 10)
		case json.Number:
			return v.String()
		}
	case "integer":
		if s, ok := value.(string); ok {
			text := strings.TrimSpace(s)
			// "5.7" 配 integer 不做截断，原样交给 MCP 服务器报它的域内错误
			if !intShape.MatchString(text) {
				return value
			}
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				return n
			}
		}
	case "number":
		if s, ok := value.(string); ok {
			text := strings.TrimSpace(s)
			if !numShape.MatchString(text) {
				return value
			}
			if f, err := strconv.ParseFloat(text, 64); err == nil {
				return f
			}
		}
	case "boolean":
		if s, ok := value.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true":
				return true
			case "false":
				return false
			}
		}
	}
	return value
}

// CoerceBySchema 按 JSON schema 递归修正模型给的参数。
//
// MCP 工具不在 tools[] 里的时候，参数是模型自由生成的，没有接口层的 schema 约束，偶尔
// 会写错 JSON 类型。这里的修正规则四个语言必须逐条一致：
//
//	schema 声明        模型给的               修正为
//	string            数字                   "8891"
//	integer / number  数字形字符串            5 / 5.0
//	boolean           "true" / "false"       true / false
//	array             单键对象且值是数组       拆出内层数组
//	array             逗号分隔字符串          按逗号切分去空白
//	object            对象                    按 properties 递归
//	array             数组                    按 items 递归每个元素
//
// 修正不了的原样往下传，交给 MCP 服务器报它自己的错——服务器的域内错误比本地
// 类型错误对模型更有指导性。
func CoerceBySchema(value any, schema any) any {
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		return value
	}
	want, _ := schemaMap["type"].(string)

	if want == "object" {
		obj, ok := value.(map[string]any)
		if !ok {
			return value
		}
		props, _ := schemaMap["properties"].(map[string]any)
		out := make(map[string]any, len(obj))
		for k, v := range obj {
			if sub, found := props[k]; found {
				out[k] = CoerceBySchema(v, sub)
			} else {
				out[k] = v
			}
		}
		return out
	}

	if want == "array" {
		itemSchema := schemaMap["items"]
		// 模型常把数组包成 {"item": [...]} 这类单键对象
		if obj, isObj := value.(map[string]any); isObj && len(obj) == 1 {
			for _, inner := range obj {
				if arr, isArr := inner.([]any); isArr {
					value = arr
				}
			}
		} else if s, isStr := value.(string); isStr {
			// 也常拼成逗号分隔的字符串
			parts := strings.Split(s, ",")
			arr := make([]any, 0, len(parts))
			for _, p := range parts {
				if t := strings.TrimSpace(p); t != "" {
					arr = append(arr, t)
				}
			}
			value = arr
		}
		if arr, isArr := value.([]any); isArr {
			out := make([]any, len(arr))
			for i, item := range arr {
				out[i] = CoerceBySchema(item, itemSchema)
			}
			return out
		}
		return value
	}

	if want != "" {
		return coerceScalar(value, want)
	}
	return value
}

// McpCallPermissionContent 是权限规则匹配用的 content，归一化成 server__tool。
//
// 不带 mcp__ 前缀，也不受各语言 wrapper 命名差异影响，四个语言的
// permissions.yaml 写法因此完全一致：mcp_call(linear__create_issue)。
//
// 两段都要过一遍 sanitize。模型可能传短名也可能传全名，全名里的段是 wrapper
// 已经处理过的，短名是模型原样给的——不统一处理的话，同一个调用传短名和传全名
// 会算出不同的 content，规则就会漏匹配。
func McpCallPermissionContent(server, tool string) string {
	if strings.HasPrefix(tool, MCPToolPrefix) {
		rest := strings.TrimPrefix(tool, MCPToolPrefix)
		// 全名里已经带了服务器段，用它，避免拼出 linear__linear__x
		if idx := strings.Index(rest, MCPNameSep); idx >= 0 {
			return sanitizeSegment(rest[:idx]) + MCPNameSep +
				sanitizeSegment(rest[idx+len(MCPNameSep):])
		}
	}
	return sanitizeSegment(server) + MCPNameSep + sanitizeSegment(tool)
}

// McpCallTool 是 MCP 工具的统一调用入口。
//
// MCP 工具不进入 tools[]，模型先用 ToolSearch 读到 schema，再通过它把工具名和
// 参数传进来。这样 tools 数组在整场会话里字节不变，prompt cache 的前缀不会被
// 打断——工具排在 system 之后、messages 之前，数组一变，它后面的整段历史都要
// 重算。
type McpCallTool struct {
	Registry *Registry
}

func (t *McpCallTool) Name() string { return McpCallToolName }

func (t *McpCallTool) Description() string {
	return "Invoke a tool on a connected MCP server. Call ToolSearch first to load " +
		"the tool's schema, then pass its arguments here exactly as that schema " +
		"requires, using the same JSON types."
}

func (t *McpCallTool) Category() ToolCategory { return CategoryCommand }

func (t *McpCallTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server": map[string]any{
					"type":        "string",
					"description": "MCP server name, e.g. 'linear'.",
				},
				"tool": map[string]any{
					"type": "string",
					"description": "Full tool name as returned by ToolSearch, " +
						"e.g. 'mcp__linear__create_issue'.",
				},
				"arguments": map[string]any{
					"type": "object",
					"description": "The target tool's arguments. Must match that " +
						"tool's input_schema exactly, including JSON types: bare " +
						"numbers for integer fields, bare true/false for boolean " +
						"fields, quoted strings for string fields, and plain JSON " +
						"arrays for array fields.",
				},
			},
			"required": []string{"server", "tool", "arguments"},
		},
	}
}

// resolve 依次尝试全名、server+短名、短名后缀唯一匹配。
//
// 模型很常只传短名（实测约三成调用），这里必须容错，否则会白白换来一轮重试。
func (t *McpCallTool) resolve(server, tool string) Tool {
	if found := t.Registry.Get(tool); found != nil {
		return found
	}
	if found := t.Registry.Get(MCPToolPrefix + sanitizeSegment(server) + MCPNameSep + sanitizeSegment(tool)); found != nil {
		return found
	}
	suffix := MCPNameSep + sanitizeSegment(tool)
	var matches []Tool
	for _, candidate := range t.Registry.ListTools() {
		name := candidate.Name()
		if strings.HasPrefix(name, MCPToolPrefix) && strings.HasSuffix(name, suffix) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return nil
}

// sanitizeSegment 与 mcp.SanitizeName 保持一致的替换规则。这里不引用
// internal/mcp 以避免循环依赖。
func sanitizeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func (t *McpCallTool) availableNames() []string {
	var names []string
	for _, candidate := range t.Registry.ListTools() {
		if strings.HasPrefix(candidate.Name(), MCPToolPrefix) {
			names = append(names, candidate.Name())
		}
	}
	sort.Strings(names)
	return names
}

func (t *McpCallTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	server, _ := args["server"].(string)
	tool, _ := args["tool"].(string)
	if tool == "" {
		return ToolResult{Output: "mcp_call requires a 'tool' name", IsError: true}
	}

	target := t.resolve(server, tool)
	if target == nil {
		names := t.availableNames()
		hint := "(none connected)"
		if len(names) > 0 {
			hint = strings.Join(names, ", ")
		}
		return ToolResult{
			Output: fmt.Sprintf(
				"Unknown MCP tool '%s' on server '%s'. Available tools: %s",
				tool, server, hint),
			IsError: true,
		}
	}

	inner, _ := args["arguments"].(map[string]any)
	if inner == nil {
		inner = map[string]any{}
	}
	if mt, ok := target.(MCPTool); ok {
		if schema := mt.MCPInputSchema(); len(schema) > 0 {
			if fixed, ok := CoerceBySchema(inner, schema).(map[string]any); ok {
				inner = fixed
			}
		}
	}
	return target.Execute(ctx, inner)
}
