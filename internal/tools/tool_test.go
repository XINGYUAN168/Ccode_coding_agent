package tools

import "testing"

func TestOpenAIProtocol(t *testing.T) {
	for _, protocol := range []string{"openai", "openai-compat"} {
		if !isOpenAIProtocol(protocol) {
			t.Fatalf("%s 应使用 OpenAI 工具格式", protocol)
		}
	}
	for _, protocol := range []string{"anthropic", "OpenAI", "other"} {
		if isOpenAIProtocol(protocol) {
			t.Fatalf("协议 %q 不应使用 OpenAI 工具格式", protocol)
		}
	}
}
