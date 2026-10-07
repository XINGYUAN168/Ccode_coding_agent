package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	minInputLines        = 1
	maxInputLines        = 5
	minViewportLines     = 1
	fullBrandMinWidth    = 64
	minimumTerminalWidth = 12
	inputPromptText      = "❯ "
)

var compactLogoRows = [...]string{
	"  ____    ____    ___    ____    _____",
	` / ___|  / ___|  / _ \  |  _ \  | ____|`,
	`| |     | |     | | | | | | | | |  _|`,
	`| |___  | |___  | |_| | | |_| | | |___`,
	` \____|  \____|  \___/  |____/  |_____|`,
}

type frameLayout struct {
	Compact        bool
	ViewportHeight int
	OverlayHeight  int
	InputHeight    int
	StatusHeight   int
	TooSmall       bool
}

func calculateFrameLayout(width, height, inputLines, overlayHeight int) frameLayout {
	inputLines = max(minInputLines, min(maxInputLines, inputLines))
	overlayHeight = max(0, overlayHeight)

	layout := frameLayout{
		Compact:       width < fullBrandMinWidth,
		OverlayHeight: overlayHeight,
		InputHeight:   inputLines + inputBoxStyle.GetVerticalFrameSize(),
		StatusHeight:  1,
	}
	brandHeight := len(compactLogoRows)
	if layout.Compact {
		brandHeight = 1
	}

	reserved := brandHeight + layout.OverlayHeight + layout.InputHeight + layout.StatusHeight
	if width < minimumTerminalWidth || height < reserved+minViewportLines {
		layout.TooSmall = true
		return layout
	}
	layout.ViewportHeight = height - reserved
	return layout
}

func (m Model) renderBrand(compact bool) string {
	if compact {
		return bannerStyle.Render("CCODE")
	}

	metadata := [...]string{"Ccode v0.1.0", m.getModelName(), m.getWorkDir()}
	width := m.width
	if width <= 0 {
		width = 80
	}
	logoWidth := lipgloss.Width(compactLogoRows[len(compactLogoRows)-1])
	metadataWidth := max(1, width-logoWidth-2)
	lines := make([]string, 0, len(compactLogoRows))
	for i, row := range compactLogoRows {
		line := bannerStyle.Render(row)
		if i < len(metadata) {
			paddedRow := row + strings.Repeat(" ", max(0, logoWidth-lipgloss.Width(row)))
			meta := bannerDimStyle.Copy().MaxWidth(metadataWidth).Render(metadata[i])
			line = bannerStyle.Render(paddedRow) + "  " + meta
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func inputInnerWidth(width int) int {
	return max(1, width-inputBoxStyle.GetHorizontalFrameSize()-lipgloss.Width(inputPromptText))
}

func (m Model) renderInputBox() string {
	if m.planApprovalDialog {
		return inputBoxStyle.Render(lipgloss.NewStyle().Foreground(dimText).Render("Select an option above..."))
	}
	m.textarea.SetWidth(inputInnerWidth(m.width))
	line := lipgloss.JoinHorizontal(
		lipgloss.Top,
		inputPromptStyle.Render(inputPromptText),
		m.textarea.View(),
	)
	return inputBoxStyle.Render(line)
}

func (m Model) renderInteractionLayer() string {
	parts := make([]string, 0, 7)
	appendPart := func(part string) {
		part = strings.TrimRight(part, "\n")
		if part != "" {
			parts = append(parts, part)
		}
	}

	if m.permDialog {
		appendPart(m.renderPermDialog())
	}
	if m.planApprovalDialog {
		appendPart(m.renderPlanApprovalDialog())
	}
	if m.askUserDialog {
		appendPart(m.renderAskUserDialog())
	}
	if m.rewindDialog {
		appendPart(m.renderRewindDialog())
	}
	if m.sandboxDialog {
		appendPart(m.renderSandboxDialog())
	}
	if m.slashMenuOpen && len(m.slashMatches) > 0 {
		appendPart(m.renderSlashMenu())
	}
	if m.atMenuOpen && len(m.atMatches) > 0 {
		appendPart(m.renderAtMenu())
	}
	return strings.Join(parts, "\n")
}
