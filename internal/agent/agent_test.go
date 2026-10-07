package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccode/internal/conversation"
	"ccode/internal/llm"
	"ccode/internal/skills"
	"ccode/internal/tools"
)

// --- 模拟基础设施 ---

// mockClient 按顺序返回预设响应。
type mockClient struct {
	responses [][]llm.StreamEvent
	callIdx   int
}

func (m *mockClient) SetSystemPrompt(string) {}

func (m *mockClient) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	ch := make(chan llm.StreamEvent, 64)
	errCh := make(chan error, 1)
	go func() {
		defer close(ch)
		defer close(errCh)
		if m.callIdx >= len(m.responses) {
			ch <- llm.TextDelta{Text: "[mock: no more scripted responses]"}
			ch <- llm.StreamEnd{StopReason: "end_turn"}
			return
		}
		for _, ev := range m.responses[m.callIdx] {
			ch <- ev
		}
		m.callIdx++
	}()
	return ch, errCh
}

// dynamicMock 检查对话内容后决定如何响应。
// 每个处理器接收当前对话并返回事件。
// 处理器按顺序消费；一次 agent.Run() 调用内可能多次调用该模拟客户端，
// 即工具调用循环中的每个 LLM 轮次调用一次。
type dynamicMock struct {
	handlers []func(msgs []conversation.Message) []llm.StreamEvent
	callIdx  int
}

func (m *dynamicMock) SetSystemPrompt(string) {}

func (m *dynamicMock) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	ch := make(chan llm.StreamEvent, 64)
	errCh := make(chan error, 1)
	msgs := conv.GetMessages()
	go func() {
		defer close(ch)
		defer close(errCh)
		if m.callIdx >= len(m.handlers) {
			ch <- llm.TextDelta{Text: "[dynamic mock: no more handlers]"}
			ch <- llm.StreamEnd{StopReason: "end_turn"}
			return
		}
		events := m.handlers[m.callIdx](msgs)
		for _, ev := range events {
			ch <- ev
		}
		m.callIdx++
	}()
	return ch, errCh
}

// mockTool 返回固定结果。
type mockTool struct {
	name   string
	result string
}

func (t *mockTool) Name() string                 { return t.name }
func (t *mockTool) Description() string          { return "mock tool" }
func (t *mockTool) Category() tools.ToolCategory { return tools.CategoryRead }
func (t *mockTool) Schema() map[string]any {
	return map[string]any{
		"name": t.name, "description": "mock",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}
func (t *mockTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	return tools.ToolResult{Output: t.result}
}

// --- 辅助函数 ---

func collectEvents(ch <-chan AgentEvent) []AgentEvent {
	var events []AgentEvent
	for ev := range ch {
		if perm, ok := ev.(PermissionRequestEvent); ok {
			perm.ResponseCh <- PermissionReply{Response: PermAllow}
			continue
		}
		events = append(events, ev)
	}
	return events
}

func getStreamText(events []AgentEvent) string {
	var sb strings.Builder
	for _, ev := range events {
		if st, ok := ev.(StreamText); ok {
			sb.WriteString(st.Text)
		}
	}
	return sb.String()
}

func getToolResults(events []AgentEvent) []ToolResultEvent {
	var results []ToolResultEvent
	for _, ev := range events {
		if tr, ok := ev.(ToolResultEvent); ok {
			results = append(results, tr)
		}
	}
	return results
}

// buildSkillListing 生成 Skill 清单文本。它随首条 system-reminder 注入对话，
// 不进系统提示词（见 Agent.SkillSection）。
func buildSkillListing(skillsDir string, catalog *skills.Catalog) string {
	var sb strings.Builder
	sb.WriteString("## Available Skills\n\n")
	sb.WriteString(fmt.Sprintf("Skills are installed at: %s\n", skillsDir))
	sb.WriteString("When creating new skills, always place them under this directory as <skill-name>/SKILL.md.\n\n")
	sb.WriteString("The following skills are available. When the user invokes /<name>, follow that skill's instructions.\n\n")
	for _, meta := range catalog.List() {
		desc := meta.Description
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		sb.WriteString(fmt.Sprintf("- /%s: %s\n", meta.Name, desc))
	}
	return sb.String()
}

// runConversationRound 模拟 TUI 处理一条用户消息的流程：把用户消息加入对话，
// 调用 agent.Run()、收集事件并返回文本。
func runConversationRound(ag *Agent, conv *conversation.Manager, userMsg string) (string, []AgentEvent) {
	conv.AddUserMessage(userMsg)
	events := collectEvents(ag.Run(context.Background(), conv))
	return getStreamText(events), events
}

// --- Agent 基础测试 ---

func TestAgentSimpleResponse(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{{
		llm.TextDelta{Text: "Hello, "},
		llm.TextDelta{Text: "world!"},
		llm.StreamEnd{StopReason: "end_turn"},
	}}}
	ag := New(client, tools.NewRegistry(), "anthropic")
	conv := conversation.NewManager()
	text, events := runConversationRound(ag, conv, "hi")
	if text != "Hello, world!" {
		t.Errorf("got %q, want %q", text, "Hello, world!")
	}
	hasComplete := false
	for _, ev := range events {
		if _, ok := ev.(LoopComplete); ok {
			hasComplete = true
		}
	}
	if !hasComplete {
		t.Error("missing LoopComplete event")
	}
}

