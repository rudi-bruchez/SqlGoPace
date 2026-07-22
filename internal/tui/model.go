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
	// StatusSuspended means a running operation is currently blocked, waiting on a lock
	// held by another session (the server's own request status for a blocked request). It
	// is a display-only state derived from the block, never set as the lifecycle status.
	StatusSuspended
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
	case StatusSuspended:
		return "SUSPENDED"
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

// OperationRow is one operation of the running manifest, with its live status, for the
// operations panel. Status is a display label: "TO RUN" | "RUNNING" | "SUSPENDED" | "DONE"
// | "FAILED" | "INTERRUPTED" | "INCOMPLETE" | "SKIPPED".
type OperationRow struct {
	Index  int
	Label  string // "<command> <target>", e.g. "shrink_data all"
	Status string
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
	// BlockedByMsg carries whether the running operation is itself blocked (the victim)
	// and, when it is, the session blocking it. This is the mirror of BlockersMsg, which
	// carries the sessions our DDL blocks; Blocked=false clears the indicator.
	BlockedByMsg struct {
		Blocked  bool
		SPID     int
		Login    string
		Program  string
		WaitType string
		WaitMS   int64
		Query    string
	}
	// SuspensionMsg carries the running history of how often and how long the operation
	// has been blocked (suspended) and by which sessions — the cumulative counterpart of
	// BlockedByMsg's live snapshot. Durations are sampled at the poll cadence.
	SuspensionMsg struct {
		Episodes int
		TotalMS  int64
		Blockers []SuspensionBlocker
	}
	// SuspensionBlocker is one session that has blocked our operation, with how many
	// times it started blocking us and for how long in total.
	SuspensionBlocker struct {
		SPID    int
		Login   string
		Host    string
		Count   int
		TotalMS int64
	}
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
	// ShrinkMsg carries the running shrink operation's live per-chunk progress. Like
	// BatchMsg it is distinct from ProgressMsg: a shrink's percent is the deterministic
	// fraction of the planned reduction (design §9), not the server's percent_complete
	// (which is 0 for the chunked DBCC SHRINKFILE loop).
	ShrinkMsg struct {
		File              string
		Type              string // "data" | "log"
		CurrentMB         int
		StartMB           int
		FinalMB           int
		StepMB            int     // current chunk increment
		Percent           float64 // 0..1
		Chunks            int     // chunks completed
		ChunksRemaining   int     // estimated chunks left
		ETASeconds        int     // estimated seconds left (with blocking)
		ETASecondsNoBlock int     // estimated seconds left over productive time only
		BlockedSeconds    int     // cumulative seconds spent blocked/stalled
		ChunkTargetMB     int     // target size the current chunk shrinks to (0 in the TRUNCATEONLY phase)
		Statement         string  // the literal T-SQL in flight (TRUNCATEONLY, then each DBCC SHRINKFILE chunk)
		PercentComplete   float64 // SQL Server's own percent_complete for the running chunk (0 when unavailable)
	}
	// SPIDMsg carries the session id of the DDL the console is monitoring, so the
	// operator can see which server session (and which blocks) is ours.
	SPIDMsg struct{ SPID int }
	// AlertMsg carries a prominent, sticky failure notice — typically a manifest that
	// failed preflight (e.g. the login lacks db_owner for a shrink). Unlike LogMsg's
	// single overwritten notice line, alerts accumulate and are rendered near the top so
	// the operator sees the reason even as other state updates.
	AlertMsg struct {
		Title string
		Lines []string
	}
	// LogMsg appends a narration line.
	LogMsg struct{ Line string }
	// ServerInfoMsg carries the target's identity for the header banner. Sent once at
	// startup; App is the SqlGoPace version, Product the SQL Server year label.
	ServerInfoMsg struct {
		App         string
		Name        string
		Product     string
		Database    string
		Edition     string
		Recovery    string
		ADR         bool
		RCSI        bool
		SnapshotIso bool
	}
	// OperationsMsg carries the full operation list of the running manifest, sent once when
	// it starts, so the operations panel can show pending ops (not just the current one).
	OperationsMsg struct{ Ops []OperationRow }
	// StepDoneMsg reports an operation's terminal outcome so its row can show DONE/FAILED/…
	// (Outcome mirrors run.StepEvent.Outcome: "success"|"failed"|"interrupted"|
	// "incomplete"|"skipped").
	StepDoneMsg struct {
		Index   int
		Outcome string
	}
	// KillerArmedMsg tells the console whether kill_blockers is enabled in config, so the roster
	// can warn that armed rules will not fire until it is. Sent once at startup.
	KillerArmedMsg struct{ Armed bool }
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
	// ActionDrain requests a graceful stop after the current operation.
	ActionDrain
	// ActionIgnoreBlocker adds an ignore rule for the selected session to the running
	// manifest (so the DDL holds its lock through it). Criterion/Value/SPID carry the
	// chosen match.
	ActionIgnoreBlocker
	// ActionKillBlockerAuto kills the selected blocking session now AND adds a kill rule
	// for it to the running manifest, so a recurrence is auto-killed without a restart.
	// Criterion/Value/SPID carry the chosen match (the inverse of ActionIgnoreBlocker).
	ActionKillBlockerAuto
	// ActionArmKillRule appends a kill_blocked_sessions rule (Criterion/Value) to the running
	// manifest without killing now: a session that later blocks the DDL and matches the rule is
	// terminated by the armed BlockerKiller. Emitted from the blocker roster.
	ActionArmKillRule
	// ActionDisarmKillRule removes the matching kill_blocked_sessions rule from the running
	// manifest — the inverse of ActionArmKillRule.
	ActionDisarmKillRule
	// ActionQuit leaves the console.
	ActionQuit
)

