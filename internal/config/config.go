package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"ccode/internal/hooks"
)

const (
	ProtocolAnthropic    = "anthropic"
	ProtocolOpenAI       = "openai"
	ProtocolOpenAICompat = "openai-compat"
)

var validProtocols = map[string]bool{
	ProtocolAnthropic:    true,
	ProtocolOpenAI:       true,
	ProtocolOpenAICompat: true,
}

var protocolAPIKeyEnvs = map[string]string{
	ProtocolAnthropic:    "ANTHROPIC_API_KEY",
	ProtocolOpenAI:       "OPENAI_API_KEY",
	ProtocolOpenAICompat: "OPENAI_API_KEY",
}

var providerAPIKeyEnvs = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"openai":    "OPENAI_API_KEY",
	"gemini":    "GEMINI_API_KEY",
	"google":    "GEMINI_API_KEY",
	"kimi":      "MOONSHOT_API_KEY",
	"moonshot":  "MOONSHOT_API_KEY",
	"glm":       "ZHIPUAI_API_KEY",
	"zhipu":     "ZHIPUAI_API_KEY",
	"deepseek":  "DEEPSEEK_API_KEY",
}

type ConfigError struct {
	Message string
}

func (e *ConfigError) Error() string { return e.Message }

type ProviderConfig struct {
	Name            string `yaml:"name"`
	Protocol        string `yaml:"protocol"`
	BaseURL         string `yaml:"base_url"`
	Model           string `yaml:"model"`
	APIKey          string `yaml:"api_key"`
	Thinking        bool   `yaml:"thinking"`
	ContextWindow   int    `yaml:"context_window"`
	MaxOutputTokens int    `yaml:"max_output_tokens"`

	// fetchedContextWindow 缓存模型信息查询返回的上下文容量。零值表示尚未取得
	// 有效容量；该字段只在运行期使用，不参与 YAML 编解码。
	fetchedContextWindow int
}

// UnmarshalYAML 对 Provider 节点执行字段白名单校验。错误只包含安全字段名，
// 不回显原始 YAML 或密钥值。
func (p *ProviderConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return &ConfigError{Message: "provider 必须是映射"}
	}
	allowed := map[string]bool{
		"name": true, "protocol": true, "base_url": true,
		"api_key": true, "model": true, "thinking": true,
		"context_window": true, "max_output_tokens": true,
	}
	seen := make(map[string]bool, len(allowed))
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !allowed[key] {
			return &ConfigError{Message: fmt.Sprintf("provider 包含未知字段 %q", key)}
		}
		if seen[key] {
			return &ConfigError{Message: fmt.Sprintf("provider 字段 %q 重复", key)}
		}
		seen[key] = true
	}
	type rawProvider struct {
		Name            string `yaml:"name"`
		Protocol        string `yaml:"protocol"`
		BaseURL         string `yaml:"base_url"`
		APIKey          string `yaml:"api_key"`
		Model           string `yaml:"model"`
		Thinking        bool   `yaml:"thinking"`
		ContextWindow   int    `yaml:"context_window"`
		MaxOutputTokens int    `yaml:"max_output_tokens"`
	}
	var raw rawProvider
	if err := node.Decode(&raw); err != nil {
		return &ConfigError{Message: "provider 字段类型无效"}
	}
	*p = ProviderConfig{
		Name:            strings.TrimSpace(raw.Name),
		Protocol:        strings.TrimSpace(raw.Protocol),
		BaseURL:         strings.TrimSpace(raw.BaseURL),
		APIKey:          raw.APIKey,
		Model:           strings.TrimSpace(raw.Model),
		Thinking:        raw.Thinking,
		ContextWindow:   raw.ContextWindow,
		MaxOutputTokens: raw.MaxOutputTokens,
	}
	return nil
}

// modelContextWindows 按模型名称子串提供本地容量回退。匹配从具体到通用，
// 第一个命中值生效。
var modelContextWindows = []struct {
	substr string
	window int
}{
	{"1m", 1000000},
	{"gpt-4.1", 1000000},
	{"gpt-4o", 128000},
	{"gpt-4-turbo", 128000},
	{"o1", 200000},
	{"o3", 200000},
	{"o4", 200000},
	{"gpt-3.5", 16385},
	{"claude", 200000},
}

