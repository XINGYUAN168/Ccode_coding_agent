package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"ccode/internal/commands"
	"ccode/internal/config"
)

var ansiControlSequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func stripANSI(value string) string {
	return ansiControlSequence.ReplaceAllString(value, "")
}

func TestCompactLogoRows(t *testing.T) {
	wantRows := []string{
		"  ____    ____    ___    ____    _____",
		` / ___|  / ___|  / _ \  |  _ \  | ____|`,
		`| |     | |     | | | | | | | | |  _|`,
		`| |___  | |___  | |_| | | |_| | | |___`,
		` \____|  \____|  \___/  |____/  |_____|`,
	}
	wantWidths := []int{38, 39, 37, 38, 39}
	if len(compactLogoRows) != len(wantWidths) {
		t.Fatalf("Logo 行数 = %d，want %d", len(compactLogoRows), len(wantWidths))
	}
	for i, row := range compactLogoRows {
		if row != wantRows[i] {
			t.Errorf("第 %d 行 = %q，want %q", i+1, row, wantRows[i])
		}
		if got := lipgloss.Width(row); got != wantWidths[i] {
			t.Errorf("第 %d 行宽度 = %d，want %d：%q", i+1, got, wantWidths[i], row)
		}
		if strings.ContainsAny(row, "█▓▒") {
			t.Errorf("第 %d 行含块字符：%q", i+1, row)
		}
	}
	joined := strings.Join(compactLogoRows[:], "\n")
	for _, stroke := range []string{"/ ___|", `/ _ \`, `| |_| |`, `|____/`, "|_____"} {
		if !strings.Contains(joined, stroke) {
			t.Errorf("Logo 缺少可辨识笔画 %q：\n%s", stroke, joined)
		}
	}
}

func TestBrandIsYellowAndCompactFallback(t *testing.T) {
	provider := config.ProviderConfig{Name: "deepseek", Model: "deepseek-chat"}
	m := Model{width: 80, providers: []config.ProviderConfig{provider}, selectedProvider: &provider}
	if got := bannerStyle.GetForeground(); got != yellowText {
		t.Fatalf("Logo 颜色 = %v，want %v", got, yellowText)
	}
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	coloredBrand := m.renderBrand(false)
	lipgloss.SetColorProfile(profile)
	if !strings.Contains(coloredBrand, "38;5;214") {
		t.Fatalf("Logo 渲染结果缺少黄色 ANSI 前景色：%q", coloredBrand)
	}
	brand := stripANSI(m.renderBrand(false))
	lines := strings.Split(brand, "\n")
	if len(lines) != 5 || !strings.Contains(brand, "Ccode v0.1.0") {
		t.Fatalf("普通 Logo 结构错误：\n%s", brand)
	}
	metadata := []string{"Ccode v0.1.0", "deepseek-chat", m.getWorkDir()}
	for i, want := range metadata {
		if !strings.Contains(lines[i], want) {
			t.Errorf("Logo 第 %d 行缺少元数据 %q：%q", i+1, want, lines[i])
		}
		if got, wantColumn := strings.Index(lines[i], want), 41; got != wantColumn {
			t.Errorf("Logo 第 %d 行元数据起始列 = %d，want %d：%q", i+1, got, wantColumn, lines[i])
		}
	}
	if strings.Contains(lines[3], "Ccode v0.1.0") || strings.Contains(lines[3], "deepseek-chat") ||
		strings.Contains(lines[4], "Ccode v0.1.0") || strings.Contains(lines[4], "deepseek-chat") {
		t.Fatalf("Logo 后两行不应包含元数据：\n%s", brand)
	}
	if got := strings.TrimSpace(stripANSI(m.renderBrand(true))); got != "CCODE" {
		t.Fatalf("紧凑 Logo = %q，want CCODE", got)
	}
}

func TestCalculateFrameLayout(t *testing.T) {
	tests := []struct {
		name           string
		width, height  int
		input, overlay int
		compact        bool
		tooSmall       bool
	}{
		{name: "normal", width: 80, height: 24, input: 1},
		{name: "five line", width: 80, height: 24, input: 5, overlay: 4},
		{name: "full brand threshold", width: 64, height: 16, input: 1},
		{name: "below full brand threshold", width: 63, height: 16, input: 1, compact: true},
		{name: "compact", width: 40, height: 16, input: 1, compact: true},
		{name: "too narrow", width: 8, height: 24, input: 1, compact: true, tooSmall: true},
		{name: "too short", width: 80, height: 4, input: 1, tooSmall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateFrameLayout(tt.width, tt.height, tt.input, tt.overlay)
			if got.Compact != tt.compact || got.TooSmall != tt.tooSmall {
				t.Fatalf("layout = %+v，want compact=%v tooSmall=%v", got, tt.compact, tt.tooSmall)
			}
			for name, value := range map[string]int{
				"viewport": got.ViewportHeight,
				"overlay":  got.OverlayHeight,
				"input":    got.InputHeight,
				"status":   got.StatusHeight,
			} {
				if value < 0 {
					t.Errorf("%s 高度为负数：%d", name, value)
				}
			}
			if !got.TooSmall {
				brandHeight := len(compactLogoRows)
				if got.Compact {
					brandHeight = 1
				}
				total := brandHeight + got.ViewportHeight + got.OverlayHeight + got.InputHeight + got.StatusHeight
				if total != tt.height {
					t.Errorf("布局总高 = %d，want %d：%+v", total, tt.height, got)
				}
			}
		})
	}
}

func TestInputTextInsideBorder(t *testing.T) {
	m := New(nil, nil, nil, nil)
	m.width = 40
	m.textarea.Focus()
	m.textarea.SetValue("hello 中文 🙂")
	box := stripANSI(m.renderInputBox())
	lines := strings.Split(box, "\n")
	if len(lines) != 3 {
		t.Fatalf("单行输入框高度 = %d，want 3：\n%s", len(lines), box)
	}
	if !strings.HasPrefix(lines[0], "╭") || !strings.HasSuffix(lines[0], "╮") ||
		!strings.HasPrefix(lines[2], "╰") || !strings.HasSuffix(lines[2], "╯") {
		t.Fatalf("输入框边界不完整：\n%s", box)
	}
	if !strings.HasPrefix(lines[1], "│") || !strings.HasSuffix(lines[1], "│") {
		t.Fatalf("输入内容不在左右边界内：%q", lines[1])
	}
	if !strings.Contains(lines[1], "❯ hello 中文 🙂") {
		t.Fatalf("输入内容缺失或位于边框外：%q", lines[1])
	}
	if strings.Count(box, "❯") != 1 {
		t.Fatalf("提示符数量 = %d，want 1：\n%s", strings.Count(box, "❯"), box)
	}
}

func TestInputBoxStyle(t *testing.T) {
	if inputPromptStyle.GetForeground() != yellowText {
		t.Fatalf("输入提示符颜色 = %v，want %v", inputPromptStyle.GetForeground(), yellowText)
	}
	border := inputBoxStyle.GetBorderStyle()
	if border.TopLeft != "╭" || border.TopRight != "╮" || border.BottomLeft != "╰" || border.BottomRight != "╯" {
		t.Fatalf("输入框不是完整圆角边框：%+v", border)
	}
}

func TestUnicodeInputInsideBorder(t *testing.T) {
	m := New(nil, nil, nil, nil)
	m.width = 28
	m.textarea.Focus()
	m.textarea.SetValue("你好🙂\n第二行")
	m.resizeTextarea()

	box := stripANSI(m.renderInputBox())
	lines := strings.Split(box, "\n")
	if len(lines) != 4 {
		t.Fatalf("两行输入框高度 = %d，want 4：\n%s", len(lines), box)
	}
	for i, line := range lines[1 : len(lines)-1] {
		if !strings.HasPrefix(line, "│") || !strings.HasSuffix(line, "│") {
			t.Errorf("第 %d 行文字越出左右边界：%q", i+1, line)
		}
	}
	if !strings.Contains(box, "你好🙂") || !strings.Contains(box, "第二行") {
		t.Fatalf("Unicode 多行内容缺失：\n%s", box)
	}
	if strings.Count(box, "❯") != 1 {
		t.Fatalf("提示符数量 = %d，want 1：\n%s", strings.Count(box, "❯"), box)
	}
}

func TestTextareaHeight(t *testing.T) {
	m := New(nil, nil, nil, nil)
	m.width = 40
	for lines := 1; lines <= 7; lines++ {
		m.textarea.SetValue(strings.Repeat("一行\n", lines-1) + "一行")
		m.resizeTextarea()
		want := min(lines, maxInputLines)
		if got := m.textarea.Height(); got != want {
			t.Errorf("%d 行输入的可见高度 = %d，want %d", lines, got, want)
		}
	}

	m.textarea.Reset()
	m.resizeTextarea()
	if got := m.textarea.Height(); got != minInputLines {
		t.Fatalf("清空后的可见高度 = %d，want %d", got, minInputLines)
	}
}

func TestUnicodeWrap(t *testing.T) {
	m := New(nil, nil, nil, nil)
	m.width = 20
	inner := inputInnerWidth(m.width)
	m.textarea.SetValue(strings.Repeat("界", inner/2+1))
	m.resizeTextarea()
	if got := m.textarea.Height(); got != 2 {
		t.Fatalf("宽字符软换行高度 = %d，want 2（内部宽度 %d）", got, inner)
	}
}

func TestBottomAnchoredLayout(t *testing.T) {
	for _, count := range []int{0, 1, 30} {
		t.Run(fmt.Sprintf("history_%d", count), func(t *testing.T) {
			m := readyChatModel(t, 80, 24)
			for i := 0; i < count; i++ {
				m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: fmt.Sprintf("消息 %d", i)})
			}
			m.updateViewport()
			assertInputBottomAnchor(t, m)
		})
	}
}

func TestHistoryScrollAndTailFollow(t *testing.T) {
	m := readyChatModel(t, 40, 12)
	for i := 0; i < 30; i++ {
		m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: fmt.Sprintf("历史消息 %02d", i)})
	}
	m.updateViewport()
	if !m.viewport.AtBottom() {
		t.Fatal("初始长历史没有跟随到底部")
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = updated.(Model)
	if !m.userScrolled || m.viewport.AtBottom() {
		t.Fatal("Page Up 后没有进入用户滚动状态")
	}
	offset := m.viewport.YOffset
	m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: "后台新增消息"})
	m.updateViewport()
	if m.viewport.YOffset != offset || m.viewport.AtBottom() {
		t.Fatal("用户主动上滚后，新消息错误地把 viewport 拉回底部")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = updated.(Model)
	if m.userScrolled || !m.viewport.AtBottom() {
		t.Fatal("End 后没有恢复尾部跟随")
	}
	m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: "继续新增消息"})
	m.updateViewport()
	if !m.viewport.AtBottom() {
		t.Fatal("恢复跟随后，新消息没有保持在底部")
	}
}

func TestInputBottomAnchor(t *testing.T) {
	m := readyChatModel(t, 80, 24)
	wantBottom := inputBorderBottomRow(stripANSI(m.renderChatView()))
	for lines := 1; lines <= 7; lines++ {
		m.textarea.SetValue(strings.Repeat("输入\n", lines-1) + "输入")
		m.resizeTextarea()
		view := stripANSI(m.renderChatView())
		if got := inputBorderBottomRow(view); got != wantBottom {
			t.Errorf("%d 行输入时底边行号 = %d，want %d", lines, got, wantBottom)
		}
		if got := lipgloss.Height(view); got != m.height {
			t.Errorf("%d 行输入时总高度 = %d，want %d", lines, got, m.height)
		}
	}
}

func TestOverlayAboveInput(t *testing.T) {
	m := readyChatModel(t, 80, 24)
	m.slashMenuOpen = true
	m.slashMatches = []*commands.Command{m.cmdRegistry.Find("help")}
	m.updateViewport()

	view := stripANSI(m.renderChatView())
	lines := strings.Split(view, "\n")
	menuRow, inputRow := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "/help") {
			menuRow = i
		}
		if strings.Contains(line, "╭") {
			inputRow = i
		}
	}
	if menuRow < 0 || inputRow < 0 || menuRow >= inputRow {
		t.Fatalf("交互层没有位于输入框上方：menu=%d input=%d\n%s", menuRow, inputRow, view)
	}
	assertInputBottomAnchor(t, m)
}

func TestResponsiveLayout(t *testing.T) {
	m := readyChatModel(t, 40, 16)
	m.textarea.SetValue("保留输入")
	m.resizeTextarea()
	compact := stripANSI(m.renderChatView())
	compactFirstLine := strings.TrimSpace(strings.SplitN(compact, "\n", 2)[0])
	if compactFirstLine != "CCODE" || !strings.Contains(compact, "保留输入") {
		t.Fatalf("窄窗口未使用紧凑品牌或丢失输入：\n%s", compact)
	}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 8, Height: 8})
	m = updated.(Model)
	tooSmall := stripANSI(m.renderChatView())
	compactWarning := strings.Join(strings.Fields(tooSmall), "")
	if !strings.Contains(compactWarning, "CCODE") || !strings.Contains(compactWarning, "终端窗口太小") {
		t.Fatalf("过小窗口没有明确回退：\n%s", tooSmall)
	}

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	restored := stripANSI(m.renderChatView())
	if !strings.Contains(restored, compactLogoRows[0]) || !strings.Contains(restored, "保留输入") {
		t.Fatalf("恢复窗口后品牌或输入丢失：\n%s", restored)
	}
}

func readyChatModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := New(nil, nil, nil, nil)
	m.state = stateChat
	m.textarea.Focus()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return updated.(Model)
}

func assertInputBottomAnchor(t *testing.T, m Model) {
	t.Helper()
	view := stripANSI(m.renderChatView())
	if got := lipgloss.Height(view); got != m.height {
		t.Fatalf("视图总高度 = %d，want %d：\n%s", got, m.height, view)
	}
	if got, want := inputBorderBottomRow(view), m.height-2; got != want {
		t.Fatalf("输入框底边行号 = %d，want %d：\n%s", got, want, view)
	}
}

func inputBorderBottomRow(view string) int {
	for i, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "╰") {
			return i
		}
	}
	return -1
}
