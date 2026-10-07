package agents

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentMemoryScope 表示持久记忆的位置：用户级、项目级或检出目录级（不纳入版本控制）。
type AgentMemoryScope string

const (
	AgentMemoryScopeUser    AgentMemoryScope = "user"
	AgentMemoryScopeProject AgentMemoryScope = "project"
	AgentMemoryScopeLocal   AgentMemoryScope = "local"
)

// IsolationMode 表示 frontmatter 中的 `isolation` 字段。
type IsolationMode string

const (
	IsolationWorktree IsolationMode = "worktree"
	IsolationRemote   IsolationMode = "remote"
)

// AgentDefinition 中尚未在运行时使用的字段（Effort、Skills、McpServers、Hooks、Memory、
// InitialPrompt、OmitCcodeMd、RequiredMcpServers）仍会被解析，避免用户定义往返处理时丢失数据，
// 也让后续通道无需再次迁移 schema 即可使用这些字段。
type AgentDefinition struct {
	AgentType       string   `yaml:"name"`
	WhenToUse       string   `yaml:"description"`
	Tools           []string `yaml:"tools"`
	DisallowedTools []string `yaml:"disallowedTools"`
	Model           string   `yaml:"model"`
	MaxTurns        int      `yaml:"maxTurns"`

	// permissionMode 覆盖此子 Agent 继承的父 Agent 权限模式。合法值与
	// internal/permissions.PermissionMode 一致。
	PermissionMode string `yaml:"permissionMode"`

	// Effort 是提供给模型的任务复杂度提示（"low" | "medium" | "high" | int）。
	// 目前只保存，尚未使用。
	Effort any `yaml:"effort"`

	// Skills 是子 Agent 启动时预加载的 Skill 名称。
	Skills []string `yaml:"skills"`

	// McpServers 是当前 Agent 范围内的 MCP 服务器名称或内联配置。这里保留为原始 any，
	// 便于后续加载逻辑同时解释字符串引用和内联配置。
	McpServers []any `yaml:"mcpServers"`

	// RequiredMcpServers 是 Agent 的加载门禁：加载时若列出的服务器不可用，
	// hasRequiredMcpServers 会过滤掉该 Agent。
	RequiredMcpServers []string `yaml:"requiredMcpServers"`

	// Hooks 是当前 Agent 启动时注册的会话级 Hook。这里保留原始 YAML，
	// 使用时由 hooks 包执行类型检查。
	Hooks any `yaml:"hooks"`

	// Memory 在三个作用域之一启用持久记忆。
	Memory AgentMemoryScope `yaml:"memory"`

	// Background 强制此 Agent 在生成后始终作为后台任务运行，不受
	// run_in_background 参数影响。
	Background bool `yaml:"background"`

	// Isolation 选择生成 Agent 时使用的文件系统隔离模式。
	Isolation IsolationMode `yaml:"isolation"`

	// InitialPrompt 会添加到第一轮用户消息之前（支持斜杠命令）。
	InitialPrompt string `yaml:"initialPrompt"`

	// OmitCcodeMd 从此 Agent 的用户上下文中移除 CCODE.md 层级。只读 Agent
	//（Explore、Plan）可通过跳过它节省 token。
	OmitCcodeMd bool `yaml:"omitCcodeMd"`

	// SystemPrompt 是定义文件的 Markdown 正文。
	SystemPrompt string `yaml:"-"`

	// FilePath、Source 和 Filename 在加载时填充。
	FilePath string `yaml:"-"`
	Source   string `yaml:"-"`
	Filename string `yaml:"-"`
}

// validPermissionModes 是 Agent 定义里 permission_mode 字段的合法取值，空串表示不覆盖、
// 沿用父 Agent 的模式。
var validPermissionModes = map[string]bool{
	"":                  true,
	"acceptEdits":       true,
	"bypassPermissions": true,
	"default":           true,
	"plan":              true,
}

var validMemoryScopes = map[AgentMemoryScope]bool{
	"":                      true,
	AgentMemoryScopeUser:    true,
	AgentMemoryScopeProject: true,
	AgentMemoryScopeLocal:   true,
}

var validIsolationModes = map[IsolationMode]bool{
	"":                true,
	IsolationWorktree: true,
	IsolationRemote:   true,
}

func ParseAgentFile(path string) (*AgentDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	var def AgentDefinition
	def.FilePath = path

	if strings.HasPrefix(strings.TrimSpace(content), "---") {
		parts := strings.SplitN(content, "---", 3)
		if len(parts) >= 3 {
			if err := yaml.Unmarshal([]byte(parts[1]), &def); err != nil {
				return nil, fmt.Errorf("parse frontmatter in %s: %w", path, err)
			}
			def.SystemPrompt = strings.TrimSpace(parts[2])
		}
	} else {
		def.SystemPrompt = strings.TrimSpace(content)
	}

	if def.AgentType == "" {
		return nil, fmt.Errorf("agent definition %s: missing required field 'name'", path)
	}
	if def.WhenToUse == "" {
		return nil, fmt.Errorf("agent definition %s: missing required field 'description'", path)
	}

	// 规范化并校验 `model`。规则与 AgentJsonSchema 一致：只要求它是非空字符串；
	// 实际可用性由宿主的 ModelResolver 或 LLM 路由器判断。"glm-5.1" 等第三方模型名
	// 必须能原样往返。不同大小写的 "inherit" 统一为小写哨兵值，表示使用父 Agent 的客户端；
	// 其它值保持原样，供路由器匹配。
	def.Model = strings.TrimSpace(def.Model)
	if strings.EqualFold(def.Model, "inherit") {
		def.Model = "inherit"
	}

	if !validPermissionModes[def.PermissionMode] {
		return nil, fmt.Errorf("agent definition %s: invalid permissionMode '%s'", path, def.PermissionMode)
	}

	if !validMemoryScopes[def.Memory] {
		return nil, fmt.Errorf("agent definition %s: invalid memory scope '%s'", path, def.Memory)
	}

	if !validIsolationModes[def.Isolation] {
		return nil, fmt.Errorf("agent definition %s: invalid isolation mode '%s'", path, def.Isolation)
	}

	return &def, nil
}

func (d *AgentDefinition) ToSpec() SubAgentSpec {
	return SubAgentSpec{
		Name:                 d.AgentType,
		Description:          d.WhenToUse,
		Tools:                d.Tools,
		DisallowedTools:      d.DisallowedTools,
		SystemPromptOverride: d.SystemPrompt,
		MaxTurns:             d.MaxTurns,
		Model:                d.Model,
		PermissionMode:       d.PermissionMode,
		Background:           d.Background,
		Isolation:            d.Isolation,
		InitialPrompt:        d.InitialPrompt,
		OmitCcodeMd:          d.OmitCcodeMd,
		Skills:               d.Skills,
		Memory:               d.Memory,
		McpServers:           d.McpServers,
		RequiredMcpServers:   d.RequiredMcpServers,
		Hooks:                d.Hooks,
		Effort:               d.Effort,
	}
}

// HasRequiredMcpServers 在 Agent 没有 MCP 要求，或每个必需模式都能匹配可用服务器名称时
// 返回 true；匹配采用不区分大小写的子串规则。
func (d *AgentDefinition) HasRequiredMcpServers(availableServers []string) bool {
	if len(d.RequiredMcpServers) == 0 {
		return true
	}
	for _, pattern := range d.RequiredMcpServers {
		patLower := strings.ToLower(pattern)
		matched := false
		for _, server := range availableServers {
			if strings.Contains(strings.ToLower(server), patLower) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
