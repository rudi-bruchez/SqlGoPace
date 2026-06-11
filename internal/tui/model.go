// Package tui is the incident console (--tui): a Bubble Tea model that shows live
// progress and blocked sessions, and turns operator key presses into action
// intents on a channel. It consumes the same monitoring data as silent mode; the
// host wires the action channel to the executor.
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Status is the lifecycle state of the running operation.
type Status int

const (
	// StatusRunning means the DDL is executing.
	StatusRunning Status = iota
	// StatusPaused means a resumable operation is paused.
	StatusPaused
	// StatusCancelling means a cancel/kill is in progress.
	StatusCancelling
	// StatusDone means the operation finished.
	StatusDone
)

// String returns the status label.
func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "RUNNING"
	case StatusPaused:
		return "PAUSED"
	case StatusCancelling:
		return "CANCELLING"
	case StatusDone:
		return "DONE"
	default:
		return "UNKNOWN"
	}
}

// Blocker is one session blocked by the running DDL.
type Blocker struct {
	SPID     int
	Login    string
	Host     string
	Program  string
	WaitType string
	WaitMS   int64
	Query    string
}

// WaitCategory is one category of waits accumulated by the running DDL.
type WaitCategory struct {
	Name   string
	WaitMS int64
	Tasks  int64
}

// Messages the host feeds from the monitor stream.
type (
	// ProgressMsg carries progress for the running operation.
	ProgressMsg struct {
		Percent         float64
		ETASeconds      int64
		RollbackPercent float64
	}
	// BlockersMsg carries the current set of blocked sessions.
	BlockersMsg struct{ Blockers []Blocker }
	// WaitsMsg carries the running DDL's wait categories (what is slowing it down).
	WaitsMsg struct {
		Categories []WaitCategory
		TotalMS    int64
	}
	// StatusMsg updates the lifecycle status (and optionally the operation label).
	StatusMsg struct {
		Status    Status
		Operation string
	}
	// LogMsg appends a narration line.
	LogMsg struct{ Line string }
)

// ActionKind is an operator intent emitted by key presses.
type ActionKind int

const (
	// ActionKillBlocker kills the selected blocking session.
	ActionKillBlocker ActionKind = iota
	// ActionKillDDL kills the running DDL.
	ActionKillDDL
	// ActionPause pauses a resumable operation.
	ActionPause
	// ActionExtend extends the blocking wait timer.
	ActionExtend
	// ActionSnapshot writes the current state to the log.
	ActionSnapshot
	// ActionQuit leaves the console.
	ActionQuit
)

// Action is an operator intent, dispatched to the host via the action channel.
type Action struct {
	Kind ActionKind
	SPID int // set for ActionKillBlocker
}

// Model is the incident console state.
type Model struct {
	operation       string
	resumable       bool
	status          Status
	percent         float64
	etaSeconds      int64
	rollbackPercent float64
	blockers        []Blocker
	waits           []WaitCategory
	waitTotalMS     int64
	cursor          int
	actions         chan<- Action
	quitting        bool
}

// New returns a console model for the given operation. actions may be nil (no
// dispatch); resumable enables the pause action.
func New(operation string, resumable bool, actions chan<- Action) Model {
	return Model{operation: operation, resumable: resumable, status: StatusRunning, actions: actions}
}

// Init implements tea.Model; the host drives updates, so there is no initial cmd.
func (m Model) Init() tea.Cmd { return nil }

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ProgressMsg:
		m.percent = msg.Percent
		m.etaSeconds = msg.ETASeconds
		m.rollbackPercent = msg.RollbackPercent
	case BlockersMsg:
		m.blockers = msg.Blockers
		if m.cursor >= len(m.blockers) {
			m.cursor = max(0, len(m.blockers)-1)
		}
	case WaitsMsg:
		m.waits = msg.Categories
		m.waitTotalMS = msg.TotalMS
	case StatusMsg:
		m.status = msg.Status
		if msg.Operation != "" {
			m.operation = msg.Operation
		}
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		m.emit(Action{Kind: ActionQuit})
		return m, tea.Quit
	case "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down":
		if m.cursor < len(m.blockers)-1 {
			m.cursor++
		}
	case "x":
		if len(m.blockers) > 0 {
			m.emit(Action{Kind: ActionKillBlocker, SPID: m.blockers[m.cursor].SPID})
		}
	case "k":
		m.emit(Action{Kind: ActionKillDDL})
	case "p":
		if m.resumable {
			m.emit(Action{Kind: ActionPause})
		}
	case "e":
		m.emit(Action{Kind: ActionExtend})
	case "s":
		m.emit(Action{Kind: ActionSnapshot})
	}
	return m, nil
}

// emit dispatches an action without blocking if the host is not ready.
func (m Model) emit(a Action) {
	if m.actions == nil {
		return
	}
	select {
	case m.actions <- a:
	default:
	}
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	selStyle   = lipgloss.NewStyle().Reverse(true)
	helpStyle  = lipgloss.NewStyle().Faint(true)
)

// View implements tea.Model.
func (m Model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("SqlGoPace — incident console") + "\n\n")
	fmt.Fprintf(&b, "operation: %s   [%s]\n", m.operation, m.status)
	fmt.Fprintf(&b, "progress: %.0f%%   ETA: %ds", m.percent, m.etaSeconds)
	if m.rollbackPercent > 0 {
		fmt.Fprintf(&b, "   rollback: %.0f%%", m.rollbackPercent)
	}
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "blocked sessions (%d):\n", len(m.blockers))
	for i, bl := range m.blockers {
		marker := "  "
		line := fmt.Sprintf("SPID %d  login=%s host=%s wait=%s (%dms)",
			bl.SPID, bl.Login, bl.Host, bl.WaitType, bl.WaitMS)
		if i == m.cursor {
			marker = "> "
			line = selStyle.Render(line)
		}
		fmt.Fprintf(&b, "%s%s\n", marker, line)
		if bl.Query != "" {
			fmt.Fprintf(&b, "    %s\n", truncate(bl.Query, 80))
		}
	}

	if len(m.waits) > 0 {
		fmt.Fprintf(&b, "\nwaits slowing the DDL (total %dms):\n", m.waitTotalMS)
		for _, w := range m.waits {
			fmt.Fprintf(&b, "  %-20s %8dms  %6d tasks\n", w.Name, w.WaitMS, w.Tasks)
		}
	}

	b.WriteString("\n" + helpStyle.Render(
		"[↑/↓] select  [x] kill blocker  [k] kill DDL  [p] pause  [e] extend  [s] snapshot  [q] quit"))
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
