// Package tui is the incident console (--tui): a Bubble Tea model that shows live
// progress and blocked sessions, and turns operator key presses into action
// intents on a channel. It consumes the same monitoring data as silent mode; the
// host wires the action channel to the executor.
package tui

import (
	"fmt"
	"strings"
	"time"

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
	// StatusDraining means a graceful stop was requested: the run finishes the current
	// operation, then stops before the next one.
	StatusDraining
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
		return "CANCELING"
	case StatusDraining:
		return "DRAINING"
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
	// StatusMsg updates the lifecycle status (and optionally the operation label). A
	// non-zero StepTotal sets the manifest-level "op i/N" counter; a non-zero
	// StartedAt (re)starts the live elapsed timer for the current operation.
	StatusMsg struct {
		Status    Status
		Operation string
		StepIndex int
		StepTotal int
		StartedAt time.Time
	}
	// BatchMsg carries the running batch-DML operation's live progress (rows done vs
	// estimate, current batch size, throughput). Distinct from ProgressMsg, whose
	// percent comes from the server and is 0 for a batch loop.
	BatchMsg struct {
		Verb       string
		Table      string // schema.table
		RowsDone   int64
		EstRows    int64
		Percent    float64 // 0..1
		BatchRows  int
		RowsPerSec float64
	}
	// LogMsg appends a narration line.
	LogMsg struct{ Line string }
)

// tickMsg drives the once-a-second re-render that keeps the elapsed timer live.
type tickMsg time.Time

// tick schedules the next elapsed-timer refresh.
func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

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
	// ActionDrain requests a graceful stop after the current operation.
	ActionDrain
	// ActionSnapshot writes the current state to the log.
	ActionSnapshot
	// ActionIgnoreBlocker adds an ignore rule for the selected session to the running
	// manifest (so the DDL holds its lock through it). Criterion/Value/SPID carry the
	// chosen match.
	ActionIgnoreBlocker
	// ActionQuit leaves the console.
	ActionQuit
)

// Action is an operator intent, dispatched to the host via the action channel.
type Action struct {
	Kind ActionKind
	SPID int // set for ActionKillBlocker and ActionIgnoreBlocker

	// Criterion and Value carry the ignore match for ActionIgnoreBlocker: Criterion is
	// "session_id" | "app_name" | "login_name" | "host_name"; Value is the observed
	// attribute (empty for session_id, which uses SPID).
	Criterion string
	Value     string
}

// inputMode is the console's key-handling mode: normal, or prompting for the ignore
// criterion after the operator pressed "i".
type inputMode int

const (
	modeNormal inputMode = iota
	modeCriterion
)

// Model is the incident console state.
type Model struct {
	operation       string
	resumable       bool
	status          Status
	stepIndex       int
	stepTotal       int
	startedAt       time.Time     // current operation's start; anchors the elapsed timer
	elapsed         time.Duration // refreshed by the 1s tick
	percent         float64
	etaSeconds      int64
	rollbackPercent float64
	batch           BatchMsg
	hasBatch        bool
	blockers        []Blocker
	waits           []WaitCategory
	waitTotalMS     int64
	cursor          int
	mode            inputMode
	notice          string // last host feedback line (e.g. "ignoring SPID 53 …")
	actions         chan<- Action
	quitting        bool
}

// New returns a console model for the given operation. actions may be nil (no
// dispatch); resumable enables the pause action.
func New(operation string, resumable bool, actions chan<- Action) Model {
	return Model{operation: operation, resumable: resumable, status: StatusRunning, actions: actions}
}

