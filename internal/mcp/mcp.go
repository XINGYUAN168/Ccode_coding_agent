package mcp

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ccode/internal/tools"
)

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// legacyProtocolVersion 是 initialize 握手时代的最后一个协议版本
const legacyProtocolVersion = "2025-11-25"

type ServerConfig struct {
	Name      string            `yaml:"name"`
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	URL       string            `yaml:"url"`
	Transport string            `yaml:"transport"`
	Headers   map[string]string `yaml:"headers"`
	Env       map[string]string `yaml:"env"`
}

func (c *ServerConfig) IsStdio() bool {
	return c.Command != ""
}

// transportKind 选 HTTP 传输：空/"http"/"streamable" 走 Streamable HTTP；
// "sse" 走 HTTP+SSE。HTTP+SSE 在 2026-07-28 规范里已弃用，只用来连还没迁移的老服务器。
func (c *ServerConfig) transportKind() string {
	switch strings.ToLower(c.Transport) {
	case "sse":
		return "sse"
	default:
		return "http"
	}
}

// headerRoundTripper 为每个出站请求注入固定请求头。
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range h.headers {
		clone.Header.Set(k, os.ExpandEnv(v))
	}
	return h.base.RoundTrip(clone)
}

func newHTTPClient(headers map[string]string) *http.Client {
	if len(headers) == 0 {
		return http.DefaultClient
	}
	return &http.Client{
		Transport: &headerRoundTripper{
			base:    http.DefaultTransport,
			headers: headers,
		},
	}
}

type Client struct {
	config    ServerConfig
	session   *mcp.ClientSession
	sdkClient *mcp.Client
}

func NewClient(config ServerConfig) *Client {
	return &Client{config: config}
}

func (c *Client) Connect(ctx context.Context) error {
	impl := &mcp.Implementation{Name: "ccode", Version: "0.1.0"}
	c.sdkClient = mcp.NewClient(impl, nil)

	var transport mcp.Transport
	var sessionOpts *mcp.ClientSessionOptions
	switch {
	case c.config.IsStdio():
		cmd := exec.Command(c.config.Command, c.config.Args...)
		cmd.Env = os.Environ()
		for k, v := range c.config.Env {
			cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
		}
		// 将 stderr 与父 TTY 分离。否则子进程（npx/node）会检测到 stderr 是 TTY 并发送
		// OSC 颜色查询，终端随后把响应写入控制进程的 stdin，污染 TUI 输入。
		cmd.Stderr = io.Discard
		// 老服务器可能对 discover 探测不回应，stdio 上给探测加超时，超时按老服务器处理
		transport = &discoverTimeoutTransport{Transport: &mcp.CommandTransport{Command: cmd}}
	case c.config.URL != "":
		httpClient := newHTTPClient(c.config.Headers)
		if c.config.transportKind() == "sse" {
			transport = &mcp.SSEClientTransport{Endpoint: c.config.URL, HTTPClient: httpClient}
			// HTTP+SSE 服务器只会说旧协议，直接走 initialize 握手，跳过 server/discover 探测。
			// 老 SSE 服务器收到不认识的 discover 可能直接断开事件流，探测反而连不上。
			sessionOpts = &mcp.ClientSessionOptions{ProtocolVersion: legacyProtocolVersion}
		} else {
			transport = &mcp.StreamableClientTransport{Endpoint: c.config.URL, HTTPClient: httpClient}
		}
	default:
		return fmt.Errorf("MCP server %s: neither command nor url configured", c.config.Name)
	}

	// stdio 和 Streamable HTTP 的协议版本协商由 SDK 完成：先发 server/discover 按 2026-07-28 无状态协议探测，
	// 服务器支持就直接用，之后每个请求在 _meta 里带协议版本和客户端能力；
	// discover 报错（老服务器不认识这个方法，stdio 上还包括超时）就在同一连接上回退到 initialize 握手，
	// 按 2025-11-25 及更早的协议走。
	session, err := c.sdkClient.Connect(ctx, transport, sessionOpts)
	if err != nil {
		return fmt.Errorf("connect MCP server %s: %w", c.config.Name, err)
	}
	c.session = session
	return nil
}

func (c *Client) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	result, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return result.Tools, nil
}

func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return "", true, err
	}
	var parts []string
	for _, content := range result.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" {
		text = "(no output)"
	}
	return text, result.IsError, nil
}

func (c *Client) Close() {
	if c.session != nil {
		c.session.Close()
	}
}

// Manager 管理多个 MCP 服务器。