func TestAgentThinkingResponse(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{{
		llm.ThinkingDelta{Text: "第一段"},
		llm.ThinkingDelta{Text: "第二段"},
		llm.ThinkingComplete{Thinking: "第一段第二段", Signature: "sig-1"},
		llm.ThinkingComplete{Thinking: "补充", Signature: "sig-2"},
		llm.TextDelta{Text: "答案"},
		llm.StreamEnd{StopReason: "end_turn"},
	}}}
	ag := New(client, tools.NewRegistry(), "anthropic")
	conv := conversation.NewManager()
	_, events := runConversationRound(ag, conv, "问题")

	var thinking strings.Builder
	for _, event := range events {
		if value, ok := event.(ThinkingText); ok {
			thinking.WriteString(value.Text)
		}
	}
	if thinking.String() != "第一段第二段" {
		t.Fatalf("上层思考增量 = %q", thinking.String())
	}
	messages := conv.GetMessages()
	assistant := messages[len(messages)-1]
	if assistant.Content != "答案" || len(assistant.ThinkingBlocks) != 2 || assistant.ThinkingBlocks[0].Signature != "sig-1" || assistant.ThinkingBlocks[1].Thinking != "补充" {
		t.Fatalf("助手历史 = %+v", assistant)
	}
}

func TestAgentThinkingWithToolContinuation(t *testing.T) {
	client := &dynamicMock{handlers: []func([]conversation.Message) []llm.StreamEvent{
		func([]conversation.Message) []llm.StreamEvent {
			return []llm.StreamEvent{
				llm.ThinkingDelta{Text: "查找"},
				llm.ThinkingComplete{Thinking: "查找", Signature: "sig-tool"},
				llm.ToolCallStart{ToolName: "ReadFile", ToolID: "call-1"},
				llm.ToolCallComplete{ToolID: "call-1", ToolName: "ReadFile", Arguments: map[string]any{"path": "a.go"}},
				llm.StreamEnd{StopReason: "tool_use"},
			}
		},
		func(messages []conversation.Message) []llm.StreamEvent {
			if len(messages) < 3 {
				t.Fatalf("工具续接历史过短: %+v", messages)
			}
			assistant := messages[len(messages)-2]
			if len(assistant.ThinkingBlocks) != 1 || assistant.ThinkingBlocks[0].Signature != "sig-tool" || len(assistant.ToolUses) != 1 {
				t.Fatalf("思考和工具未写入同一助手消息: %+v", assistant)
			}
			return []llm.StreamEvent{llm.TextDelta{Text: "完成"}, llm.StreamEnd{StopReason: "end_turn"}}
		},
	}}
	registry := tools.NewRegistry()
	registry.Register(&mockTool{name: "ReadFile", result: "内容"})
	ag := New(client, registry, "anthropic")
	conv := conversation.NewManager()
	text, _ := runConversationRound(ag, conv, "读取")
	if text != "完成" {
		t.Fatalf("最终正文 = %q", text)
	}
}

func TestAgentMaxTokensPreservesThinking(t *testing.T) {
	client := &dynamicMock{handlers: []func([]conversation.Message) []llm.StreamEvent{
		func([]conversation.Message) []llm.StreamEvent {
			return []llm.StreamEvent{
				llm.ThinkingDelta{Text: "未完成思考"},
				llm.ThinkingComplete{Thinking: "未完成思考", Signature: "sig-cut"},
				llm.StreamEnd{StopReason: "max_tokens"},
			}
		},
		func(messages []conversation.Message) []llm.StreamEvent {
			if len(messages) < 3 {
				t.Fatalf("截断恢复历史过短: %+v", messages)
			}
			assistant := messages[len(messages)-2]
			if len(assistant.ThinkingBlocks) != 1 || assistant.ThinkingBlocks[0].Thinking != "未完成思考" {
				t.Fatalf("截断恢复丢失思考: %+v", assistant)
			}
			return []llm.StreamEvent{llm.TextDelta{Text: "继续完成"}, llm.StreamEnd{StopReason: "end_turn"}}
		},
	}}
	ag := New(client, tools.NewRegistry(), "anthropic")
	conv := conversation.NewManager()
	text, _ := runConversationRound(ag, conv, "长任务")
	if text != "继续完成" {
		t.Fatalf("恢复正文 = %q", text)
	}
}

func TestAgentToolCallLoop(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{
		{
			llm.TextDelta{Text: "Let me read that."},
			llm.ToolCallStart{ToolName: "ReadFile", ToolID: "t1"},
			llm.ToolCallComplete{ToolID: "t1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": "/tmp/x"}},
			llm.StreamEnd{StopReason: "tool_use"},
		},
		{
			llm.TextDelta{Text: "File says: hello from mock"},
			llm.StreamEnd{StopReason: "end_turn"},
		},
	}}
	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "ReadFile", result: "hello from mock"})
	ag := New(client, reg, "anthropic")
	conv := conversation.NewManager()
	text, events := runConversationRound(ag, conv, "read it")
	if !strings.Contains(text, "hello from mock") {
		t.Errorf("response %q should mention tool output", text)
	}
	trs := getToolResults(events)
	if len(trs) != 1 || trs[0].Output != "hello from mock" {
		t.Errorf("unexpected tool results: %+v", trs)
	}
}

