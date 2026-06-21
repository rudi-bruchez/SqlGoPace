package run

import "fmt"

// Pressure describes why the engine may need to react: the DDL is blocking other
// sessions beyond the configured timeout, or the transaction log is over its cap.
type Pressure struct {
	BlockingOthers bool
	LogOverCap     bool
	LogReuseWait   string // log_reuse_wait_desc when over cap, for the reaction detail
}

// Any reports whether any pressure is present.
func (p Pressure) Any() bool { return p.BlockingOthers || p.LogOverCap }

// Detail describes the pressure for a reaction log entry.
func (p Pressure) Detail() string {
	switch {
	case p.BlockingOthers && p.LogOverCap:
		return fmt.Sprintf("blocking other sessions and transaction log over cap%s", reuseWaitSuffix(p.LogReuseWait))
	case p.BlockingOthers:
		return "blocking other sessions"
	case p.LogOverCap:
		return fmt.Sprintf("transaction log over cap%s", reuseWaitSuffix(p.LogReuseWait))
	default:
		return "pressure"
	}
}

func reuseWaitSuffix(reuseWait string) string {
	if reuseWait == "" {
		return ""
	}
	return fmt.Sprintf(" (reuse_wait=%s)", reuseWait)
}

// ReactionEvent records a reaction the runner took while an operation ran, so the
// engine can narrate it and attach it to the run report.
type ReactionEvent struct {
	Kind   string // "pause" | "resume" | "cancel" | "kill"
	Detail string
}

// ReactionSink receives reaction events as they happen.
type ReactionSink func(ReactionEvent)

// Capabilities describes what the running operation and server support, which
// determines the least-destructive reaction available.
type Capabilities struct {
	Resumable bool // the running operation can PAUSE/RESUME
	ADR       bool // Accelerated Database Recovery makes a KILL rollback cheap
	// CancelSafe marks an operation whose cancellation is a clean stop with no
	// expensive rollback: REORGANIZE commits incrementally, DBCC CHECKDB is a
	// read-only snapshot, UPDATE STATISTICS rolls back cheaply. It does not change
	// which Action is chosen (these are not resumable, so they still Cancel) — it
	// refines the narration so the cancel is not mistaken for a rollback-bearing kill.
	CancelSafe bool
	// IgnoreBlocking suppresses the blocking reaction for this operation: it holds
	// its lock through to completion even while blocking other sessions. Transaction-
	// log pressure is still honored. Set per operation via the ignore_blocking option.
	IgnoreBlocking bool
	// IgnoredSessions lists sessions that are allowed to remain blocked by this run
	// (the manifest's ignore_blocked_sessions). Blocking is only reacted to when a
	// session matching none of these rules is blocked; transaction-log pressure is
	// still honored regardless.
	IgnoredSessions IgnoredSessions
}

// Action is the reaction the engine takes under pressure.
type Action int

const (
	// Continue means no reaction is needed.
	Continue Action = iota
	// Pause means PAUSE the resumable operation, wait for relief, then RESUME.
	Pause
	// Cancel means cancel the Go context and KILL as a fallback.
	Cancel
)

// String returns the action name.
func (a Action) String() string {
	switch a {
	case Continue:
		return "continue"
	case Pause:
		return "pause"
	case Cancel:
		return "cancel"
	default:
		return "unknown"
	}
}

// DecideReaction selects the least-destructive reaction for the current pressure
// and capabilities. A resumable operation is paused (which relieves both blocking
// and log pressure while preserving work); otherwise the operation is canceled.
func DecideReaction(p Pressure, c Capabilities) Action {
	switch {
	case !p.Any():
		return Continue
	case c.Resumable:
		return Pause
	default:
		return Cancel
	}
}

// reactionEvent builds the reaction event for a stop action. A Pause preserves an
// in-flight resumable operation; a Cancel of a cancel-safe (incremental) operation
// is a clean stop whose committed work survives, which the detail makes explicit so
// it reads as a clean cancel, not a rollback-bearing kill.
func reactionEvent(action Action, p Pressure, c Capabilities) ReactionEvent {
	if action == Pause {
		return ReactionEvent{Kind: "pause", Detail: p.Detail()}
	}
	detail := p.Detail()
	if c.CancelSafe {
		detail += "; incremental — committed work preserved, no rollback"
	}
	return ReactionEvent{Kind: "cancel", Detail: detail}
}