// Action is an operator intent, dispatched to the host via the action channel.
type Action struct {
	Kind ActionKind
	SPID int // set for ActionKillBlocker, ActionIgnoreBlocker, and ActionKillBlockerAuto

	// Criterion and Value carry the match for ActionIgnoreBlocker / ActionKillBlockerAuto:
	// Criterion is "session_id" | "app_name" | "login_name" | "host_name"; Value is the
	// observed attribute (empty for session_id, which uses SPID).
	Criterion string
	Value     string
}

// inputMode is the console's key-handling mode: normal, or prompting for the match
// criterion after the operator pressed "i" (ignore) or "X" (kill + auto-kill).
type inputMode int

const (
	modeNormal        inputMode = iota
	modeCriterion               // ignore-this-session prompt (from "i")
	modeKillCriterion           // kill-and-remember prompt (from "X")
)

// Model is the incident console state.
type Model struct {
	operation       string
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
	shrink          ShrinkMsg
	hasShrink       bool
	spid            int
	alerts          []AlertMsg
	blockedBy       BlockedByMsg  // set when our operation is itself blocked (the victim)
	suspension      SuspensionMsg // cumulative suspension history (how long/often/by whom)
	blockers        []Blocker
	waits           []WaitCategory
	waitTotalMS     int64
	cursor          int
	mode            inputMode
	notice          string // last host feedback line (e.g. "ignoring SPID 53 …")
	actions         chan<- Action
	quitting        bool

	server    ServerInfoMsg  // header banner; zero value renders no server line
	ops       []OperationRow // the running manifest's operations, with live status
	width     int            // terminal width, from WindowSizeMsg (0 until first resize)
	height    int            // terminal height, budgets the operations panel so lower panels stay visible
	expandSQL bool           // Enter toggles: show the selected blocker's full SQL
	showHelp  bool           // '?' toggles the shortcuts footer

	rosterOpen   bool            // the blocker roster modal is showing
	rosterCursor int             // selected group within the roster
	rosterByHost bool            // roster grouping key: false = login, true = host
	armed        map[string]bool // roster-armed kill rules, keyed "criterion=value" (drives ✓)
	killerArmed  bool            // whether kill_blockers is enabled in config (roster warning)
}

// New returns a console model for the given operation. actions may be nil (no dispatch).
func New(operation string, actions chan<- Action) Model {
	return Model{operation: operation, status: StatusRunning, actions: actions, showHelp: true, armed: map[string]bool{}}
}

// Init implements tea.Model; it starts the once-a-second tick that keeps the
// elapsed timer live. All other updates are host-driven.
func (m Model) Init() tea.Cmd { return tick() }

// displayStatus is the status shown to the operator. A running operation that is
// currently blocked reads SUSPENDED (the server's request status for a blocked
// request); the lifecycle states (paused, draining, canceling, done) take precedence
// over the transient block, so the substitution applies only while RUNNING.
func (m Model) displayStatus() Status {
	if m.status == StatusRunning && m.blockedBy.Blocked {
		return StatusSuspended
	}
	return m.status
}

// setOpStatus sets the status of the operation with the given 1-based index in the
// operations panel, if present (a no-op before the OperationsMsg arrives).
func (m *Model) setOpStatus(index int, status string) {
	for i := range m.ops {
		if m.ops[i].Index == index {
			m.ops[i].Status = status
			return
		}
	}
}

// rosterGroup is one row of the blocker roster: a distinct login or host that has blocked the
// DDL this run, with its aggregate episode count and total blocked time. Value is "" for the
// non-armable "(unknown)" group (the blocking session was absent from the snapshot).
type rosterGroup struct {
	Criterion string // "login_name" | "host_name"
	Value     string
	Count     int
	TotalMS   int64
}