func TestAgentMaxIterations(t *testing.T) {
	loop := []llm.StreamEvent{
		llm.ToolCallStart{ToolName: "Glob", ToolID: "t"},
		llm.ToolCallComplete{ToolID: "t", ToolName: "Glob", Arguments: map[string]any{"pattern": "*"}},
		llm.StreamEnd{StopReason: "tool_use"},
	}
	responses := make([][]llm.StreamEvent, 10)
	for i := range responses {
		responses[i] = loop
	}
	client := &mockClient{responses: responses}
	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "Glob", result: "f.txt"})
	ag := New(client, reg, "anthropic")
	ag.MaxIterations = 3
	conv := conversation.NewManager()
	_, events := runConversationRound(ag, conv, "loop")
	found := false
	for _, ev := range events {
		if e, ok := ev.(ErrorEvent); ok && strings.Contains(e.Message, "maximum iterations") {
			found = true
		}
	}
	if !found {
		t.Error("expected max iterations error")
	}
}

func TestAgentStoresOnlyFinalText(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{{
		llm.TextDelta{Text: "My answer."},
		llm.StreamEnd{StopReason: "end_turn"},
	}}}
	ag := New(client, tools.NewRegistry(), "anthropic")
	conv := conversation.NewManager()
	runConversationRound(ag, conv, "think")
	msgs := conv.GetMessages()
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || last.Content != "My answer." {
		t.Fatalf("助手消息未只保存最终正文: %+v", last)
	}
}

// --- 多轮对话测试 ---
// 这些测试模拟真实 TUI 流程：LLM 返回不含工具调用的文本时 agent.Run() 结束，
// 随后用户发送新消息并再次调用 agent.Run()。真实对话即按此方式运行。

func TestMultiRoundConversation(t *testing.T) {
	// 模拟：用户提问 → Agent 请求澄清 → 用户回答 → Agent 执行 → 完成。
	client := &dynamicMock{handlers: []func([]conversation.Message) []llm.StreamEvent{
		// 第 1 轮：Agent 提出澄清问题。
		func(msgs []conversation.Message) []llm.StreamEvent {
			last := msgs[len(msgs)-1]
			if !strings.Contains(last.Content, "refactor") {
				t.Errorf("round 1: expected user msg about refactor, got %q", last.Content)
			}
			return []llm.StreamEvent{
				llm.TextDelta{Text: "Which file do you want me to refactor? And what style — extract functions, simplify conditionals, or both?"},
				llm.StreamEnd{StopReason: "end_turn"},
			}
		},
		// 第 2 轮：Agent 读取文件。
		func(msgs []conversation.Message) []llm.StreamEvent {
			last := msgs[len(msgs)-1]
			if !strings.Contains(last.Content, "main.go") {
				t.Errorf("round 2: expected user msg about main.go, got %q", last.Content)
			}
			return []llm.StreamEvent{
				llm.TextDelta{Text: "I'll read main.go first."},
				llm.ToolCallStart{ToolName: "ReadFile", ToolID: "r1"},
				llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": "main.go"}},
				llm.StreamEnd{StopReason: "tool_use"},
			}
		},
		// 第 2 轮继续（取得工具结果后）：Agent 生成最终回答。
		func(msgs []conversation.Message) []llm.StreamEvent {
			// 验证工具结果已进入对话。
			found := false
			for _, m := range msgs {
				for _, tr := range m.ToolResults {
					if strings.Contains(tr.Content, "func main") {
						found = true
					}
				}
			}
			if !found {
				t.Error("round 2 continued: tool result not found in conversation")
			}
			return []llm.StreamEvent{
				llm.TextDelta{Text: "Here's the refactored version:\n```go\nfunc main() {\n    run()\n}\n```\nI extracted the logic into a `run()` function."},
				llm.StreamEnd{StopReason: "end_turn"},
			}
		},
	}}

	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "ReadFile", result: "package main\n\nfunc main() {\n    // lots of code\n    fmt.Println(\"hello\")\n}"})

	ag := New(client, reg, "anthropic")
	conv := conversation.NewManager()

	// 第 1 轮：用户提问，Agent 请求澄清。
	text1, _ := runConversationRound(ag, conv, "please refactor this code")
	if !strings.Contains(text1, "Which file") {
		t.Errorf("round 1: agent should ask clarification, got: %s", text1)
	}
	t.Logf("Round 1 agent: %s", text1)

	// 第 2 轮：用户回答，Agent 读取文件并重构。
	text2, events2 := runConversationRound(ag, conv, "main.go, extract functions please")
	if !strings.Contains(text2, "refactored") {
		t.Errorf("round 2: agent should produce refactored code, got: %s", text2)
	}
	t.Logf("Round 2 agent: %s", text2)

	// 验证工具已被调用。
	trs := getToolResults(events2)
	if len(trs) != 1 || trs[0].ToolName != "ReadFile" {
		t.Errorf("expected ReadFile tool call, got: %+v", trs)
	}

	// 验证完整对话结构正确。
	msgs := conv.GetMessages()
	t.Logf("Total messages in conversation: %d", len(msgs))
	// 预期顺序：user1、assistant1、user2、assistant2+tool、tool_result、assistant3。
	if len(msgs) < 6 {
		t.Fatalf("expected 6+ messages, got %d", len(msgs))
	}
	if msgs[0].Content != "please refactor this code" {
		t.Error("msg[0] should be first user message")
	}
	if msgs[2].Content != "main.go, extract functions please" {
		t.Error("msg[2] should be second user message")
	}
}

