// Package llmfactory 集中管理模型协议路由、批量预检和子 Agent 模型覆盖。
package llmfactory

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"ccode/internal/config"
	"ccode/internal/llm"
	llmanthropic "ccode/internal/llm/anthropic"
	llmopenai "ccode/internal/llm/openai"
	"ccode/internal/llmtransport"
)

type Options struct {
	BaseTransport http.RoundTripper
}

type Factory struct {
	opts Options
}

func NewFactory(opts Options) *Factory { return &Factory{opts: opts} }

// New 构造一个协议客户端。该过程只校验配置并装配 SDK，不执行网络访问。
func (f *Factory) New(cfg config.ProviderConfig, systemPrompt string) (llm.Client, error) {
	apiKey := cfg.ResolveAPIKey()
	switch cfg.Protocol {
	case config.ProtocolAnthropic:
		httpClient := llmtransport.New(cfg.BaseURL, f.opts.BaseTransport)
		client, err := llmanthropic.New(
			cfg.Name, cfg.Model, apiKey, cfg.BaseURL, cfg.Thinking,
			systemPrompt, cfg.GetMaxOutputTokens(), httpClient,
		)
		if err != nil {
			return nil, fmt.Errorf("Provider %q（anthropic）装配失败: %w", safeName(cfg.Name), err)
		}
		return client, nil
	case config.ProtocolOpenAI:
		httpClient := &http.Client{Transport: f.opts.BaseTransport}
		client, err := llmopenai.NewResponses(
			cfg.Name, cfg.Model, apiKey, cfg.BaseURL, cfg.Thinking,
			systemPrompt, cfg.GetMaxOutputTokens(), httpClient,
		)
		if err != nil {
			return nil, fmt.Errorf("Provider %q（openai）装配失败: %w", safeName(cfg.Name), err)
		}
		return client, nil
	case config.ProtocolOpenAICompat:
		httpClient := &http.Client{Transport: f.opts.BaseTransport}
		client, err := llmopenai.NewCompat(cfg.Name, cfg.Model, apiKey, cfg.BaseURL, systemPrompt, httpClient)
		if err != nil {
			return nil, fmt.Errorf("Provider %q（openai-compat）装配失败: %w", safeName(cfg.Name), err)
		}
		return client, nil
	default:
		return nil, fmt.Errorf("Provider %q 使用不支持的协议；仅支持 anthropic、openai、openai-compat: %w", safeName(cfg.Name), ErrUnknownProtocol)
	}
}

// NewAll 按配置顺序预构造全部客户端。任一失败时返回 nil，不暴露半成品列表。
func (f *Factory) NewAll(providers []config.ProviderConfig) ([]llm.Client, error) {
	clients := make([]llm.Client, 0, len(providers))
	for i := range providers {
		client, err := f.New(providers[i], "")
		if err != nil {
			return nil, fmt.Errorf("Provider #%d（%q）预检失败: %w", i+1, safeName(providers[i].Name), err)
		}
		clients = append(clients, client)
	}
	return clients, nil
}

var anthropicModelAliases = map[string]string{
	"haiku":  "claude-haiku-4-5-20251001",
	"sonnet": "claude-sonnet-4-6-20250514",
	"opus":   "claude-opus-4-6-20250514",
}

// NewModelResolver 返回沿用基准 Provider 的客户端构造函数。Anthropic 的内置
// 模型别名解析为完整 ID；其他协议收到这些别名时继承当前模型，避免向兼容端点
// 发送无效的 Claude 别名。明确给出的其它模型 ID 仍会覆盖当前模型。
func (f *Factory) NewModelResolver(base config.ProviderConfig) func(string) (llm.Client, error) {
	return func(model string) (llm.Client, error) {
		cfg := base
		cfg.Model = resolveModel(base, model)
		return f.New(cfg, "")
	}
}

func resolveModel(base config.ProviderConfig, requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" || strings.EqualFold(requested, "inherit") {
		return base.Model
	}
	if alias, ok := anthropicModelAliases[strings.ToLower(requested)]; ok {
		if base.Protocol == config.ProtocolAnthropic {
			return alias
		}
		return base.Model
	}
	return requested
}

var ErrUnknownProtocol = errors.New("不支持的模型协议")

func safeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "未命名"
	}
	if len([]rune(name)) > 80 {
		return string([]rune(name)[:80])
	}
	return name
}