// rosterKey is the armed-set key for a group: "<criterion>=<value>".
func rosterKey(criterion, value string) string { return criterion + "=" + value }

// rosterGroups folds the suspension history (sessions that blocked us) by the active grouping
// key, summing episode count and total blocked time, preserving first-seen order.
func (m Model) rosterGroups() []rosterGroup {
	criterion := "login_name"
	pick := func(b SuspensionBlocker) string { return b.Login }
	if m.rosterByHost {
		criterion = "host_name"
		pick = func(b SuspensionBlocker) string { return b.Host }
	}
	idx := make(map[string]int)
	var out []rosterGroup
	for _, b := range m.suspension.Blockers {
		v := pick(b)
		i, ok := idx[v]
		if !ok {
			i = len(out)
			idx[v] = i
			out = append(out, rosterGroup{Criterion: criterion, Value: v})
		}
		out[i].Count += b.Count
		out[i].TotalMS += b.TotalMS
	}
	return out
}

// opStatusLabel maps a run.StepEvent.Outcome to the operations-panel label.
func opStatusLabel(outcome string) string {
	switch outcome {
	case "success":
		return "DONE"
	case "failed":
		return "FAILED"
	case "interrupted":
		return "INTERRUPTED"
	case "incomplete":
		return "INCOMPLETE"
	case "skipped":
		return "SKIPPED"
	default:
		return strings.ToUpper(outcome)
	}
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ProgressMsg:
		m.percent = msg.Percent
		m.etaSeconds = msg.ETASeconds
		m.rollbackPercent = msg.RollbackPercent
	case BlockedByMsg:
		m.blockedBy = msg
	case SuspensionMsg:
		m.suspension = msg
	case BlockersMsg:
		// Keep the selection pinned to the session's SPID across polls, not its list index:
		// the poll replaces the whole slice, and a reorder/removal would otherwise leave the
		// cursor over a DIFFERENT session — so `x` (kill) / `i` (ignore) could hit the wrong one.
		selected := -1
		if m.cursor >= 0 && m.cursor < len(m.blockers) {
			selected = m.blockers[m.cursor].SPID
		}
		m.blockers = msg.Blockers
		m.cursor = 0
		found := false
		for i, b := range m.blockers {
			if b.SPID == selected {
				m.cursor, found = i, true
				break
			}
		}
		if !found {
			// The session whose SQL was expanded is gone; don't auto-expand whatever now
			// sits at index 0 — the operator never selected it.
			m.expandSQL = false
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
			m.setOpStatus(msg.StepIndex, "RUNNING")
		}
		if !msg.StartedAt.IsZero() {
			// A new operation started: reset the timer, the previous batch/shrink line, and
			// the victim/progress state — otherwise a short op inherits the prior op's
			// "BLOCKED"/SUSPENDED indicator (and stale percent) until the next poll refreshes it.
			m.startedAt = msg.StartedAt
			m.elapsed = 0
			m.hasBatch = false
			m.hasShrink = false
			m.blockedBy = BlockedByMsg{}
			m.percent, m.etaSeconds, m.rollbackPercent = 0, 0, 0
		}
	case ServerInfoMsg:
		m.server = msg
	case OperationsMsg:
		m.ops = msg.Ops
	case StepDoneMsg:
		m.setOpStatus(msg.Index, opStatusLabel(msg.Outcome))
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case BatchMsg:
		m.hasBatch = true
		m.batch = msg
	case ShrinkMsg:
		m.hasShrink = true
		m.shrink = msg
	case SPIDMsg:
		m.spid = msg.SPID
	case AlertMsg:
		m.alerts = append(m.alerts, msg)
	case KillerArmedMsg:
		m.killerArmed = msg.Armed
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
	if m.inCriterionMode() {
		return m.handleCriterionKey(msg)
	}
	if m.rosterOpen {
		return m.handleRosterKey(msg)
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
	case "enter":
		// Toggle showing the selected blocker's full SQL (the mockup's "click to see full sql").
		if len(m.blockers) > 0 {
			m.expandSQL = !m.expandSQL
		}
	case "?":
		m.showHelp = !m.showHelp
	case "b":
		m.rosterOpen = true
		m.rosterCursor = 0
	case "x":
		if len(m.blockers) > 0 {
			m.emit(Action{Kind: ActionKillBlocker, SPID: m.blockers[m.cursor].SPID})
		}
	case "X":
		if len(m.blockers) > 0 {
			m.mode = modeKillCriterion
		}
	case "k":
		m.emit(Action{Kind: ActionKillDDL})
	case "d":
		// Toggle: request a graceful stop, or cancel a pending one. The host performs the
		// same toggle on the shared flag and confirms via StatusMsg.
		m.emit(Action{Kind: ActionDrain})
		if m.status == StatusDraining {
			m.status = StatusRunning // cancel the drain
		} else {
			m.status = StatusDraining
		}
	}
	return m, nil
}

