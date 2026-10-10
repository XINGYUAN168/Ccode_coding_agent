package llmfactory

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ccode/internal/config"
)

func TestDeriveModelsURL(t *testing.T) {
	tests := []struct {
		protocol string
		input    string
		want     string
		ok       bool
	}{
		{config.ProtocolAnthropic, "https://api.anthropic.com/v1/messages?q=1", "https://api.anthropic.com/v1/models", true},
		{config.ProtocolOpenAI, "https://api.openai.com/v1/responses", "https://api.openai.com/v1/models", true},
		{config.ProtocolOpenAICompat, "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", "https://generativelanguage.googleapis.com/v1beta/openai/models", true},
		{config.ProtocolOpenAICompat, "https://api.moonshot.cn/v1/chat/completions", "https://api.moonshot.cn/v1/models", true},
		{config.ProtocolOpenAICompat, "https://open.bigmodel.cn/api/paas/v4/chat/completions", "https://open.bigmodel.cn/api/paas/v4/models", true},
		{config.ProtocolOpenAICompat, "https://api.deepseek.com/v1/chat/completions", "https://api.deepseek.com/v1/models", true},
		{config.ProtocolOpenAI, "https://example.test/custom", "", false},
	}
	for _, tt := range tests {
		got, ok := deriveModelsURL(tt.protocol, tt.input)
		if got != tt.want || ok != tt.ok {
			t.Errorf("deriveModelsURL(%q) = %q,%v，want %q,%v", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

func TestResolveModelInfoProviderMatrix(t *testing.T) {
	tests := []struct {
		name       string
		protocol   string
		chatURL    string
		modelsURL  string
		headerName string
		headerWant string
	}{
		{name: "anthropic", protocol: config.ProtocolAnthropic, chatURL: "https://api.anthropic.com/v1/messages", modelsURL: "https://api.anthropic.com/v1/models", headerName: "x-api-key", headerWant: "explicit-key"},
		{name: "openai", protocol: config.ProtocolOpenAI, chatURL: "https://api.openai.com/v1/responses", modelsURL: "https://api.openai.com/v1/models", headerName: "Authorization", headerWant: "Bearer explicit-key"},
		{name: "gemini", protocol: config.ProtocolOpenAICompat, chatURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", modelsURL: "https://generativelanguage.googleapis.com/v1beta/openai/models", headerName: "Authorization", headerWant: "Bearer explicit-key"},
		{name: "kimi", protocol: config.ProtocolOpenAICompat, chatURL: "https://api.moonshot.cn/v1/chat/completions", modelsURL: "https://api.moonshot.cn/v1/models", headerName: "Authorization", headerWant: "Bearer explicit-key"},
		{name: "glm", protocol: config.ProtocolOpenAICompat, chatURL: "https://open.bigmodel.cn/api/paas/v4/chat/completions", modelsURL: "https://open.bigmodel.cn/api/paas/v4/models", headerName: "Authorization", headerWant: "Bearer explicit-key"},
		{name: "deepseek", protocol: config.ProtocolOpenAICompat, chatURL: "https://api.deepseek.com/v1/chat/completions", modelsURL: "https://api.deepseek.com/v1/models", headerName: "Authorization", headerWant: "Bearer explicit-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			factory := NewFactory(Options{BaseTransport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if req.URL.String() != tt.modelsURL {
					t.Errorf("模型地址 = %q，want %q", req.URL.String(), tt.modelsURL)
				}
				if got := req.Header.Get(tt.headerName); got != tt.headerWant {
					t.Errorf("认证头 = %q，want %q", got, tt.headerWant)
				}
				return &http.Response{
					StatusCode: 200,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"target-model","display_name":"Target Model","max_input_tokens":256000}]}`)),
					Request:    req,
				}, nil
			})})
			info, err := factory.ResolveModelInfo(context.Background(), config.ProviderConfig{
				Name: tt.name, Protocol: tt.protocol, BaseURL: tt.chatURL, APIKey: "explicit-key", Model: "target-model",
			})
			if err != nil {
				t.Fatal(err)
			}
			if info.ID != "target-model" || info.DisplayName != "Target Model" || info.ContextWindow != 256000 || calls.Load() != 1 {
				t.Fatalf("info=%+v calls=%d", info, calls.Load())
			}
		})
	}
}

func TestResolveModelInfoFallbacks(t *testing.T) {
	tests := []struct {
		name      string
		transport http.RoundTripper
	}{
		{
			name: "损坏响应",
			transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("not-json")), Request: req}, nil
			}),
		},
		{
			name: "没有匹配模型",
			transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"other"}]}`)), Request: req}, nil
			}),
		},
		{
			name: "请求已取消",
			transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.name == "请求已取消" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			factory := NewFactory(Options{BaseTransport: tt.transport})
			_, err := factory.ResolveModelInfo(ctx, config.ProviderConfig{
				Name: "provider", Protocol: config.ProtocolOpenAICompat,
				BaseURL: "https://example.test/v1/chat/completions", APIKey: "explicit-key", Model: "target-model",
			})
			if !errors.Is(err, ErrModelInfoUnavailable) {
				t.Fatalf("错误 = %v，want ErrModelInfoUnavailable", err)
			}
		})
	}
}

func TestResolveModelInfo(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.String() != "https://example.test/v1/models" {
			t.Errorf("模型地址 = %q", req.URL.String())
		}
		if req.Header.Get("Authorization") != "Bearer explicit-key" {
			t.Errorf("认证头不正确")
		}
		return &http.Response{
			StatusCode: 200,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"model-a","display_name":"Model A","max_input_tokens":200000}]}`)),
			Request:    req,
		}, nil
	})
	factory := NewFactory(Options{BaseTransport: transport})
	info, err := factory.ResolveModelInfo(context.Background(), config.ProviderConfig{
		Name: "provider", Protocol: config.ProtocolOpenAICompat,
		BaseURL: "https://example.test/v1/chat/completions?q=chat", APIKey: "explicit-key", Model: "model-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.DisplayName != "Model A" || info.ContextWindow != 200000 || calls.Load() != 1 {
		t.Fatalf("info=%+v calls=%d", info, calls.Load())
	}
}

func TestResolveModelInfoUnknownEndpointDoesNotRequest(t *testing.T) {
	var calls atomic.Int32
	factory := NewFactory(Options{BaseTransport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, nil
	})})
	_, err := factory.ResolveModelInfo(context.Background(), config.ProviderConfig{
		Name: "provider", Protocol: config.ProtocolOpenAI,
		BaseURL: "https://example.test/custom", APIKey: "secret", Model: "model-a",
	})
	if err == nil || calls.Load() != 0 {
		t.Fatalf("未知路径应零请求回退，calls=%d err=%v", calls.Load(), err)
	}
}

func TestResolveModelInfoFailureIsSafe(t *testing.T) {
	canary := "sk-canary-ModelInfo-Secret-123456789"
	factory := NewFactory(Options{BaseTransport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 500,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(canary)),
			Request:    req,
		}, nil
	})})
	_, err := factory.ResolveModelInfo(context.Background(), config.ProviderConfig{
		Name: "p", Protocol: config.ProtocolOpenAICompat,
		BaseURL: "https://example.test/v1/chat/completions", APIKey: canary, Model: "m",
	})
	if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "example.test") {
		t.Fatalf("失败结果不安全: %v", err)
	}
}
