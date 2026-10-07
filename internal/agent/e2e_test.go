//go:build e2e

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccode/internal/config"
	"ccode/internal/conversation"
	"ccode/internal/llmfactory"
	"ccode/internal/permissions"
	"ccode/internal/tools"
)

// TestE2E_DeepSeekMultiTurnTools 使用真实兼容服务验证读取、写入、再次读取和最终回复。
func TestE2E_DeepSeekMultiTurnTools(t *testing.T) {
	apiKey := os.Getenv("CCODE_TEST_API_KEY")
	endpoint := os.Getenv("CCODE_TEST_BASE_URL")
	model := os.Getenv("CCODE_TEST_MODEL")
	if apiKey == "" || endpoint == "" || model == "" {
		t.Skip("缺少 CCODE_TEST_API_KEY、CCODE_TEST_BASE_URL 或 CCODE_TEST_MODEL，跳过真实 E2E")
	}

	workDir := t.TempDir()
	sourcePath := filepath.Join(workDir, "source.txt")
	targetPath := filepath.Join(workDir, "result.txt")
	if err := os.WriteFile(sourcePath, []byte("把目标文件写成唯一一行：CCODE_E2E_WRITE_OK\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := config.ProviderConfig{
		Name: "deepseek-e2e", Protocol: config.ProtocolOpenAI,
		BaseURL: endpoint, APIKey: apiKey, Model: model, Thinking: true,
	}
	client, err := llmfactory.NewFactory(llmfactory.Options{}).New(provider, "你正在执行自动化工具测试。必须严格遵守用户给出的工具顺序。")
	if err != nil {
		t.Fatalf("创建真实模型客户端失败: %v", err)
	}

	cache := tools.NewFileStateCache()
	registry := tools.NewRegistry()
	registry.Register(&tools.ReadFileTool{FileStateCache: cache})
	registry.Register(&tools.WriteFileTool{FileStateCache: cache})

	agent := New(client, registry, config.ProtocolOpenAI)
	agent.WorkDir = workDir
	agent.MaxIterations = 10
	agent.ContextWindow = provider.GetContextWindow()
	agent.MaxOutputTokens = provider.GetMaxOutputTokens()
	agent.Checker = permissions.NewChecker(
		permissions.NewPathSandbox(workDir),
		permissions.NewRuleEngine(workDir),
		permissions.ModeBypass,
	)

	conversationManager := conversation.NewManager()
	conversationManager.AddUserMessage("请严格依次完成：先用 ReadFile 读取 " + sourcePath + "；再按文件指示用 WriteFile 写入 " + targetPath + "；然后用 ReadFile 再次读取 " + targetPath + " 验证；最后用一句话报告成功。不要跳过任何一步。")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	var completedTools []string
	var finalText strings.Builder
	var agentError string
	for event := range agent.Run(ctx, conversationManager) {
		switch value := event.(type) {
		case ToolUseEvent:
			if value.Args != nil {
				completedTools = append(completedTools, value.ToolName)
			}
		case StreamText:
			finalText.WriteString(value.Text)
		case ErrorEvent:
			agentError = value.Message
		}
	}
	if agentError != "" {
		t.Fatalf("真实模型执行失败: %s", agentError)
	}
	if len(completedTools) < 3 || completedTools[0] != "ReadFile" || completedTools[1] != "WriteFile" || completedTools[2] != "ReadFile" {
		t.Fatalf("工具顺序 = %v，want ReadFile → WriteFile → ReadFile", completedTools)
	}
	written, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("读取目标文件失败: %v", err)
	}
	if strings.TrimSpace(string(written)) != "CCODE_E2E_WRITE_OK" {
		t.Fatalf("目标文件内容不正确: %q", strings.TrimSpace(string(written)))
	}
	if strings.TrimSpace(finalText.String()) == "" {
		t.Fatal("真实模型未返回最终正文")
	}
}
