package remote

import (
	"context"
	"strings"
	"testing"

	"ccode/internal/config"
	"ccode/internal/conversation"
	"ccode/internal/llm"
)

type fakeClient struct{}

func (fakeClient) Stream(context.Context, *conversation.Manager, []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	events := make(chan llm.StreamEvent)
	errs := make(chan error)
	close(events)
	close(errs)
	return events, errs
}

func (fakeClient) SetSystemPrompt(string) {}

type fakeFactory struct{ calls int }

func (f *fakeFactory) New(config.ProviderConfig, string) (llm.Client, error) {
	f.calls++
	return fakeClient{}, nil
}

func (f *fakeFactory) NewModelResolver(base config.ProviderConfig) func(string) (llm.Client, error) {
	return func(string) (llm.Client, error) { return f.New(base, "") }
}

func TestServerUsesInjectedFactory(t *testing.T) {
	t.Chdir(t.TempDir())
	factory := &fakeFactory{}
	server := NewServer([]config.ProviderConfig{{
		Name: "p", Protocol: config.ProtocolOpenAI, APIKey: "key", Model: "model",
	}}, nil, nil, ":0", false, false, factory)
	if err := server.initAgent(); err != nil {
		t.Fatal(err)
	}
	if factory.calls != 1 || server.client == nil || server.ag == nil {
		t.Fatalf("factory calls=%d client=%v agent=%v", factory.calls, server.client, server.ag)
	}
}

func TestThinkingAgentEventsBecomeWebSocketMessages(t *testing.T) {
	message := thinkingWSMessage("分段思考")
	if message.Type != "thinking_text" {
		t.Fatalf("消息类型 = %q", message.Type)
	}
	data, ok := message.Data.(map[string]string)
	if !ok || data["text"] != "分段思考" {
		t.Fatalf("消息内容 = %#v", message.Data)
	}
}

func TestRemoteWebThinkingContract(t *testing.T) {
	required := []string{
		".thinking-block",
		"case 'thinking_text':",
		"currentThinkingText += msg.data.text",
		"function addThinking()",
		"function finishThinking()",
		"function toggleThinking(header)",
		"text.match(/^<think>",
		"escapeHtml(thinkText)",
		"<title>Ccode Remote</title>",
	}
	for _, value := range required {
		if !containsIndexHTML(value) {
			t.Errorf("远程页面缺少思考契约 %q", value)
		}
	}
}

func containsIndexHTML(value string) bool {
	return strings.Contains(indexHTML, value)
}