// SetFetchedContextWindow 记录模型信息查询得到的上下文容量。非正值会被忽略，
// 因而失败查询不会污染本地回退结果。
func (p *ProviderConfig) SetFetchedContextWindow(window int) {
	if window > 0 {
		p.fetchedContextWindow = window
	}
}

// lookupModelContextWindow 返回模型名称映射值，未命中时返回零。
func lookupModelContextWindow(model string) int {
	m := strings.ToLower(model)
	for _, e := range modelContextWindows {
		if strings.Contains(m, e.substr) {
			return e.window
		}
	}
	return 0
}

// GetContextWindow 按“显式配置、服务端正值、模型名称映射、保守默认”的顺序解析容量。
func (p *ProviderConfig) GetContextWindow() int {
	if p.ContextWindow > 0 {
		return p.ContextWindow
	}
	if p.fetchedContextWindow > 0 {
		return p.fetchedContextWindow
	}
	if w := lookupModelContextWindow(p.Model); w > 0 {
		return w
	}
	if strings.Contains(p.Model, "claude") {
		return 200000
	}
	return 128000
}

func (p *ProviderConfig) GetMaxOutputTokens() int {
	if p.MaxOutputTokens > 0 {
		return p.MaxOutputTokens
	}
	if p.Thinking {
		return 64000
	}
	return 8192
}

// ResolveAPIKey 优先使用配置中的密钥；未配置时先按 Provider 名查找厂商环境变量，
// 再回退到协议的通用环境变量。这样兼容上游配置习惯，也允许六类服务分别设密钥。
func (p *ProviderConfig) ResolveAPIKey() string {
	if key := strings.TrimSpace(p.APIKey); key != "" {
		return key
	}
	if envName := providerAPIKeyEnvs[strings.ToLower(strings.TrimSpace(p.Name))]; envName != "" {
		if key := strings.TrimSpace(os.Getenv(envName)); key != "" {
			return key
		}
	}
	return strings.TrimSpace(os.Getenv(protocolAPIKeyEnvs[p.Protocol]))
}

type MCPServerConfig struct {
	Name      string            `yaml:"name"`
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	URL       string            `yaml:"url"`
	Transport string            `yaml:"transport"`
	Headers   map[string]string `yaml:"headers"`
	Env       map[string]string `yaml:"env"`
}

// SandboxConfig 控制 OS 级沙箱的配置
type SandboxConfig struct {
	Enabled        bool `yaml:"enabled"`         // 是否启用沙箱
	AutoAllow      bool `yaml:"auto_allow"`      // 沙箱内命令是否自动放行
	NetworkEnabled bool `yaml:"network_enabled"` // 是否允许网络访问
}

type AppConfig struct {
	Providers             []ProviderConfig  `yaml:"providers"`
	PermissionMode        string            `yaml:"permission_mode"`
	MCPServers            []MCPServerConfig `yaml:"mcp_servers"`
	Hooks                 []hooks.Hook      `yaml:"hooks"`
	Sandbox               SandboxConfig     `yaml:"sandbox"`
	EnableCoordinatorMode bool              `yaml:"enable_coordinator_mode"`

	// EnableFork 控制省略 subagent_type 时是否走 fork。用指针是因为它默认开着，
	// 普通 bool 分不清「配置里没写」和「配置里写了 false」，后者就永远关不掉。
	EnableFork *bool `yaml:"enable_fork"`
}

// ForkEnabled 返回 fork 是否可用。配置里没写就是开着。
func (c *AppConfig) ForkEnabled() bool {
	return c.EnableFork == nil || *c.EnableFork
}

func loadSingleFile(path string) (*AppConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &ConfigError{Message: "无法读取配置文件"}
	}
	defer func() { _ = f.Close() }()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var cfg AppConfig
	if err := dec.Decode(&cfg); err != nil {
		var configErr *ConfigError
		if errors.As(err, &configErr) {
			return nil, configErr
		}
		return nil, &ConfigError{Message: "配置文件不是有效的 YAML 或包含未知字段"}
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, &ConfigError{Message: "配置文件不能包含多个 YAML 文档"}
	} else if !errors.Is(err, io.EOF) {
		return nil, &ConfigError{Message: "配置文件不是有效的 YAML"}
	}
	return &cfg, nil
}

