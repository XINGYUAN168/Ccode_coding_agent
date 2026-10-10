package tools

import (
	"context"
	"sort"
	"strings"
)

var SkipDirs = map[string]bool{
	".git": true, ".venv": true, "node_modules": true,
	"__pycache__": true, ".tox": true, ".mypy_cache": true,
}

// MaxOutputChars 是单条工具结果进入对话历史前的溢写阈值：超过这个字符数
// 就把完整内容写盘，历史里只留预览和文件路径。定在 50000 而不是更小的值，
// 是为了让模型一次能看到足够多的内容，不必为了看全结果再发一轮 ReadFile。
const MaxOutputChars = 50000

type ToolResult struct {
	Output  string
	IsError bool
	// ContentBlocks 用于把工具结果发成结构化 content block 而不是纯文本。
	// 目前只有官方 Anthropic 端点下的 ToolSearch 会用：它回 tool_reference 块，
	// 由服务端把 schema 展开进上下文。填了这个字段时 Output 仍保留等价文本，
	// 供 TUI 和日志展示。
	ContentBlocks []map[string]any
}

// McpLoadingMode 决定 MCP 工具怎么进上下文，由 internal/mcp 在连上服务器后写入
// Registry。放在这里而不是 internal/mcp，是因为 Registry 要持有它，而
// internal/mcp 依赖 internal/tools，反向引用会成环。
type McpLoadingMode string

const (
	// McpLoadingEager：schema 总量小于上下文的一成，全量放进 tools[]，不延迟。
	McpLoadingEager McpLoadingMode = "eager"
	// McpLoadingNative：官方端点。工具带 defer_loading 留在 tools[] 里但服务端
	// 不给模型看，ToolSearch 回 tool_reference 让服务端展开 schema。
	McpLoadingNative McpLoadingMode = "native"
	// McpLoadingDispatch：其他端点不支持 defer_loading，MCP 工具完全不进
	// tools[]，走 mcp_call 统一入口。
	McpLoadingDispatch McpLoadingMode = "dispatch"
)

// MCPTool 是 MCP 工具包装器额外暴露给分发和分流逻辑的能力。用结构化接口而不是
// 直接引用 internal/mcp，同样是为了避开循环依赖。
type MCPTool interface {
	Tool
	MCPServerName() string
	MCPInputSchema() map[string]any
	SetDeferLoading(bool)
}

// ToolSearchToolName 是工具检索的名字，注册表按模式筛它时要用。
const ToolSearchToolName = "ToolSearch"

type ToolCategory string

const (
	CategoryRead    ToolCategory = "read"
	CategoryWrite   ToolCategory = "write"
	CategoryCommand ToolCategory = "command"
)

type Tool interface {
	Name() string
	Description() string
	Category() ToolCategory
	Schema() map[string]any
	Execute(ctx context.Context, args map[string]any) ToolResult
}

// DeferrableTool 让工具声明自己要不要延迟加载。延迟的工具不出现在初始 tool list 里，
// 模型得先用 ToolSearch 把 schema 捞出来才能调。
//
// 只有 MCP 工具实现它。MCP 是按项目配的，一个服务器动辄几十个工具，schema 又长，
// 全塞进初始 tool list 会把上下文占掉一大块，而且大部分工具这次会话根本用不上。
// 内建工具是固定的那几十个，数量可控，藏起来只会让模型多绕一次 ToolSearch，
// 所以一律不延迟，直接给全量 schema。
type DeferrableTool interface {
	ShouldDefer() bool
}

// ConcurrencySafeTool 让工具按这一次调用的实际参数决定能不能跟别的调用并发跑。
//
// 不实现它的工具按类别走：只读的可以并发，写和命令类不行。实现它的目前只有 Bash：
// 一条命令是不是只读要看命令本身，ls 和 rm 都是 Bash，并发安全性完全不同。
type ConcurrencySafeTool interface {
	IsConcurrencySafe(args map[string]any) bool
}

// IsConcurrencySafe 判断某次工具调用能不能跟别的调用并发执行。
//
// 工具自己实现了 ConcurrencySafeTool 就听它的，否则按类别兜底。
func IsConcurrencySafe(t Tool, args map[string]any) bool {
	if cs, ok := t.(ConcurrencySafeTool); ok {
		return cs.IsConcurrencySafe(args)
	}
	return t.Category() == CategoryRead
}

type Registry struct {
	tools           map[string]Tool
	discoveredTools map[string]bool
	// McpLoadingMode 由 mcp.DecideAndApply 在连上服务器后写入。没有 MCP 时保持
	// eager，行为等同于不延迟。
	McpLoadingMode McpLoadingMode

	// ExposeToolSearch / ExposeMcpCall 决定这两个工具发不发给模型，由
	// mcp.ApplyMode 在会话启动时算一次。不每轮按「当前还有没有延迟工具」现算：
	// 工具可能被运行时禁用，现算会让 tools[] 中途少一个，那就是一次数组变动，
	// 缓存前缀照样断。
	ExposeToolSearch bool
	ExposeMcpCall    bool
}

func NewRegistry() *Registry {
	return &Registry{
		tools:           make(map[string]Tool),
		discoveredTools: make(map[string]bool),
		McpLoadingMode:  McpLoadingEager,
	}
}

func (r *Registry) MarkDiscovered(name string) {
	r.discoveredTools[name] = true
}

func (r *Registry) IsDiscovered(name string) bool {
	return r.discoveredTools[name]
}

func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
}

