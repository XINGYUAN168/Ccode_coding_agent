package conversation

import (
	"strings"
	"testing"
)

// 指令、记忆、Skill 清单三样都跟着项目走，必须待在首条 system-reminder 里，
// 不能进系统提示词，否则每个项目各有一份系统提示词，跨项目缓存全部失效。
func TestInjectLongTermMemoryCarriesSkills(t *testing.T) {
	m := NewManager()
	m.AddUserMessage("hello")
	m.InjectLongTermMemory("my instructions", "my memories", "- /pdf: fill forms")

	msgs := m.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	// 注入的那条必须排在最前面，位置固定前缀才稳
	first := msgs[0]
	if first.Role != "user" {
		t.Errorf("injected message role = %q, want user", first.Role)
	}
	for _, want := range []string{"my instructions", "my memories", "- /pdf: fill forms"} {
		if !strings.Contains(first.Content, want) {
			t.Errorf("injected message missing %q:\n%s", want, first.Content)
		}
	}
	if !strings.HasPrefix(first.Content, "<system-reminder>") {
		t.Errorf("injected message should be wrapped in system-reminder, got:\n%s", first.Content)
	}
	if msgs[1].Content != "hello" {
		t.Errorf("original message should follow, got %q", msgs[1].Content)
	}
}

// 一次会话只注入一条，重复调用不叠加。
func TestInjectLongTermMemoryOnlyOnce(t *testing.T) {
	m := NewManager()
	m.InjectLongTermMemory("a", "b", "c")
	m.InjectLongTermMemory("a", "b", "c")

	if got := len(m.GetMessages()); got != 1 {
		t.Errorf("want 1 injected message, got %d", got)
	}
}

// 三样都为空时不产生噪音消息。
func TestInjectLongTermMemorySkipsWhenEmpty(t *testing.T) {
	m := NewManager()
	m.InjectLongTermMemory("", "", "")

	if got := len(m.GetMessages()); got != 0 {
		t.Errorf("want no message, got %d", got)
	}
}

// 只有 Skill 清单时也要注入，因为项目可能没写 CCODE.md 也没有记忆。
func TestInjectLongTermMemorySkillsOnly(t *testing.T) {
	m := NewManager()
	m.InjectLongTermMemory("", "", "- /review: review code")

	msgs := m.GetMessages()
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "- /review: review code") {
		t.Errorf("skill section missing:\n%s", msgs[0].Content)
	}
}
