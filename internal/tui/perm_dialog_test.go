package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"ccode/internal/agent"
	"ccode/internal/config"
)

func newPermDialogModel(t *testing.T) (Model, chan agent.PermissionReply) {
	t.Helper()
	provider := config.ProviderConfig{Name: "test", Protocol: "anthropic", Model: "m", APIKey: "k"}
	m := New([]config.ProviderConfig{provider}, nil, nil, nil)
	ch := make(chan agent.PermissionReply, 1)
	updated, _ := m.handleAgentEvent(agent.PermissionRequestEvent{ToolName: "Bash", Desc: "rm -rf build", ResponseCh: ch})
	return updated.(Model), ch
}

func pressKey(m Model, key tea.KeyMsg) Model {
	updated, _ := m.handlePermDialog(key)
	return updated.(Model)
}

func typeText(m Model, s string) Model {
	return pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func TestPermDialogDenyWithFeedback(t *testing.T) {
	m, ch := newPermDialogModel(t)
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyDown})
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyDown})
	m = typeText(m, "别删，移到 backupX")
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyBackspace}) // 删掉最后一个字符
	if !strings.Contains(m.renderPermDialog(), "别删，移到 backup█") {
		t.Errorf("dialog should echo the feedback being typed, got:\n%s", m.renderPermDialog())
	}
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})

	reply := <-ch
	if reply.Response != agent.PermDeny || reply.Feedback != "别删，移到 backup" {
		t.Errorf("reply = %+v, want deny with feedback", reply)
	}
	if m.permDialog || m.permFeedback != "" || m.permCursor != 0 {
		t.Error("dialog state should be reset after replying")
	}
}

func TestPermDialogTypingIgnoredOutsideNo(t *testing.T) {
	m, ch := newPermDialogModel(t)
	m = typeText(m, "hello")
	if m.permFeedback != "" {
		t.Errorf("typing on Yes must not collect feedback, got %q", m.permFeedback)
	}
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	reply := <-ch
	if reply.Response != agent.PermAllow || reply.Feedback != "" {
		t.Errorf("reply = %+v, want plain allow", reply)
	}
}

func TestPermDialogFeedbackDroppedWhenLeavingNo(t *testing.T) {
	m, ch := newPermDialogModel(t)
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyDown})
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyDown})
	m = typeText(m, "draft")
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyUp}) // 回到「Yes, and don't ask again」
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	reply := <-ch
	if reply.Response != agent.PermAllowAlways || reply.Feedback != "" {
		t.Errorf("reply = %+v, want allowAlways without feedback", reply)
	}
}

func TestPermDialogEscDenies(t *testing.T) {
	m, ch := newPermDialogModel(t)
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEsc})
	reply := <-ch
	if reply.Response != agent.PermDeny {
		t.Errorf("esc should deny, got %+v", reply)
	}
	if m.permDialog {
		t.Error("esc should close the dialog")
	}
}

func TestPermDialogPlaceholderOnlyOnNo(t *testing.T) {
	m, _ := newPermDialogModel(t)
	if strings.Contains(m.renderPermDialog(), "tell Ccode what to do differently") {
		t.Error("placeholder should be hidden while cursor is on Yes")
	}
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyDown})
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyDown})
	if !strings.Contains(m.renderPermDialog(), "tell Ccode what to do differently") {
		t.Error("placeholder should show when cursor is on No")
	}
}

func TestCtrlCClosesPermDialogWhileStreaming(t *testing.T) {
	m, _ := newPermDialogModel(t)
	cancelled := false
	m.streaming = true
	m.cancelStream = func() { cancelled = true }
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(Model)
	if !cancelled {
		t.Error("ctrl+c should cancel the running turn")
	}
	if m.permDialog || m.permRespCh != nil {
		t.Error("ctrl+c should close the permission dialog")
	}
}