// Init implements tea.Model; it starts the once-a-second tick that keeps the
// elapsed timer live. All other updates are host-driven.
func (m Model) Init() tea.Cmd { return tick() }

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
		if msg.StepTotal > 0 {
			m.stepIndex, m.stepTotal = msg.StepIndex, msg.StepTotal
		}
		if !msg.StartedAt.IsZero() {
			// A new operation started: reset the timer and drop the previous batch line.
			m.startedAt = msg.StartedAt
			m.elapsed = 0
			m.hasBatch = false
		}
	case BatchMsg:
		m.hasBatch = true
		m.batch = msg
	case tickMsg:
		if !m.startedAt.IsZero() {
			if t := time.Time(msg); !t.Before(m.startedAt) {
				m.elapsed = t.Sub(m.startedAt)
			}
		}
		return m, tick()
	case LogMsg:
		m.notice = msg.Line
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mode == modeCriterion {
		return m.handleCriterionKey(msg)
	}
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
	case "i":
		if len(m.blockers) > 0 {
			m.mode = modeCriterion
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
	case "d":
		// Toggle: request a graceful stop, or cancel a pending one. The host performs the
		// same toggle on the shared flag and confirms via StatusMsg.
		m.emit(Action{Kind: ActionDrain})
		if m.status == StatusDraining {
			m.status = StatusRunning // cancel the drain
		} else {
			m.status = StatusDraining
		}
	case "s":
		m.emit(Action{Kind: ActionSnapshot})
	}
	return m, nil
}

// handleCriterionKey handles the "ignore this session by …" sub-prompt: it emits an
// ActionIgnoreBlocker for the selected blocker and the chosen criterion, then returns
// to normal mode. Esc/q cancels; any other key keeps the prompt open.
func (m Model) handleCriterionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.blockers) { // the selection vanished while prompting
		m.mode = modeNormal
		return m, nil
	}
	bl := m.blockers[m.cursor]
	switch msg.String() {
	case "esc", "q":
	case "s":
		m.emit(Action{Kind: ActionIgnoreBlocker, SPID: bl.SPID, Criterion: "session_id"})
	case "a":
		m.emit(Action{Kind: ActionIgnoreBlocker, SPID: bl.SPID, Criterion: "app_name", Value: bl.Program})
	case "l":
		m.emit(Action{Kind: ActionIgnoreBlocker, SPID: bl.SPID, Criterion: "login_name", Value: bl.Login})
	case "h":
		m.emit(Action{Kind: ActionIgnoreBlocker, SPID: bl.SPID, Criterion: "host_name", Value: bl.Host})
	default:
		return m, nil // unrecognized key: stay in the prompt
	}
	m.mode = modeNormal
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
	op := m.operation
	if m.stepTotal > 0 {
		op = fmt.Sprintf("%d/%d %s", m.stepIndex, m.stepTotal, m.operation)
	}
	fmt.Fprintf(&b, "operation: %s   [%s]", op, m.status)
	if m.elapsed > 0 {
		fmt.Fprintf(&b, "   elapsed %s", formatElapsed(m.elapsed))
	}
	b.WriteString("\n")
	if m.hasBatch {
		fmt.Fprintf(&b, "batch %s %s: %d/%d rows (%.0f%%)   batch=%d   %.0f rows/s\n",
			m.batch.Verb, m.batch.Table, m.batch.RowsDone, m.batch.EstRows,
			m.batch.Percent*100, m.batch.BatchRows, m.batch.RowsPerSec)
	}
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

	help := "[↑/↓] select  [i] ignore  [x] kill blocker  [k] kill DDL  [p] pause  [e] extend  [d] drain/cancel  [s] snapshot  [q] quit"
	if m.mode == modeCriterion && m.cursor < len(m.blockers) {
		bl := m.blockers[m.cursor]
		help = fmt.Sprintf("ignore SPID %d as:  [s] session_id  [a] app=%s  [l] login=%s  [h] host=%s   [esc] cancel",
			bl.SPID, bl.Program, bl.Login, bl.Host)
	}
	b.WriteString("\n" + helpStyle.Render(help))
	if m.notice != "" {
		b.WriteString("\n" + m.notice)
	}
	return b.String()
}

// formatElapsed renders a duration as mm:ss, or h:mm:ss past an hour.
func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	mn := d / time.Minute
	d -= mn * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, mn, s)
	}
	return fmt.Sprintf("%02d:%02d", mn, s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
