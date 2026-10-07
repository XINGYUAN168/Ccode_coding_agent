package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"ccode/internal/agent"
	"ccode/internal/agents"
	"ccode/internal/commands"
	"ccode/internal/config"
	"ccode/internal/tools"
)

// 用真实的 Esc 按键（bubbletea 里名字是 "esc"）驱动 Update，确认各个弹窗和菜单都能被 Esc 关掉

var escKey = tea.KeyMsg{Type: tea.KeyEsc}

func newChatModel(t *testing.T) Model {
	t.Helper()
	provider := config.ProviderConfig{Name: "test", Protocol: "anthropic", Model: "m", APIKey: "k"}
	m := New([]config.ProviderConfig{provider}, nil, nil, nil)
	m.state = stateChat
	return m
}

// asModel 统一取出 Model，部分处理函数返回的是 *Model
func asModel(t *testing.T, tm tea.Model) Model {
	t.Helper()
	switch v := tm.(type) {
	case Model:
		return v
	case *Model:
		return *v
	}
	t.Fatalf("unexpected model type %T", tm)
	return Model{}
}

func pressEsc(t *testing.T, m Model) Model {
	t.Helper()
	updated, _ := m.Update(escKey)
	return asModel(t, updated)
}

func TestEscKeyName(t *testing.T) {
	if escKey.String() != "esc" {
		t.Fatalf("bubbletea reports Esc as %q, handlers must match it", escKey.String())
	}
}

func TestEscClosesAtMenu(t *testing.T) {
	m := newChatModel(t)
	m.atMenuOpen = true
	m.atMatches = []string{"a.go"}
	m = pressEsc(t, m)
	if m.atMenuOpen || m.atMatches != nil {
		t.Error("esc should close the @ file menu")
	}
}

func TestEscClosesSlashMenu(t *testing.T) {
	m := newChatModel(t)
	m.slashMenuOpen = true
	m.slashMatches = []*commands.Command{{Name: "clear"}}
	m = pressEsc(t, m)
	if m.slashMenuOpen || m.slashMatches != nil {
		t.Error("esc should close the slash command menu")
	}
}

func TestEscClosesPlanApprovalDialog(t *testing.T) {
	m := newChatModel(t)
	m.planApprovalDialog = true
	m = pressEsc(t, m)
	if m.planApprovalDialog {
		t.Error("esc should close the plan approval dialog")
	}
}

func TestEscClosesSandboxDialog(t *testing.T) {
	m := newChatModel(t)
	m.sandboxDialog = true
	m = pressEsc(t, m)
	if m.sandboxDialog {
		t.Error("esc should close the sandbox dialog")
	}
}

func TestEscCancelsAskUserDialog(t *testing.T) {
	for _, onSubmit := range []bool{false, true} {
		m := newChatModel(t)
		ch := make(chan tools.QuestionResponse, 1)
		m.askUserDialog = true
		m.askUserQuestions = []tools.Question{{Text: "q?", Options: []tools.QuestionOption{{Label: "a"}}}}
		m.askUserCursors = []int{0}
		m.askUserSelected = []map[int]bool{{}}
		m.askUserOther = []string{""}
		m.askUserAnswered = map[int]string{}
		m.askUserOnSubmit = onSubmit
		m.askUserRespCh = ch
		m = pressEsc(t, m)
		if m.askUserDialog {
			t.Errorf("esc should close the ask-user dialog (onSubmit=%v)", onSubmit)
		}
		select {
		case resp := <-ch:
			if resp.Answers["_declined"] != "true" {
				t.Errorf("esc should decline, got %+v", resp.Answers)
			}
		default:
			t.Errorf("esc should answer the pending question (onSubmit=%v)", onSubmit)
		}
	}
}

func TestEscClosesRewindDialog(t *testing.T) {
	m := newChatModel(t)
	m.rewindDialog = true
	m.rewindPhase = 1
	m = pressEsc(t, m)
	if !m.rewindDialog || m.rewindPhase != 0 {
		t.Error("esc in phase 1 should go back to the snapshot list")
	}
	m = pressEsc(t, m)
	if m.rewindDialog {
		t.Error("esc in phase 0 should close the rewind dialog")
	}
}

func TestEscLeavesResumeList(t *testing.T) {
	m := newChatModel(t)
	m.state = stateResume
	m = pressEsc(t, m)
	if m.state != stateChat {
		t.Error("esc should leave the resume list")
	}
}

func TestEscMovesStreamingAgentToBackground(t *testing.T) {
	m := newChatModel(t)
	ch := make(chan agent.AgentEvent)
	close(ch)
	m.streaming = true
	m.agentCh = ch
	m.cancelStream = func() {}
	m.taskMgr = agents.NewTaskManager()
	m = pressEsc(t, m)
	if m.streaming || m.agentCh != nil {
		t.Error("esc during streaming should hand the agent to the background task manager")
	}
}