type Manager struct {
	configs map[string]ServerConfig
	clients map[string]*Client
}

func NewManager() *Manager {
	return &Manager{
		configs: make(map[string]ServerConfig),
		clients: make(map[string]*Client),
	}
}

func (m *Manager) LoadConfigs(configs []ServerConfig) {
	for _, cfg := range configs {
		m.configs[cfg.Name] = cfg
	}
}

type ServerInfo struct {
	Name         string
	Instructions string
}

type ConnectResult struct {
	Mgr     *Manager
	Tools   []tools.Tool
	Servers []ServerInfo
	Errors  []string
}

func (m *Manager) ConnectAll(ctx context.Context) ConnectResult {
	var errs []string
	var registered []tools.Tool
	var servers []ServerInfo
	for name, cfg := range m.configs {
		client := NewClient(cfg)
		if err := client.Connect(ctx); err != nil {
			msg := fmt.Sprintf("MCP server '%s': %s", name, err)
			log.Println(msg)
			errs = append(errs, msg)
			continue
		}
		m.clients[name] = client

		info := ServerInfo{Name: name}
		// 两种协议下 SDK 都把服务器信息放进 InitializeResult：现代服务器来自 server/discover 的结果
		if initResult := client.session.InitializeResult(); initResult != nil {
			info.Instructions = initResult.Instructions
		}
		servers = append(servers, info)

		toolDefs, err := client.ListTools(ctx)
		if err != nil {
			msg := fmt.Sprintf("MCP server '%s' list tools: %s", name, err)
			log.Println(msg)
			errs = append(errs, msg)
			continue
		}

		for _, td := range toolDefs {
			registered = append(registered, &MCPToolWrapper{
				serverName: name,
				toolDef:    td,
				client:     client,
			})
		}
	}
	return ConnectResult{Mgr: m, Tools: registered, Servers: servers, Errors: errs}
}

func (m *Manager) RegisterAllTools(ctx context.Context, registry *tools.Registry) []string {
	result := m.ConnectAll(ctx)
	for _, t := range result.Tools {
		registry.Register(t)
	}
	return result.Errors
}

func (m *Manager) Shutdown() {
	for _, client := range m.clients {
		client.Close()
	}
	m.clients = make(map[string]*Client)
}

// MCPToolWrapper 把 MCP 工具适配为 Tool 接口。

type MCPToolWrapper struct {
	serverName string
	toolDef    *mcp.Tool
	client     *Client
	// noDefer 由 eager 模式置位：schema 总量不大时 MCP 工具直接进 tools[]，
	// 不必绕 ToolSearch
	noDefer bool
}

func (w *MCPToolWrapper) Name() string {
	return MCPToolNamePrefix(w.serverName) + SanitizeName(w.toolDef.Name)
}

func SanitizeName(name string) string {
	return nonAlphanumeric.ReplaceAllString(name, "_")
}

// MCPToolNamePrefix 是某个服务器下所有工具名的公共前缀。按服务器筛工具的地方
// 都该用它，自己拼字符串会漏掉 sanitize——服务器名里的横杠会被换成下划线。
func MCPToolNamePrefix(serverName string) string {
	return "mcp__" + SanitizeName(serverName) + "__"
}

func (w *MCPToolWrapper) Description() string          { return w.toolDef.Description }
func (w *MCPToolWrapper) Category() tools.ToolCategory { return tools.CategoryCommand }
func (w *MCPToolWrapper) ShouldDefer() bool            { return !w.noDefer }
func (w *MCPToolWrapper) SetDeferLoading(on bool)      { w.noDefer = !on }
func (w *MCPToolWrapper) MCPServerName() string        { return w.serverName }

// MCPInputSchema 返回原始 JSON schema。mcp_call 的参数强转要按它逐层走。
func (w *MCPToolWrapper) MCPInputSchema() map[string]any {
	if w.toolDef.InputSchema == nil {
		return map[string]any{}
	}
	if m, ok := w.toolDef.InputSchema.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func (w *MCPToolWrapper) Schema() map[string]any {
	var inputSchema any = w.toolDef.InputSchema
	if inputSchema == nil {
		inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return map[string]any{
		"name":         w.Name(),
		"description":  w.Description(),
		"input_schema": inputSchema,
	}
}

func (w *MCPToolWrapper) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	text, isError, err := w.client.CallTool(ctx, w.toolDef.Name, args)
	if err != nil {
		return tools.ToolResult{Output: fmt.Sprintf("MCP tool call failed: %s", err), IsError: true}
	}
	return tools.ToolResult{Output: text, IsError: isError}
}
