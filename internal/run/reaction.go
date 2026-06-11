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
// and log pressure while preserving work); otherwise the operation is cancelled.
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
