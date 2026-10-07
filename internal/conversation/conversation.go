package conversation

import (
	"strings"
	"time"
)

type ToolUseBlock struct {
	ToolUseID string
	ToolName  string
	Arguments map[string]any
}

type ToolResultBlock struct {
	ToolUseID string
	Content   string
	IsError   bool
	// ContentBlocks 用于把工具结果发成结构化 content block 而不是纯文本。
	// 目前只有官方端点下的 ToolSearch 会用：它回 tool_reference 块，由服务端
	// 把 schema 展开进上下文。填了它时 Content 仍保留等价文本，token 估算和
	// TUI 展示都走 Content。
	ContentBlocks []map[string]any
}

type ThinkingBlock struct {
	Thinking  string
	Signature string
}

type Message struct {
	Role           string
	Content        string
	ThinkingBlocks []ThinkingBlock
	ToolUses       []ToolUseBlock
	ToolResults    []ToolResultBlock
}

type Manager struct {
	history        []Message
	ltmInjected    bool
	baselineTokens int
	anchorCount    int
	hasUsage       bool
}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) AddUserMessage(content string) {
	m.history = append(m.history, Message{Role: "user", Content: content})
}

func (m *Manager) AddAssistantMessage(content string) {
	m.history = append(m.history, Message{Role: "assistant", Content: content})
}

func (m *Manager) AddToolUseMessage(text, toolUseID, toolName string, arguments map[string]any) {
	m.history = append(m.history, Message{
		Role:    "assistant",
		Content: text,
		ToolUses: []ToolUseBlock{{
			ToolUseID: toolUseID,
			ToolName:  toolName,
			Arguments: arguments,
		}},
	})
}

func (m *Manager) AddAssistantMessageWithTools(text string, toolUses []ToolUseBlock) {
	m.history = append(m.history, Message{
		Role:     "assistant",
		Content:  text,
		ToolUses: toolUses,
	})
}

func (m *Manager) AddAssistantFull(text string, thinking []ThinkingBlock, toolUses []ToolUseBlock) {
	m.history = append(m.history, Message{
		Role:           "assistant",
		Content:        text,
		ThinkingBlocks: thinking,
		ToolUses:       toolUses,
	})
}

func (m *Manager) AddToolResultMessage(toolUseID, content string, isError bool) {
	m.history = append(m.history, Message{
		Role: "user",
		ToolResults: []ToolResultBlock{{
			ToolUseID: toolUseID,
			Content:   content,
			IsError:   isError,
		}},
	})
}

func (m *Manager) AddToolResultsMessage(results []ToolResultBlock) {
	m.history = append(m.history, Message{
		Role:        "user",
		ToolResults: results,
	})
}

func (m *Manager) AddSystemReminder(content string) {
	m.history = append(m.history, Message{
		Role:    "user",
		Content: "<system-reminder>\n" + content + "\n</system-reminder>",
	})
}

// HasReminderContaining 报告历史里还有没有包含 marker 的提醒。
//
// 用来判断一条「只需要说一次」的提醒是否已经在上下文里。compact 会把历史压成
// 摘要，原来那条提醒随之消失，这时候必须重发，否则模型再也看不到。调用方拿这个
// 结果决定重发，就不用在 compact 那边额外挂钩子。
func (m *Manager) HasReminderContaining(marker string) bool {
	for _, msg := range m.history {
		if msg.Role == "user" && strings.Contains(msg.Content, marker) {
			return true
		}
	}
	return false
}

