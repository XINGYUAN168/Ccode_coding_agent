package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const configCanary = "sk-canary-SECRET-Config-123456789"

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validConfig(providerExtra string) string {
	return `
providers:
  - name: test
    protocol: openai
    base_url: https://example.test/v1/chat/completions?region=cn
    api_key: ` + configCanary + `
    model: test-model
    thinking: false
` + providerExtra + `
permission_mode: default
enable_fork: false
`
}

func TestProviderConfigShape(t *testing.T) {
	ty := reflect.TypeOf(ProviderConfig{})
	var yamlFields []string
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Tag.Get("yaml")
		if name != "" {
			yamlFields = append(yamlFields, name)
		}
	}
	want := []string{"name", "protocol", "base_url", "model", "api_key", "thinking"}
	if !reflect.DeepEqual(yamlFields, want) {
		t.Fatalf("Provider YAML 字段 = %v，want %v", yamlFields, want)
	}

	_, err := LoadConfig(writeConfig(t, validConfig("    seventh: rejected")))
	if err == nil || !strings.Contains(err.Error(), "seventh") {
		t.Fatalf("未知字段应被准确拒绝，got %v", err)
	}
}

func TestThinkingRequired(t *testing.T) {
	body := strings.Replace(validConfig(""), "    thinking: false\n", "", 1)
	_, err := LoadConfig(writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("缺少 thinking 应失败，got %v", err)
	}
}

func TestDuplicateAndUnknownProviderFields(t *testing.T) {
	t.Run("重复字段", func(t *testing.T) {
		body := strings.Replace(validConfig(""), "    model: test-model", "    model: test-model\n    model: duplicate", 1)
		_, err := LoadConfig(writeConfig(t, body))
		if err == nil {
			t.Fatal("重复字段应失败")
		}
	})
	t.Run("未知字段", func(t *testing.T) {
		_, err := LoadConfig(writeConfig(t, validConfig("    unknown_key: value")))
		if err == nil || !strings.Contains(err.Error(), "unknown_key") {
			t.Fatalf("未知字段应失败，got %v", err)
		}
	})
}

func TestBaseURLMatrix(t *testing.T) {
	valid := []string{"", "https://example.test/v1/messages", "http://localhost:8080/chat/completions?q=1"}
	for _, raw := range valid {
		if err := validateBaseURL(raw, 0); err != nil {
			t.Errorf("合法地址 %q 被拒绝: %v", raw, err)
		}
	}
	invalid := []string{"ftp://example.test/x", "/relative", "https://user:pass@example.test/x", "https://example.test/x#fragment"}
	for _, raw := range invalid {
		if err := validateBaseURL(raw, 0); err == nil {
			t.Errorf("非法地址 %q 未被拒绝", raw)
		}
	}
}

func TestLoadConfigErrorsNeverLeakCanary(t *testing.T) {
	cases := []string{
		"providers: []\nsecret: " + configCanary,
		"providers:\n  - name: broken\n    protocol: openai\n    api_key: " + configCanary + "\n    model: [\n",
		validConfig("") + "---\napi_key: " + configCanary + "\n",
	}
	for _, body := range cases {
		_, err := LoadConfig(writeConfig(t, body))
		if err == nil {
			t.Fatal("无效配置应失败")
		}
		if strings.Contains(err.Error(), configCanary) {
			t.Fatalf("错误泄漏密钥: %v", err)
		}
	}
}

func TestLoadConfigProtocolAndDuplicates(t *testing.T) {
	for _, protocol := range []string{ProtocolAnthropic, ProtocolOpenAI} {
		body := strings.Replace(validConfig(""), "protocol: openai", "protocol: "+protocol, 1)
		if _, err := LoadConfig(writeConfig(t, body)); err != nil {
			t.Fatalf("合法协议 %q 被拒绝: %v", protocol, err)
		}
	}
	for _, protocol := range []string{"openai-compat", "OpenAI", "other"} {
		body := strings.Replace(validConfig(""), "protocol: openai", "protocol: "+protocol, 1)
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Fatalf("非法协议 %q 未被拒绝", protocol)
		}
	}

	body := validConfig("") + strings.TrimPrefix(validConfig(""), "\nproviders:\n")
	_, err := LoadConfig(writeConfig(t, body))
	if err == nil {
		t.Fatal("重复 Provider 名称应失败")
	}
}

func TestLoadConfigKeepsApplicationSettings(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, validConfig("")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PermissionMode != "default" || cfg.ForkEnabled() {
		t.Fatalf("应用级配置未保留: %+v", cfg)
	}
}

func TestExplicitAPIKeyOnly(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "environment-secret")
	for _, value := range []string{`""`, `"   "`} {
		body := strings.Replace(validConfig(""), "    api_key: "+configCanary+"\n", "    api_key: "+value+"\n", 1)
		_, err := LoadConfig(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), "api_key") {
			t.Fatalf("环境变量不应补齐密钥 %s，got %v", value, err)
		}
	}
}

func TestExampleConfig(t *testing.T) {
	want := []ProviderConfig{
		{Name: "anthropic", Protocol: ProtocolAnthropic, BaseURL: "", APIKey: "anthropic-example-invalid", Model: "claude-sonnet-4-6", Thinking: true},
		{Name: "openai", Protocol: ProtocolOpenAI, BaseURL: "", APIKey: "openai-example-invalid", Model: "gpt-4.1", Thinking: false},
		{Name: "gemini", Protocol: ProtocolOpenAI, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", APIKey: "gemini-example-invalid", Model: "gemini-2.5-pro", Thinking: true},
		{Name: "kimi", Protocol: ProtocolOpenAI, BaseURL: "https://api.moonshot.cn/v1/chat/completions", APIKey: "kimi-example-invalid", Model: "kimi-k2.5", Thinking: true},
		{Name: "glm", Protocol: ProtocolOpenAI, BaseURL: "https://open.bigmodel.cn/api/paas/v4/chat/completions", APIKey: "glm-example-invalid", Model: "glm-4.5", Thinking: true},
		{Name: "deepseek", Protocol: ProtocolOpenAI, BaseURL: "https://api.deepseek.com/v1/chat/completions", APIKey: "deepseek-example-invalid", Model: "deepseek-chat", Thinking: false},
	}

	paths := []string{
		filepath.Join("..", "..", "config.example.yaml"),
		filepath.Join("..", "..", ".ccode", "config.yaml.example"),
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Providers, want) {
				t.Fatalf("示例 Provider = %#v，want %#v", cfg.Providers, want)
			}
		})
	}
}
