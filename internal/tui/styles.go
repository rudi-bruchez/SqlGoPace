package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// sideBySideMin is the terminal width below which the two-panel rows stack vertically
// instead of sitting side by side.
const sideBySideMin = 90

// Palette. Colors adapt to the terminal's light/dark background (the mockup's two themes):
// accent is blue on light / green on dark, secondary a muted gray for lesser panels.
var (
	accentColor    = lipgloss.AdaptiveColor{Light: "#1c4fbf", Dark: "#39d353"}
	secondaryColor = lipgloss.AdaptiveColor{Light: "#7d7d7d", Dark: "#5a6b5a"}
	warnColor      = lipgloss.AdaptiveColor{Light: "#b45309", Dark: "#e8b339"}
	okColor        = lipgloss.AdaptiveColor{Light: "#15803d", Dark: "#39d353"}
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	selStyle   = lipgloss.NewStyle().Reverse(true)
	helpStyle  = lipgloss.NewStyle().Faint(true)
	alertStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9")) // bright red
	warnStyle  = lipgloss.NewStyle().Bold(true).Foreground(warnColor)
	okStyle    = lipgloss.NewStyle().Bold(true).Foreground(okColor)

	// basePanel is the constant border+padding shared by every panel; panelStyle only varies
	// its border color and width per box (lipgloss.Style is a value type, so this copies cheaply).
	basePanel = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

// boxChrome is subtracted from the terminal width to size a panel's inner Width. lipgloss
// counts padding inside Width and adds only the border (2 cols) outside it, so 2 would fill
// the terminal exactly; we use 4 to leave a 2-column safety margin, so a line whose visible
// width lipgloss under-counts (an ambiguous-width glyph like ⚠, or a wide ellipsis) can't push
// the box past the terminal edge and force a corrupting wrap. colGap is the gap between two
// side-by-side panels.
const (
	boxChrome = 4
	colGap    = 2
)

// panelStyle is a rounded-border box with one space of horizontal padding. contentWidth,
// when > 0, fixes the inner width (lipgloss draws the border and padding around it).
func panelStyle(color lipgloss.TerminalColor, contentWidth int) lipgloss.Style {
	s := basePanel.BorderForeground(color)
	if contentWidth > 0 {
		s = s.Width(contentWidth)
	}
	return s
}

// panel renders body inside a bordered box at the given inner width. A non-empty title is
// shown as a bold heading on the line above the box (the mockup's "operations" / "op N
// status" labels); panels whose heading belongs inside the box pass an empty title.
func panel(title, body string, color lipgloss.TerminalColor, contentWidth int) string {
	box := panelStyle(color, contentWidth).Render(body)
	if title == "" {
		return box
	}
	return titleStyle.Render(title) + "\n" + box
}

// joinRow places already-rendered panels side by side (with a two-column gap) when the
// terminal is at least sideBySideMin wide, and stacks them vertically otherwise.
func joinRow(width int, cols ...string) string {
	if width < sideBySideMin || len(cols) < 2 {
		return strings.Join(cols, "\n")
	}
	gap := strings.Repeat(" ", colGap)
	parts := make([]string, 0, len(cols)*2-1)
	for i, c := range cols {
		if i > 0 {
			parts = append(parts, gap)
		}
		parts = append(parts, c)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}
