package tui

import (
	"context"
	"strings"
	"sync"
	"testing"

	"ccode/internal/agent"
	"ccode/internal/config"
	"ccode/internal/conversation"
	"ccode/internal/llm"
)

func TestBannerUsesFiveLineHollowASCIICcodeLogo(t *testing.T) {
	provider := config.ProviderConfig{Model: "test-model"}
	model := Model{providers: []config.ProviderConfig{provider}, selectedProvider: &provider}
	banner := model.renderBanner()
	lines := strings.Split(banner, "\n")
	if len(lines) != 5 {
		t.Fatalf("Logo 行数 = %d，want 5：%q", len(lines), banner)
	}
	wantLogo := []string{
		"  ____    ____    ___    ____    _____",
		` / ___|  / ___|  / _ \  |  _ \  | ____|`,
		`| |     | |     | | | | | | | | |  _|`,
		`| |___  | |___  | |_| | | |_| | | |___`,
		` \____|  \____|  \___/  |____/  |_____|`,
	}
	for i, want := range wantLogo {
		if !strings.Contains(lines[i], want) {
			t.Errorf("第 %d 行缺少 %q：%q", i+1, want, lines[i])
		}
	}
	if strings.Contains(banner, `/\_/\`) || strings.Contains(banner, "o.o") || strings.Contains(banner, "> ^ <") {
		t.Fatalf("Logo 仍包含小猫图案：%q", banner)
	}
	for _, metadata := range []string{"Ccode v0.1.0", "test-model", model.getWorkDir()} {
		if !strings.Contains(banner, metadata) {
			t.Errorf("Logo 缺少元数据 %q", metadata)
		}
	}
}

type fakeLLMClient struct{}

func (fakeLLMClient) Stream(context.Context, *conversation.Manager, []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	events := make(chan llm.StreamEvent)
	errs := make(chan error)
	close(events)
	close(errs)
	return events, errs
}

func (fakeLLMClient) SetSystemPrompt(string) {}

type fakeClientFactory struct {
	mu       sync.Mutex
	newCalls []config.ProviderConfig
	info     llm.ModelInfo
	infoErr  error
}

func (f *fakeClientFactory) New(cfg config.ProviderConfig, _ string) (llm.Client, error) {
	f.mu.Lock()
	f.newCalls = append(f.newCalls, cfg)
	f.mu.Unlock()
	return fakeLLMClient{}, nil
}

func (f *fakeClientFactory) NewModelResolver(base config.ProviderConfig) func(string) (llm.Client, error) {
	return func(model string) (llm.Client, error) {
		if model != "" && model != "inherit" {
			base.Model = model
		}
		return f.New(base, "")
	}
}

func (f *fakeClientFactory) ResolveModelInfo(context.Context, config.ProviderConfig) (llm.ModelInfo, error) {
	return f.info, f.infoErr
}

func TestSingleProviderUsesInjectedFactory(t *testing.T) {
	t.Chdir(t.TempDir())
	provider := config.ProviderConfig{Name: "p", Protocol: config.ProtocolOpenAI, APIKey: "key", Model: "model"}
	factory := &fakeClientFactory{}
	model := New([]config.ProviderConfig{provider}, nil, nil, factory)
	updated, _ := model.Update(initSingleProviderMsg{})
	got := updated.(Model)
	if got.client == nil || got.ag == nil {
		t.Fatal("单 Provider 初始化未创建客户端和 Agent")
	}
	if len(factory.newCalls) != 1 || factory.newCalls[0].Model != "model" {
		t.Fatalf("工厂调用 = %+v", factory.newCalls)
	}
}

func TestModelInfoUpdatesCurrentSelectionOnly(t *testing.T) {
	providers := []config.ProviderConfig{{Name: "p", Protocol: config.ProtocolOpenAI, APIKey: "key", Model: "model"}}
	model := Model{providers: providers, selectedProvider: &providers[0], ag: &agent.Agent{ContextWindow: 128000}}
	updated, _ := model.Update(modelInfoMsg{
		providerName: "p", modelID: "model",
		info: llm.ModelInfo{DisplayName: "Friendly", ContextWindow: 200000},
	})
	got := updated.(Model)
	if got.modelDisplayName != "Friendly" || got.ag.ContextWindow != 200000 {
		t.Fatalf("模型信息未更新: label=%q window=%d", got.modelDisplayName, got.ag.ContextWindow)
	}

	stale, _ := got.Update(modelInfoMsg{
		providerName: "other", modelID: "old",
		info: llm.ModelInfo{DisplayName: "Stale", ContextWindow: 1},
	})
	unchanged := stale.(Model)
	if unchanged.modelDisplayName != "Friendly" || unchanged.ag.ContextWindow != 200000 {
		t.Fatal("旧查询覆盖了当前 Provider")
	}
}