func mergeConfig(base, override *AppConfig) *AppConfig {
	if override.Providers != nil {
		base.Providers = override.Providers
	}
	if override.PermissionMode != "" {
		base.PermissionMode = override.PermissionMode
	}
	if len(override.MCPServers) > 0 {
		byName := make(map[string]int)
		for i, s := range base.MCPServers {
			byName[s.Name] = i
		}
		for _, s := range override.MCPServers {
			if idx, ok := byName[s.Name]; ok {
				base.MCPServers[idx] = s
			} else {
				base.MCPServers = append(base.MCPServers, s)
				byName[s.Name] = len(base.MCPServers) - 1
			}
		}
	}
	base.Hooks = append(base.Hooks, override.Hooks...)
	// 沙箱配置：后加载的配置覆盖先前的
	if override.Sandbox.Enabled {
		base.Sandbox = override.Sandbox
	}
	if override.EnableCoordinatorMode {
		base.EnableCoordinatorMode = true
	}
	if override.EnableFork != nil {
		base.EnableFork = override.EnableFork
	}
	return base
}

func validateProviders(cfg *AppConfig) error {
	if len(cfg.Providers) == 0 {
		return &ConfigError{Message: "At least one provider must be configured"}
	}
	requiredFields := []string{"name", "protocol", "model"}
	seen := make(map[string]int, len(cfg.Providers))
	for i, p := range cfg.Providers {
		var missing []string
		values := map[string]string{
			"name":     p.Name,
			"protocol": p.Protocol,
			"model":    p.Model,
		}
		for _, f := range requiredFields {
			if strings.TrimSpace(values[f]) == "" {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			return &ConfigError{
				Message: fmt.Sprintf("Provider #%d: missing fields: %s", i+1, strings.Join(missing, ", ")),
			}
		}
		if !validProtocols[p.Protocol] {
			return &ConfigError{
				Message: fmt.Sprintf("Provider #%d: invalid protocol %q, must be one of: anthropic, openai, openai-compat", i+1, p.Protocol),
			}
		}
		if p.ContextWindow < 0 {
			return &ConfigError{Message: fmt.Sprintf("Provider #%d: context_window 不能为负数", i+1)}
		}
		if p.MaxOutputTokens < 0 {
			return &ConfigError{Message: fmt.Sprintf("Provider #%d: max_output_tokens 不能为负数", i+1)}
		}
		if first, ok := seen[p.Name]; ok {
			return &ConfigError{Message: fmt.Sprintf("Provider #%d: duplicate name %q (first declared at #%d)", i+1, p.Name, first+1)}
		}
		seen[p.Name] = i
		if err := validateBaseURL(p.BaseURL, i); err != nil {
			return err
		}
	}
	return nil
}

// validateBaseURL 校验可选的完整请求地址。空值表示使用 SDK 官方默认地址；
// 非空值必须是带主机的 HTTP(S) URL，且不能携带用户信息或片段。
func validateBaseURL(raw string, index int) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	u, err := url.Parse(raw)
	field := fmt.Sprintf("Provider #%d base_url", index+1)
	if err != nil {
		return &ConfigError{Message: field + ": invalid URL"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return &ConfigError{Message: field + ": scheme must be http or https"}
	}
	if u.Host == "" {
		return &ConfigError{Message: field + ": host is required"}
	}
	if u.User != nil {
		return &ConfigError{Message: field + ": user info is not allowed"}
	}
	if u.Fragment != "" {
		return &ConfigError{Message: field + ": fragment is not allowed"}
	}
	return nil
}

func LoadConfig(path string) (*AppConfig, error) {
	if path != "" {
		cfg, err := loadSingleFile(path)
		if err != nil {
			return nil, err
		}
		if err := validateProviders(cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return nil, &ConfigError{Message: fmt.Sprintf("Failed to get working directory: %s", err)}
	}

	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".ccode", "config.yaml"),
		filepath.Join(wd, ".ccode", "config.yaml"),
		filepath.Join(wd, ".ccode", "config.local.yaml"),
	}

	var merged *AppConfig
	for _, path := range candidates {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		layer, err := loadSingleFile(path)
		if err != nil {
			return nil, err
		}
		if merged == nil {
			merged = layer
		} else {
			merged = mergeConfig(merged, layer)
		}
	}

	if merged == nil {
		return nil, &ConfigError{Message: "未找到配置文件，请在项目或用户目录的 .ccode/config.yaml 中配置"}
	}

	if err := validateProviders(merged); err != nil {
		return nil, err
	}
	return merged, nil
}
