package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"ccode/internal/config"
	"ccode/internal/llmfactory"
)

type immediateQuitModel struct{}

func (immediateQuitModel) Init() tea.Cmd { return tea.Quit }

func (m immediateQuitModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }

func (immediateQuitModel) View() string { return "" }

type countingTransport struct{ calls atomic.Int32 }

func (t *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("unexpected network")
}

func TestStreamJSONThinkingPayload(t *testing.T) {
	payload, err := json.Marshal(thinkingStreamJSON("分析\n\"路径\""))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"text":"分析\n\"路径\"","type":"thinking"}`
	if string(payload) != want {
		t.Fatalf("思考事件 JSON = %s，want %s", payload, want)
	}
}

func TestProviderPreflight(t *testing.T) {
	transport := &countingTransport{}
	factory := llmfactory.NewFactory(llmfactory.Options{BaseTransport: transport})
	providers := []config.ProviderConfig{
		{Name: "first", Protocol: config.ProtocolAnthropic, APIKey: "key-a", Model: "a"},
		{Name: "second", Protocol: config.ProtocolOpenAI, APIKey: "key-b", Model: "b"},
	}
	if err := preflightProviders(factory, providers); err != nil {
		t.Fatal(err)
	}
	if transport.calls.Load() != 0 {
		t.Fatalf("预检阶段发起了 %d 次网络请求", transport.calls.Load())
	}
}

func TestStartupErrorDoesNotLeakSecret(t *testing.T) {
	canary := "sk-canary-Startup-Secret-123456789"
	factory := llmfactory.NewFactory(llmfactory.Options{})
	err := preflightProviders(factory, []config.ProviderConfig{{
		Name: "bad", Protocol: "unsupported", APIKey: canary, Model: "m",
	}})
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("预检错误不安全: %v", err)
	}
}

func TestInteractiveProgramUsesAlternateScreen(t *testing.T) {
	var output bytes.Buffer
	p := newInteractiveProgram(
		immediateQuitModel{},
		tea.WithInput(nil),
		tea.WithOutput(&output),
	)
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	if !strings.Contains(rendered, "\x1b[?1049h") {
		t.Fatalf("交互程序没有进入备用屏幕：%q", rendered)
	}
	if !strings.Contains(rendered, "\x1b[?1049l") {
		t.Fatalf("交互程序退出时没有恢复主屏幕：%q", rendered)
	}
}