// inCriterionMode reports whether a match-criterion sub-prompt is open (ignore or kill).
func (m Model) inCriterionMode() bool {
	return m.mode == modeCriterion || m.mode == modeKillCriterion
}

// handleCriterionKey handles the match-criterion sub-prompt shared by "ignore this session
// by …" (modeCriterion) and "kill + remember this session by …" (modeKillCriterion): it
// emits the corresponding action for the selected blocker and chosen criterion, then returns
// to normal mode. Esc/q cancels; any other key keeps the prompt open.
func (m Model) handleCriterionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.blockers) { // the selection vanished while prompting
		m.mode = modeNormal
		return m, nil
	}
	kind := ActionIgnoreBlocker
	if m.mode == modeKillCriterion {
		kind = ActionKillBlockerAuto
	}
	bl := m.blockers[m.cursor]
	switch msg.String() {
	case "esc", "q":
	case "s":
		m.emit(Action{Kind: kind, SPID: bl.SPID, Criterion: "session_id"})
	case "a":
		m.emit(Action{Kind: kind, SPID: bl.SPID, Criterion: "app_name", Value: bl.Program})
	case "l":
		m.emit(Action{Kind: kind, SPID: bl.SPID, Criterion: "login_name", Value: bl.Login})
	case "h":
		m.emit(Action{Kind: kind, SPID: bl.SPID, Criterion: "host_name", Value: bl.Host})
	default:
		return m, nil // unrecognized key: stay in the prompt
	}
	m.mode = modeNormal
	return m, nil
}

// handleRosterKey drives the blocker-roster modal: navigate groups, toggle the login/host
// grouping, and arm/disarm a kill rule for the selected group. b/esc/q close it (q closes the
// roster, it does not quit the app while the roster is open).
func (m Model) handleRosterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	groups := m.rosterGroups()
	switch msg.String() {
	case "b", "esc", "q":
		m.rosterOpen = false
	case "up":
		if m.rosterCursor > 0 {
			m.rosterCursor--
		}
	case "down":
		if m.rosterCursor < len(groups)-1 {
			m.rosterCursor++
		}
	case "g":
		m.rosterByHost = !m.rosterByHost
		m.rosterCursor = 0
	case "enter", " ":
		if m.rosterCursor >= len(groups) {
			return m, nil
		}
		g := groups[m.rosterCursor]
		if g.Value == "" {
			return m, nil // the (unknown) row has no criterion value to match on
		}
		key := rosterKey(g.Criterion, g.Value)
		if m.armed[key] {
			m.emit(Action{Kind: ActionDisarmKillRule, Criterion: g.Criterion, Value: g.Value})
			delete(m.armed, key)
		} else {
			m.emit(Action{Kind: ActionArmKillRule, Criterion: g.Criterion, Value: g.Value})
			m.armed[key] = true
		}
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

// HumanizeMB renders a size in megabytes compactly, escalating to GB then TB so large
// file sizes stay legible: "500 MB", "9.1 GB", "16.00 TB". Exported so the non-TUI
// stdout progress line formats identically.
func HumanizeMB(mb int) string {
	switch {
	case mb < 1024:
		return fmt.Sprintf("%d MB", mb)
	case mb < 1024*1024:
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	default:
		return fmt.Sprintf("%.2f TB", float64(mb)/(1024*1024))
	}
}

// humanizeMS renders a millisecond duration compactly, escalating the unit so large
// values stay readable: "775ms", "2.8s", "6m35s", "1h04m", and — past 72h, where an
// hours count becomes unwieldy (a multi-day shrink ETA) — days and hours: "32d06h".
func humanizeMS(ms int64) string {
	const hoursToDays = int64(72 * 3_600_000) // switch to days past 72h
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		s := ms / 1000
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	case ms < hoursToDays:
		s := ms / 1000
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	default:
		s := ms / 1000
		return fmt.Sprintf("%dd%02dh", s/86400, (s%86400)/3600)
	}
}

// humanizeCount renders a task/row tally compactly with k/m suffixes so large wait-task
// counts stay readable: 21 -> "21", 768162 -> "768k", 7446916 -> "7.4m". Values under a
// thousand are shown as-is; a scaled value keeps one decimal only while it is below ten.
func humanizeCount(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return scaledCount(float64(n)/1000, "k")
	default:
		return scaledCount(float64(n)/1_000_000, "m")
	}
}

func scaledCount(v float64, unit string) string {
	if v < 10 {
		return fmt.Sprintf("%.1f%s", v, unit)
	}
	return fmt.Sprintf("%.0f%s", v, unit)
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
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
