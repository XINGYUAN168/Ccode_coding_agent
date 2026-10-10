package llmfactory

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ccode/internal/config"
	llmopenai "ccode/internal/llm/openai"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFactoryRoutesAndDoesNotUseNetwork(t *testing.T) {
	var calls atomic.Int32
	factory := NewFactory(Options{BaseTransport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected network")
	})})
	providers := []config.ProviderConfig{
		{Name: "a", Protocol: config.ProtocolAnthropic, APIKey: "explicit-a", Model: "claude-test"},
		{Name: "b", Protocol: config.ProtocolOpenAI, APIKey: "explicit-b", Model: "gpt-test"},
		{Name: "c", Protocol: config.ProtocolOpenAICompat, APIKey: "explicit-c", Model: "compat-test"},
	}
	clients, err := factory.NewAll(providers)
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 3 || calls.Load() != 0 {
		t.Fatalf("clients=%d network=%d", len(clients), calls.Load())
	}
}

func TestFactoryFailureIsAtomicAndSafe(t *testing.T) {
	canary := "sk-canary-Factory-Secret-123456789"
	factory := NewFactory(Options{})
	providers := []config.ProviderConfig{
		{Name: "first", Protocol: config.ProtocolOpenAI, APIKey: "key", Model: "m"},
		{Name: "broken", Protocol: "not-supported", APIKey: canary, Model: "m"},
	}
	clients, err := factory.NewAll(providers)
	if err == nil || clients != nil {
		t.Fatalf("非法列表应原子失败，clients=%v err=%v", clients, err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("错误泄漏密钥: %v", err)
	}
}

func TestModelResolver(t *testing.T) {
	factory := NewFactory(Options{})
	base := config.ProviderConfig{Name: "p", Protocol: config.ProtocolOpenAI, APIKey: "key", Model: "base"}
	resolver := factory.NewModelResolver(base)
	for _, model := range []string{"", "inherit", "override"} {
		if _, err := resolver(model); err != nil {
			t.Fatalf("model %q: %v", model, err)
		}
	}
}

func TestResolveModelAliases(t *testing.T) {
	cases := []struct {
		name      string
		protocol  string
		requested string
		want      string
	}{
		{name: "Anthropic haiku", protocol: config.ProtocolAnthropic, requested: "haiku", want: "claude-haiku-4-5-20251001"},
		{name: "Anthropic sonnet", protocol: config.ProtocolAnthropic, requested: "sonnet", want: "claude-sonnet-4-6-20250514"},
		{name: "Anthropic opus", protocol: config.ProtocolAnthropic, requested: "opus", want: "claude-opus-4-6-20250514"},
		{name: "兼容端点不发送 Claude 别名", protocol: config.ProtocolOpenAICompat, requested: "haiku", want: "base-model"},
		{name: "OpenAI 不发送 Claude 别名", protocol: config.ProtocolOpenAI, requested: "sonnet", want: "base-model"},
		{name: "显式模型原样保留", protocol: config.ProtocolOpenAICompat, requested: "Vendor/Model-X", want: "Vendor/Model-X"},
		{name: "inherit 继承", protocol: config.ProtocolAnthropic, requested: "inherit", want: "base-model"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			base := config.ProviderConfig{Protocol: tt.protocol, Model: "base-model"}
			if got := resolveModel(base, tt.requested); got != tt.want {
				t.Fatalf("resolveModel(%q) = %q，want %q", tt.requested, got, tt.want)
			}
		})
	}
}

func TestFactoryOpenAIUsesInjectedTransport(t *testing.T) {
	factory := NewFactory(Options{BaseTransport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})})
	if _, err := factory.New(config.ProviderConfig{Name: "p", Protocol: config.ProtocolOpenAI, APIKey: "key", Model: "m"}, "system"); err != nil {
		t.Fatal(err)
	}
}

func TestFactoryRoutesOpenAICompatibleProviders(t *testing.T) {
	factory := NewFactory(Options{})
	providers := []config.ProviderConfig{
		{Name: "gemini", Protocol: config.ProtocolOpenAICompat, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", APIKey: "key", Model: "gemini-2.5-pro"},
		{Name: "kimi", Protocol: config.ProtocolOpenAICompat, BaseURL: "https://api.moonshot.cn/v1/chat/completions", APIKey: "key", Model: "kimi-k2.5"},
		{Name: "glm", Protocol: config.ProtocolOpenAICompat, BaseURL: "https://open.bigmodel.cn/api/paas/v4/chat/completions", APIKey: "key", Model: "glm-4.5"},
		{Name: "deepseek", Protocol: config.ProtocolOpenAICompat, BaseURL: "https://api.deepseek.com/v1/chat/completions", APIKey: "key", Model: "deepseek-chat"},
	}
	for _, provider := range providers {
		t.Run(provider.Name, func(t *testing.T) {
			client, err := factory.New(provider, "system")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := client.(*llmopenai.Client); !ok {
				t.Fatalf("%s 未路由到 OpenAI Chat Completions 适配器: %T", provider.Name, client)
			}
		})
	}
}

func TestFactoryRoutesOfficialOpenAIToResponses(t *testing.T) {
	factory := NewFactory(Options{})
	client, err := factory.New(config.ProviderConfig{
		Name: "openai", Protocol: config.ProtocolOpenAI, APIKey: "key", Model: "gpt-5",
	}, "system")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.(*llmopenai.ResponsesClient); !ok {
		t.Fatalf("OpenAI 官方协议未路由到 Responses 适配器: %T", client)
	}
}
