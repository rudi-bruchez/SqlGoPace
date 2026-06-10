package run

// Pressure describes why the engine may need to react: the DDL is blocking other
// sessions beyond the configured timeout, or the transaction log is over its cap.
type Pressure struct {
	BlockingOthers bool
	LogOverCap     bool
}

// Any reports whether any pressure is present.
func (p Pressure) Any() bool { return p.BlockingOthers || p.LogOverCap }

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