func (r *Registry) Get(name string) Tool {
	return r.tools[name]
}

func (r *Registry) ListTools() []Tool {
	result := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

func isDeferred(t Tool) bool {
	if dt, ok := t.(DeferrableTool); ok {
		return dt.ShouldDefer()
	}
	return false
}

func isOpenAIProtocol(protocol string) bool {
	return protocol == "openai" || protocol == "openai-compat"
}

// GetAllSchemas 构建这一轮要发给模型的工具列表。
//
// 按工具名排序遍历，不直接遍历 map。Go 的 map 遍历顺序是随机的，不排序的话同一批
// 工具每次序列化出来的数组顺序都不同，而工具列表渲染在系统提示词之后、消息之前，
// 顺序一变整个块的字节就变了，它后面的对话历史缓存全部作废。内容没动、光顺序变，
// 代价跟真加了一个工具一样。
func (r *Registry) GetAllSchemas(protocol string) []map[string]any {
	// 官方端点走原生延迟：工具留在 tools[] 里但打上 defer_loading，由服务端决定
	// 给不给模型看。这样即使发现了新工具，tools 数组的字节也不变，prompt cache
	// 的前缀不会被打断。其他端点只能把延迟工具整个藏起来，靠 mcp_call 兜。
	native := r.McpLoadingMode == McpLoadingNative && !isOpenAIProtocol(protocol)
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	schemas := make([]map[string]any, 0, len(r.tools))
	for _, name := range names {
		t := r.tools[name]
		// 检索和分发只在用得上的模式里发。eager 下没有延迟工具可搜、也不需要
		// 分发，两个都发过去只是白占 token，还可能引诱模型去绕一圈。
		if name == ToolSearchToolName && !r.ExposeToolSearch {
			continue
		} else if name == McpCallToolName && !r.ExposeMcpCall {
			continue
		}
		deferred := isDeferred(t) && !r.discoveredTools[name]
		if deferred && !native {
			continue
		}
		base := t.Schema()
		if isOpenAIProtocol(protocol) {
			schemas = append(schemas, map[string]any{
				"type":        "function",
				"name":        base["name"],
				"description": base["description"],
				"parameters":  base["input_schema"],
			})
		} else {
			if deferred {
				withFlag := make(map[string]any, len(base)+1)
				for k, v := range base {
					withFlag[k] = v
				}
				withFlag["defer_loading"] = true
				base = withFlag
			}
			schemas = append(schemas, base)
		}
	}
	return schemas
}

// GetDeferredToolNames 返回还没被捞出来的延迟工具名，按字典序。
//
// 排序不是为了好看：tools 是 map，不排的话每次调用顺序都不同，同一批工具会拼出
// 不同的文本，调用方就没法靠比较判断这批工具到底变没变。
func (r *Registry) GetDeferredToolNames() []string {
	var names []string
	for _, t := range r.tools {
		if isDeferred(t) && !r.discoveredTools[t.Name()] {
			names = append(names, t.Name())
		}
	}
	sort.Strings(names)
	return names
}

func (r *Registry) GetDeferredTools() []Tool {
	var result []Tool
	for _, t := range r.tools {
		if isDeferred(t) {
			result = append(result, t)
		}
	}
	return result
}

func (r *Registry) SearchDeferred(query string, maxResults int, protocol string) []map[string]any {
	query = strings.ToLower(query)
	var matches []map[string]any
	for _, t := range r.tools {
		if !isDeferred(t) {
			continue
		}
		name := strings.ToLower(t.Name())
		desc := strings.ToLower(t.Description())
		if strings.Contains(name, query) || strings.Contains(desc, query) {
			base := t.Schema()
			if isOpenAIProtocol(protocol) {
				matches = append(matches, map[string]any{
					"type":        "function",
					"name":        base["name"],
					"description": base["description"],
					"parameters":  base["input_schema"],
				})
			} else {
				matches = append(matches, base)
			}
			if len(matches) >= maxResults {
				break
			}
		}
	}
	return matches
}

func (r *Registry) FindDeferredByNames(names []string, protocol string) []map[string]any {
	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[strings.ToLower(n)] = true
	}
	var matches []map[string]any
	for _, t := range r.tools {
		if nameSet[strings.ToLower(t.Name())] {
			base := t.Schema()
			if isOpenAIProtocol(protocol) {
				matches = append(matches, map[string]any{
					"type":        "function",
					"name":        base["name"],
					"description": base["description"],
					"parameters":  base["input_schema"],
				})
			} else {
				matches = append(matches, base)
			}
		}
	}
	return matches
}

type DefaultTools struct {
	Registry  *Registry
	WriteFile *WriteFileTool
	EditFile  *EditFileTool
}

func CreateDefaultRegistry() *Registry {
	dt := CreateDefaultTools()
	return dt.Registry
}

func CreateDefaultToolsWithWorkDir(workDir string) DefaultTools {
	fsc := NewFileStateCache()
	wf := &WriteFileTool{FileStateCache: fsc}
	ef := &EditFileTool{FileStateCache: fsc}
	reg := NewRegistry()
	reg.Register(&ReadFileTool{FileStateCache: fsc})
	reg.Register(wf)
	reg.Register(ef)
	reg.Register(&BashTool{WorkDir: workDir})
	reg.Register(&GlobTool{})
	reg.Register(&GrepTool{})
	return DefaultTools{Registry: reg, WriteFile: wf, EditFile: ef}
}

func CreateDefaultTools() DefaultTools {
	return CreateDefaultToolsWithWorkDir("")
}
