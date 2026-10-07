package config

import "testing"

func TestGetContextWindow(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		explicit int
		fetched  int
		want     int
	}{
		{name: "显式配置优先", model: "gpt-4o", explicit: 654321, fetched: 321000, want: 654321},
		{name: "服务端容量优先", model: "gpt-4o", fetched: 321000, want: 321000},
		{name: "百万上下文标识", model: "claude-sonnet-4-6-1m", want: 1000000},
		{name: "GPT 4.1", model: "gpt-4.1-mini", want: 1000000},
		{name: "GPT 4o", model: "gpt-4o-mini", want: 128000},
		{name: "推理模型", model: "o3-mini", want: 200000},
		{name: "Claude 默认", model: "claude-sonnet-4-6", want: 200000},
		{name: "未知模型", model: "unknown-model", want: 128000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &ProviderConfig{Model: tt.model, ContextWindow: tt.explicit}
			p.SetFetchedContextWindow(tt.fetched)
			if got := p.GetContextWindow(); got != tt.want {
				t.Fatalf("GetContextWindow() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSetFetchedContextWindowIgnoresInvalidValues(t *testing.T) {
	p := &ProviderConfig{Model: "gpt-4o"}
	p.SetFetchedContextWindow(-1)
	if got := p.GetContextWindow(); got != 128000 {
		t.Fatalf("无效容量不应覆盖回退值，got %d", got)
	}
}

func TestLookupModelContextWindow(t *testing.T) {
	if got := lookupModelContextWindow("totally-unknown"); got != 0 {
		t.Fatalf("未命中映射时应返回 0，got %d", got)
	}
}

func TestGetMaxOutputTokens(t *testing.T) {
	tests := []struct {
		name     string
		thinking bool
		explicit int
		want     int
	}{
		{name: "显式配置优先", thinking: true, explicit: 12345, want: 12345},
		{name: "关闭思考", thinking: false, want: 8192},
		{name: "开启思考", thinking: true, want: 64000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &ProviderConfig{Thinking: tt.thinking, MaxOutputTokens: tt.explicit}
			if got := p.GetMaxOutputTokens(); got != tt.want {
				t.Fatalf("thinking=%v 时输出预算为 %d，want %d", tt.thinking, got, tt.want)
			}
		})
	}
}