// --- 使用完整 Agent 模拟的 Skill 集成测试 ---

func TestFrontendDesignSkillFullSession(t *testing.T) {
	// 模拟：用户调用 /frontend-design → Agent 询问要构建什么 →
	// 用户回答“登录页” → Agent 创建文件 → 验证文件存在。

	workDir := t.TempDir()

	// 加载真实 frontend-design Skill，找不到时创建测试版本。
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "frontend-design")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: frontend-design
description: Create distinctive, production-grade frontend interfaces
---

# Frontend Design Skill

Create high-quality frontend code. Follow these principles:
1. Semantic HTML structure
2. Modern CSS with custom properties
3. Responsive design
4. Accessibility (ARIA labels, focus states)

Output files directly using WriteFile.
`), 0o644)

	catalog, _ := skills.LoadFromDirectory(dir)
	skill := catalog.Get("frontend-design")
	skillBody := skill.PromptBody

	outputFile := filepath.Join(workDir, "login.html")

	client := &dynamicMock{handlers: []func([]conversation.Message) []llm.StreamEvent{
		// 第 1 轮：Agent 收到 Skill 提示词并询问要构建什么。
		func(msgs []conversation.Message) []llm.StreamEvent {
			last := msgs[len(msgs)-1]
			// 验证 Skill 提示词已正确注入。
			if !strings.Contains(last.Content, "Frontend Design Skill") {
				t.Errorf("skill body not in user message: %s", last.Content[:min(100, len(last.Content))])
			}
			return []llm.StreamEvent{
				llm.TextDelta{Text: "I'd love to help you build a frontend! What kind of page or component do you need? For example:\n- Login page\n- Dashboard\n- Landing page"},
				llm.StreamEnd{StopReason: "end_turn"},
			}
		},
		// 第 2 轮：用户要求登录页，Agent 创建文件。
		func(msgs []conversation.Message) []llm.StreamEvent {
			last := msgs[len(msgs)-1]
			if !strings.Contains(last.Content, "login") {
				t.Errorf("expected user to say login, got: %s", last.Content)
			}
			return []llm.StreamEvent{
				llm.TextDelta{Text: "I'll create a login page with email/password fields."},
				llm.ToolCallStart{ToolName: "WriteFile", ToolID: "w1"},
				llm.ToolCallComplete{ToolID: "w1", ToolName: "WriteFile", Arguments: map[string]any{
					"file_path": outputFile,
					"content": `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Login</title>
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body { font-family: system-ui; display: flex; justify-content: center; align-items: center; min-height: 100vh; background: #f0f2f5; }
    .login-card { background: white; padding: 2rem; border-radius: 12px; box-shadow: 0 2px 10px rgba(0,0,0,0.1); width: 100%; max-width: 400px; }
    h1 { margin-bottom: 1.5rem; color: #1a1a2e; }
    label { display: block; margin-bottom: 0.5rem; font-weight: 500; }
    input { width: 100%; padding: 0.75rem; border: 1px solid #ddd; border-radius: 8px; margin-bottom: 1rem; }
    button { width: 100%; padding: 0.75rem; background: #4361ee; color: white; border: none; border-radius: 8px; cursor: pointer; font-size: 1rem; }
    button:hover { background: #3a56d4; }
  </style>
</head>
<body>
  <div class="login-card">
    <h1>Sign In</h1>
    <form>
      <label for="email">Email</label>
      <input type="email" id="email" name="email" placeholder="you@example.com" required aria-label="Email address">
      <label for="password">Password</label>
      <input type="password" id="password" name="password" placeholder="Your password" required aria-label="Password">
      <button type="submit">Log In</button>
    </form>
  </div>
</body>
</html>`,
				}},
				llm.StreamEnd{StopReason: "tool_use"},
			}
		},
		// 第 2 轮继续：写入文件后，Agent 给出确认。
		func(msgs []conversation.Message) []llm.StreamEvent {
			return []llm.StreamEvent{
				llm.TextDelta{Text: fmt.Sprintf("Done! I created `%s` with:\n- Clean semantic HTML\n- Responsive card layout\n- Accessible form fields with ARIA labels\n- Modern CSS with custom properties\n\nOpen it in your browser to see the result.", outputFile)},
				llm.StreamEnd{StopReason: "end_turn"},
			}
		},
	}}

	// 使用真实 WriteFile 工具，以验证文件确实被创建。
	reg := tools.NewRegistry()
	reg.Register(&tools.WriteFileTool{})

	ag := New(client, reg, "anthropic")
	conv := conversation.NewManager()

	// 第 1 轮：调用 Skill，等同于用户在 TUI 输入 /frontend-design。
	skillPrompt := skillBody
	text1, _ := runConversationRound(ag, conv, skillPrompt)
	t.Logf("Agent (round 1): %s", text1)

	if !strings.Contains(text1, "What kind of page") {
		t.Errorf("agent should ask what to build, got: %s", text1)
	}

	// 第 2 轮：用户回答，Agent 创建文件。
	text2, events2 := runConversationRound(ag, conv, "a login page with email and password")
	t.Logf("Agent (round 2): %s", text2)

	if !strings.Contains(text2, "login") && !strings.Contains(text2, "Login") {
		t.Errorf("agent should confirm login page creation, got: %s", text2)
	}

	// 验证 WriteFile 已被调用。
	trs := getToolResults(events2)
	writeCount := 0
	for _, tr := range trs {
		if tr.ToolName == "WriteFile" {
			writeCount++
			if tr.IsError {
				t.Errorf("WriteFile failed: %s", tr.Output)
			}
		}
	}
	if writeCount != 1 {
		t.Errorf("expected 1 WriteFile call, got %d", writeCount)
	}

	// 关键检查：验证文件确实存在于磁盘且内容正确。
	content, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	html := string(content)

	checks := []struct {
		substr string
		desc   string
	}{
		{"<!DOCTYPE html>", "valid HTML doctype"},
		{"<form", "has a form element"},
		{"type=\"email\"", "has email input"},
		{"type=\"password\"", "has password input"},
		{"<button", "has submit button"},
		{"aria-label", "has accessibility attributes"},
		{"border-radius", "has modern CSS styling"},
		{"max-width", "has responsive layout"},
	}
	for _, c := range checks {
		if !strings.Contains(html, c.substr) {
			t.Errorf("output file missing %s (looking for %q)", c.desc, c.substr)
		}
	}
	t.Logf("Output file: %s (%d bytes, all %d checks passed)", outputFile, len(content), len(checks))

	// 验证对话历史连贯。
	msgs := conv.GetMessages()
	t.Logf("Conversation: %d messages", len(msgs))
	for i, m := range msgs {
		summary := m.Content
		if len(summary) > 80 {
			summary = summary[:80] + "..."
		}
		if len(m.ToolUses) > 0 {
			summary += fmt.Sprintf(" [+%d tool calls]", len(m.ToolUses))
		}
		if len(m.ToolResults) > 0 {
			summary += fmt.Sprintf(" [+%d tool results]", len(m.ToolResults))
		}
		t.Logf("  [%d] %s: %s", i, m.Role, summary)
	}
}

func TestSkillCreatorOutputsToCorrectDirectory(t *testing.T) {
	// 模拟：用户调用 /skill-creator → 提供细节 → Agent 创建新 Skill →
	// 验证它位于 .ccode/skills/，而不是项目根目录。

	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".ccode", "skills")
	os.MkdirAll(skillsDir, 0o755)

	// 准备 skill-creator Skill。
	creatorDir := filepath.Join(skillsDir, "skill-creator")
	os.MkdirAll(creatorDir, 0o755)
	os.WriteFile(filepath.Join(creatorDir, "SKILL.md"), []byte(`---
name: skill-creator
description: Create new skills
---

# Skill Creator

New skills MUST be created under the .ccode/skills/ directory.
The full path should be .ccode/skills/<skill-name>/SKILL.md.
`), 0o644)

	catalog, _ := skills.LoadFromDirectory(skillsDir)
	skillListing := buildSkillListing(skillsDir, catalog)
	skill := catalog.Get("skill-creator")

	newSkillDir := filepath.Join(skillsDir, "git-helper")
	newSkillFile := filepath.Join(newSkillDir, "SKILL.md")

	client := &dynamicMock{handlers: []func([]conversation.Message) []llm.StreamEvent{
		// 第 1 轮：Agent 询问 Skill 应执行什么任务。
		func(msgs []conversation.Message) []llm.StreamEvent {
			return []llm.StreamEvent{
				llm.TextDelta{Text: "I'll help you create a new skill! What should this skill do? What would you like to name it?"},
				llm.StreamEnd{StopReason: "end_turn"},
			}
		},
		// 第 2 轮：用户给出描述，Agent 在 .ccode/skills/ 中创建 Skill。
		func(msgs []conversation.Message) []llm.StreamEvent {
			last := msgs[len(msgs)-1]
			if !strings.Contains(last.Content, "git") {
				t.Errorf("expected user to mention git, got: %s", last.Content)
			}
			// Agent 先创建目录，再写入 SKILL.md。
			return []llm.StreamEvent{
				llm.TextDelta{Text: "I'll create a git-helper skill for you."},
				llm.ToolCallStart{ToolName: "Bash", ToolID: "b1"},
				llm.ToolCallComplete{ToolID: "b1", ToolName: "Bash", Arguments: map[string]any{
					// 在工作目录里用相对路径建目录：Windows 上 bash 可能是 WSL，
					// 它不认 C:/ 这种盘符路径，写绝对路径会在当前目录下建出名为 C: 的目录
					"command": "mkdir -p .ccode/skills/git-helper",
				}},
				llm.StreamEnd{StopReason: "tool_use"},
			}
		},
		// 创建目录后写入 SKILL.md。
		func(msgs []conversation.Message) []llm.StreamEvent {
			return []llm.StreamEvent{
				llm.ToolCallStart{ToolName: "WriteFile", ToolID: "w1"},
				llm.ToolCallComplete{ToolID: "w1", ToolName: "WriteFile", Arguments: map[string]any{
					"file_path": newSkillFile,
					"content": `---
name: git-helper
description: Help with common git operations like branching, rebasing, and resolving conflicts
---

# Git Helper Skill

Help the user with git operations:
1. Create and manage branches
2. Interactive rebase guidance
3. Merge conflict resolution
4. Commit message best practices
`,
				}},
				llm.StreamEnd{StopReason: "tool_use"},
			}
		},
		// 最终确认。
		func(msgs []conversation.Message) []llm.StreamEvent {
			return []llm.StreamEvent{
				llm.TextDelta{Text: fmt.Sprintf("Done! Created git-helper skill at `%s`.\n\nYou can now use it with `/git-helper`.", newSkillFile)},
				llm.StreamEnd{StopReason: "end_turn"},
			}
		},
	}}

	reg := tools.NewRegistry()
	reg.Register(&tools.WriteFileTool{})
	reg.Register(&tools.BashTool{WorkDir: workDir})

	ag := New(client, reg, "anthropic")
	conv := conversation.NewManager()

	// 验证系统提示词已告诉 Agent 应把 Skill 放在哪里。
	if !strings.Contains(skillListing, skillsDir) {
		t.Fatalf("system prompt missing skills dir: %s", skillsDir)
	}

	// 第 1 轮：调用 skill-creator。
	text1, _ := runConversationRound(ag, conv, skill.PromptBody+"\n\n## User Request\n\ncreate a new skill")
	t.Logf("Agent (round 1): %s", text1)
	if !strings.Contains(text1, "skill") {
		t.Errorf("agent should ask about the skill, got: %s", text1)
	}

	// 第 2 轮：用户描述 Skill。
	text2, _ := runConversationRound(ag, conv, "a git helper that helps with branching, rebasing and conflicts")
	t.Logf("Agent (round 2): %s", text2)

	// 关键检查：新 Skill 创建在 .ccode/skills/ 内，而不是项目根目录。
	if _, err := os.Stat(newSkillFile); os.IsNotExist(err) {
		t.Fatalf("skill file not created at expected path: %s", newSkillFile)
	}

	// 验证 Skill 系统可以加载新 Skill。
	updatedCatalog, err := skills.LoadFromDirectory(skillsDir)
	if err != nil {
		t.Fatalf("failed to reload skills: %v", err)
	}

	gitHelper := updatedCatalog.Get("git-helper")
	if gitHelper == nil {
		t.Fatal("git-helper skill not found after creation")
	}
	if !strings.Contains(gitHelper.PromptBody, "rebase") {
		t.Error("git-helper body should mention rebase")
	}
	if !strings.Contains(gitHelper.Meta.Description, "git") {
		t.Error("git-helper description should mention git")
	}

	// 验证没有在项目根目录创建文件。
	rootSkillFile := filepath.Join(workDir, "git-helper", "SKILL.md")
	if _, err := os.Stat(rootSkillFile); err == nil {
		t.Errorf("skill was INCORRECTLY created at project root: %s", rootSkillFile)
	}

	t.Logf("New skill loaded successfully: name=%s, body=%d chars", gitHelper.Meta.Name, len(gitHelper.PromptBody))

	// 验证现在共有两个 Skill。
	allSkills := updatedCatalog.List()
	if len(allSkills) != 2 {
		t.Errorf("expected 2 skills (skill-creator + git-helper), got %d", len(allSkills))
	}
}

func TestSkillMultiRoundWithToolChain(t *testing.T) {
	// 模拟真实的 Skill 会话：Agent 读取现有代码、请求用户确认，再写入修改后的代码。
	// 测试流程：Skill 提示词 → 读取 → 询问用户 → 用户确认 → 写入 → 验证。

	workDir := t.TempDir()

	// 创建一个供 Agent 读取的现有文件。
	srcFile := filepath.Join(workDir, "app.js")
	os.WriteFile(srcFile, []byte(`const express = require('express');
const app = express();
app.get('/', (req, res) => res.send('Hello'));
app.listen(3000);
`), 0o644)

	outputFile := filepath.Join(workDir, "app.js")

	client := &dynamicMock{handlers: []func([]conversation.Message) []llm.StreamEvent{
		// 第 1 轮：Agent 先读取文件。
		func(msgs []conversation.Message) []llm.StreamEvent {
			return []llm.StreamEvent{
				llm.TextDelta{Text: "Let me check the current code."},
				llm.ToolCallStart{ToolName: "ReadFile", ToolID: "r1"},
				llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": srcFile}},
				llm.StreamEnd{StopReason: "tool_use"},
			}
		},
		// 读取后，Agent 提出修改方案。
		func(msgs []conversation.Message) []llm.StreamEvent {
			// 验证 Agent 确实取得了文件内容。
			for _, m := range msgs {
				for _, tr := range m.ToolResults {
					if !strings.Contains(tr.Content, "express") {
						t.Errorf("tool result should contain express, got: %s", tr.Content)
					}
				}
			}
			return []llm.StreamEvent{
				llm.TextDelta{Text: "I see you have a basic Express app. I'll add:\n- Error handling middleware\n- CORS support\n- Health check endpoint\n\nShall I proceed?"},
				llm.StreamEnd{StopReason: "end_turn"},
			}
		},
		// 第 2 轮：用户确认，Agent 写入文件。
		func(msgs []conversation.Message) []llm.StreamEvent {
			return []llm.StreamEvent{
				llm.TextDelta{Text: "Updating the file with improvements."},
				llm.ToolCallStart{ToolName: "WriteFile", ToolID: "w1"},
				llm.ToolCallComplete{ToolID: "w1", ToolName: "WriteFile", Arguments: map[string]any{
					"file_path": outputFile,
					"content": `const express = require('express');
const cors = require('cors');
const app = express();

app.use(cors());
app.use(express.json());

app.get('/health', (req, res) => res.json({ status: 'ok' }));
app.get('/', (req, res) => res.send('Hello'));

app.use((err, req, res, next) => {
  console.error(err.stack);
  res.status(500).json({ error: 'Internal server error' });
});

app.listen(3000);
`,
				}},
				llm.StreamEnd{StopReason: "tool_use"},
			}
		},
		// 最终回复。
		func(msgs []conversation.Message) []llm.StreamEvent {
			return []llm.StreamEvent{
				llm.TextDelta{Text: "Updated! Added CORS, JSON parsing, health check, and error handling."},
				llm.StreamEnd{StopReason: "end_turn"},
			}
		},
	}}

	reg := tools.NewRegistry()
	reg.Register(&tools.ReadFileTool{})
	reg.Register(&tools.WriteFileTool{})

	ag := New(client, reg, "anthropic")
	conv := conversation.NewManager()

	// 第 1 轮：用户要求改进代码。
	text1, _ := runConversationRound(ag, conv, fmt.Sprintf("improve the express app at %s", srcFile))
	t.Logf("Agent (round 1): %s", text1)
	if !strings.Contains(text1, "proceed") {
		t.Errorf("agent should ask for confirmation, got: %s", text1)
	}

	// 第 2 轮：用户确认。
	text2, _ := runConversationRound(ag, conv, "yes, go ahead")
	t.Logf("Agent (round 2): %s", text2)

	// 验证文件确实已修改。
	content, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("failed to read output: %v", err)
	}
	updated := string(content)

	checks := []struct {
		substr string
		desc   string
	}{
		{"cors", "CORS middleware added"},
		{"express.json()", "JSON body parser added"},
		{"/health", "health check endpoint added"},
		{"err, req, res, next", "error handling middleware added"},
		{"app.listen(3000)", "original listen preserved"},
		{"res.send('Hello')", "original route preserved"},
	}
	for _, c := range checks {
		if !strings.Contains(updated, c.substr) {
			t.Errorf("output missing %s (looking for %q)", c.desc, c.substr)
		}
	}

	// 验证对话往返流程完整。
	msgs := conv.GetMessages()
	t.Logf("Conversation: %d messages", len(msgs))
	userMsgCount := 0
	assistantMsgCount := 0
	for _, m := range msgs {
		if m.Role == "user" && m.Content != "" {
			userMsgCount++
		}
		if m.Role == "assistant" {
			assistantMsgCount++
		}
	}
	if userMsgCount < 2 {
		t.Errorf("expected at least 2 user messages, got %d", userMsgCount)
	}
	if assistantMsgCount < 2 {
		t.Errorf("expected at least 2 assistant messages, got %d", assistantMsgCount)
	}
	t.Logf("All %d content checks passed on output file (%d bytes)", len(checks), len(content))
}

func TestRealSkillsLoadAndRunSimulation(t *testing.T) {
	// 加载实际安装的 Skill，并验证端到端模拟可以运行。
	wd, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(wd, ".ccode", "skills")); err == nil {
			break
		}
		// 父目录等于自身说明到了根目录（Windows 上是盘符根，不是 "/"）
		parent := filepath.Dir(wd)
		if parent == wd {
			break
		}
		wd = parent
	}
	skillsDir := filepath.Join(wd, ".ccode", "skills")
	if _, err := os.Stat(skillsDir); os.IsNotExist(err) {
		t.Skip("No .ccode/skills directory")
	}

	catalog, _ := skills.LoadFromDirectory(skillsDir)
	metas := catalog.List()
	t.Logf("Loaded %d real skills", len(metas))

	skillListing := buildSkillListing(skillsDir, catalog)

	// 验证系统提示词结构。
	if !strings.Contains(skillListing, "Skills are installed at:") {
		t.Error("system prompt missing skill path")
	}
	for _, meta := range metas {
		if !strings.Contains(skillListing, "/"+meta.Name) {
			t.Errorf("system prompt missing /%s", meta.Name)
		}
	}

	// 测试每个真实 Skill 均可加载并用作提示词。
	for _, meta := range metas {
		skill := catalog.Get(meta.Name)
		if skill == nil {
			t.Errorf("skill %q returned nil from Get", meta.Name)
			continue
		}
		if skill.PromptBody == "" {
			t.Errorf("skill %q has empty body", meta.Name)
			continue
		}

		// 模拟携带参数调用 Skill。
		prompt := skill.PromptBody + "\n\n## User Request\n\ntest request for " + meta.Name
		if !strings.Contains(prompt, "## User Request") {
			t.Errorf("skill %q prompt missing user request section", meta.Name)
		}

		// 使用模拟客户端运行一轮，并验证它收到了 Skill 提示词。
		client := &dynamicMock{handlers: []func([]conversation.Message) []llm.StreamEvent{
			func(msgs []conversation.Message) []llm.StreamEvent {
				lastUser := msgs[len(msgs)-1]
				if !strings.Contains(lastUser.Content, meta.Name) && !strings.Contains(lastUser.Content, "test request") {
					t.Errorf("skill %q: agent did not receive skill prompt", meta.Name)
				}
				return []llm.StreamEvent{
					llm.TextDelta{Text: fmt.Sprintf("I received the %s skill prompt and I'm ready to help.", meta.Name)},
					llm.StreamEnd{StopReason: "end_turn"},
				}
			},
		}}

		ag := New(client, tools.NewRegistry(), "anthropic")
		conv := conversation.NewManager()
		text, _ := runConversationRound(ag, conv, prompt)

		if !strings.Contains(text, meta.Name) {
			t.Errorf("skill %q: mock response should echo skill name, got: %s", meta.Name, text)
		}
		t.Logf("  /%s: prompt=%d chars, response OK", meta.Name, len(prompt))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestAgentOnLoopCompleteFiresOnFinalTurn(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{{
		llm.TextDelta{Text: "done"},
		llm.StreamEnd{StopReason: "end_turn"},
	}}}
	ag := New(client, tools.NewRegistry(), "anthropic")

	got := make(chan *conversation.Manager, 1)
	ag.OnLoopComplete = func(conv *conversation.Manager) {
		got <- conv
	}

	conv := conversation.NewManager()
	runConversationRound(ag, conv, "hi")

	select {
	case received := <-got:
		if received != conv {
			t.Error("callback should receive the same conv pointer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnLoopComplete was not called within 2s")
	}
}

func TestAgentOnLoopCompleteSkippedOnError(t *testing.T) {
	// 在 LoopComplete 前达到 MaxIterations，回调不得触发。
	loop := []llm.StreamEvent{
		llm.ToolCallStart{ToolName: "Glob", ToolID: "t"},
		llm.ToolCallComplete{ToolID: "t", ToolName: "Glob", Arguments: map[string]any{"pattern": "*"}},
		llm.StreamEnd{StopReason: "tool_use"},
	}
	responses := make([][]llm.StreamEvent, 5)
	for i := range responses {
		responses[i] = loop
	}
	client := &mockClient{responses: responses}
	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "Glob", result: "f.txt"})
	ag := New(client, reg, "anthropic")
	ag.MaxIterations = 2

	called := make(chan struct{}, 1)
	ag.OnLoopComplete = func(*conversation.Manager) { called <- struct{}{} }

	conv := conversation.NewManager()
	runConversationRound(ag, conv, "spin")

	select {
	case <-called:
		t.Error("OnLoopComplete must not fire when loop exits via error")
	case <-time.After(200 * time.Millisecond):
		// 预期不触发回调。
	}
}

func TestFilterSchemasByName(t *testing.T) {
	schemas := []map[string]any{
		{"name": "Agent", "x": 1},
		{"name": "Bash", "x": 2},
		{"name": "ReadFile", "x": 3},
	}
	allow := func(name string) bool { return name == "Agent" || name == "ReadFile" }
	got := filterSchemasByName(schemas, allow)
	if len(got) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(got))
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s["name"].(string)] = true
	}
	if !names["Agent"] || !names["ReadFile"] || names["Bash"] {
		t.Errorf("filter kept the wrong set: %v", names)
	}
}

func TestFilterSchemasByNameEmptyInput(t *testing.T) {
	got := filterSchemasByName(nil, func(string) bool { return true })
	if len(got) != 0 {
		t.Errorf("expected empty result, got %d entries", len(got))
	}
}

func schemaNames(schemas []map[string]any) []string {
	var out []string
	for _, s := range schemas {
		if n, ok := s["name"].(string); ok {
			out = append(out, n)
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