func (m *Manager) InjectLongTermMemory(instructions, memories, skills string) {
	if m.ltmInjected {
		return
	}
	var sections []string
	if instructions != "" {
		sections = append(sections, "# ccodeMd\nCodebase and user instructions are shown below. Be sure to adhere to these instructions. IMPORTANT: These instructions OVERRIDE any default behavior and you MUST follow them exactly as written.\n\n"+instructions)
	}
	if memories != "" {
		sections = append(sections, "# autoMemory\n"+memories)
	}
	// Skill 清单跟着项目走，放系统提示词会让每个项目各有一份、跨项目缓存全失效，
	// 所以和指令、记忆一样放在这条消息里。
	if skills != "" {
		sections = append(sections, "# availableSkills\n"+skills)
	}
	if len(sections) == 0 {
		return
	}
	sections = append(sections, "# currentDate\nToday's date is "+time.Now().Format("2006-01-02")+".")
	body := strings.Join(sections, "\n\n")
	wrapped := "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n" +
		body +
		"\n\n      IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.\n</system-reminder>"
	m.history = append([]Message{{Role: "user", Content: wrapped}}, m.history...)
	m.ltmInjected = true
}

// AppendMessages 把给定消息复制到历史末尾，供压缩流程在摘要之后原样重放近期尾部消息。
func (m *Manager) AppendMessages(messages []Message) {
	m.history = append(m.history, messages...)
}

func (m *Manager) Len() int {
	return len(m.history)
}

func (m *Manager) TruncateTo(index int) {
	if index < 0 {
		index = 0
	}
	if index > len(m.history) {
		return
	}
	m.history = m.history[:index]
}

func (m *Manager) GetMessages() []Message {
	result := make([]Message, len(m.history))
	for i, msg := range m.history {
		result[i] = cloneMessage(msg)
	}
	return result
}

// cloneMessage 复制消息及其可变成员，避免调用方修改内部历史。
func cloneMessage(msg Message) Message {
	cloned := msg
	cloned.ThinkingBlocks = append([]ThinkingBlock(nil), msg.ThinkingBlocks...)
	cloned.ToolUses = make([]ToolUseBlock, len(msg.ToolUses))
	for i, toolUse := range msg.ToolUses {
		cloned.ToolUses[i] = toolUse
		cloned.ToolUses[i].Arguments = cloneMap(toolUse.Arguments)
	}
	cloned.ToolResults = make([]ToolResultBlock, len(msg.ToolResults))
	for i, result := range msg.ToolResults {
		cloned.ToolResults[i] = result
		cloned.ToolResults[i].ContentBlocks = make([]map[string]any, len(result.ContentBlocks))
		for j, block := range result.ContentBlocks {
			cloned.ToolResults[i].ContentBlocks[j] = cloneMap(block)
		}
	}
	return cloned
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneValue(value)
	}
	return cloned
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for i, item := range typed {
			cloned[i] = cloneValue(item)
		}
		return cloned
	default:
		return value
	}
}

// ReplaceToolResults 就地替换指定消息的 ToolResults 列表。
// 用于 Layer 1 tool-result budget 的 Design A 实现：直接修改原始
// 对话历史，而非生成新的副本。msgIndex 越界时静默忽略。
func (m *Manager) ReplaceToolResults(msgIndex int, newResults []ToolResultBlock) {
	if msgIndex < 0 || msgIndex >= len(m.history) {
		return
	}
	m.history[msgIndex].ToolResults = newResults
}

// RecordUsageAnchor 锚定本轮 API 返回的真实 token 用量。
// 调用时机：assistant 消息已追加到 history 之后。
func (m *Manager) RecordUsageAnchor(input, output, cacheRead, cacheCreation int) {
	baseline := input + cacheRead + cacheCreation + output
	if baseline <= 0 {
		return
	}
	m.baselineTokens = baseline
	m.anchorCount = len(m.history)
	m.hasUsage = true
}

// ClearUsageAnchor 压缩后清零锚点，下次估算回退到全量字符估算。
func (m *Manager) ClearUsageAnchor() {
	m.baselineTokens = 0
	m.anchorCount = 0
	m.hasUsage = false
}

// UsageAnchorState 返回当前锚点状态，供 compact 层读取。
func (m *Manager) UsageAnchorState() (baselineTokens, anchorCount int, hasUsage bool) {
	return m.baselineTokens, m.anchorCount, m.hasUsage
}
