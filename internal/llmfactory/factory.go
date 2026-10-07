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
	switch cfg.Protocol {
	case config.ProtocolAnthropic:
		httpClient := llmtransport.New(cfg.BaseURL, f.opts.BaseTransport)
		client, err := llmanthropic.New(
			cfg.Name, cfg.Model, cfg.APIKey, cfg.BaseURL, cfg.Thinking,
			systemPrompt, cfg.GetMaxOutputTokens(), httpClient,
		)
		if err != nil {
			return nil, fmt.Errorf("Provider %q（anthropic）装配失败: %w", safeName(cfg.Name), err)
		}
		return client, nil
	case config.ProtocolOpenAI:
		httpClient := &http.Client{Transport: f.opts.BaseTransport}
		client, err := llmopenai.New(cfg.Name, cfg.Model, cfg.APIKey, cfg.BaseURL, cfg.Thinking, systemPrompt, httpClient)
		if err != nil {
			return nil, fmt.Errorf("Provider %q（openai）装配失败: %w", safeName(cfg.Name), err)
		}
		return client, nil
	default:
		return nil, fmt.Errorf("Provider %q 使用不支持的协议；仅支持 anthropic、openai: %w", safeName(cfg.Name), ErrUnknownProtocol)
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

// NewModelResolver 返回沿用基准 Provider、仅允许覆盖模型 ID 的客户端构造函数。
func (f *Factory) NewModelResolver(base config.ProviderConfig) func(string) (llm.Client, error) {
	return func(model string) (llm.Client, error) {
		cfg := base
		model = strings.TrimSpace(model)
		if model != "" && model != "inherit" {
			cfg.Model = model
		}
		return f.New(cfg, "")
	}
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
