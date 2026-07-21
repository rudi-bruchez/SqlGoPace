package run

import (
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// StepPhase distinguishes the start of an operation from its completion in a StepEvent.
type StepPhase int

const (
	// StepStarted marks the beginning of an operation; Duration and Outcome are unset.
	StepStarted StepPhase = iota
	// StepFinished marks the end of an operation; Duration and Outcome are set.
	StepFinished
)

// StepEvent reports manifest-level progress: which operation (1-based Index of Total)
// is starting or finishing. It lets stdout and the TUI show a readable "op i/N"
// counter and per-op timing that is independent of the server's percent_complete
// (which stays 0 for an offline rebuild). The engine emits it from its operation
// loop; consumers attach via WithStepSink.
type StepEvent struct {
	Index, Total int           // 1-based position and total, after plan/expansion
	Command      string        // operation CommandType, e.g. "rebuild_index"
	Target       string        // schema.table[.name] label
	StartedAt    time.Time     // operation start; the elapsed-time anchor
	Phase        StepPhase     // StepStarted | StepFinished
	Duration     time.Duration // set on StepFinished
	Outcome      string        // set on StepFinished: "success" | "failed" | "interrupted" | "incomplete" | "skipped"
	Detail       string        // optional note on StepFinished, e.g. a skip reason ("already PAGE")
}

// finished returns a StepFinished copy of a StepStarted event, filling in the outcome
// and duration. The engine builds the base (started) event once per operation and
// derives the finished event from it, so Index/Total/Command/Target stay consistent.
func (e StepEvent) finished(outcome string, dur time.Duration) StepEvent {
	e.Phase = StepFinished
	e.Outcome = outcome
	e.Duration = dur
	return e
}

// OpInfo is one operation of a manifest, for the whole-list signal the TUI uses to show
// pending operations (not just the running one). Emitted once per manifest, before the
// operation loop, via WithOpListSink.
type OpInfo struct {
	Index   int    // 1-based position, after plan/expansion
	Command string // operation CommandType, e.g. "shrink_data"
	Target  string // schema.table[.name] / file / database label
}

// emitStep delivers a step event to the sink when one is wired.
func (e *Engine) emitStep(ev StepEvent) {
	if e.stepSink != nil {
		e.stepSink(ev)
	}
}

// emitOpList delivers the full operation list to the sink when one is wired.
func (e *Engine) emitOpList(ops []OpInfo) {
	if e.opListSink != nil {
		e.opListSink(ops)
	}
}

// opDuration is the operation report's measured duration as a time.Duration (the
// report stores it in milliseconds).
func opDuration(r report.OperationReport) time.Duration {
	return time.Duration(r.DurationMS) * time.Millisecond
}
