package tools

import "testing"

func TestOpenAIProtocol(t *testing.T) {
	if !isOpenAIProtocol("openai") {
		t.Fatal("openai 应使用 OpenAI 工具格式")
	}
	for _, protocol := range []string{"anthropic", "openai-compat", "OpenAI", "other"} {
		if isOpenAIProtocol(protocol) {
			t.Fatalf("协议 %q 不应使用 OpenAI 工具格式", protocol)
		}
	}
}
