package run

import "sync/atomic"

// DrainFlag is a cancellable graceful-stop signal shared by the CLI, the engine, and the
// chunked drivers. Request asks the run to stop after the current operation/chunk
// (finishing already-committed work); Cancel withdraws that request. Draining reports the
// current state and is what the engine and drivers check at each operation, chunk, and
// monitoring-poll boundary — so a Cancel takes effect as long as it lands before the next
// check. It is safe for concurrent use: the CLI's signal and TUI goroutines set it while
// the engine and driver goroutines read it.
type DrainFlag struct{ v atomic.Bool }

// Request asks for a graceful stop after the current operation/chunk.
func (d *DrainFlag) Request() { d.v.Store(true) }

// Cancel withdraws a graceful-stop request before it has been observed.
func (d *DrainFlag) Cancel() { d.v.Store(false) }

// Draining reports whether a graceful stop is currently requested.
func (d *DrainFlag) Draining() bool { return d.v.Load() }
