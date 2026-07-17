package run

import (
	"context"
	"fmt"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// ServerClock reads the SQL Server's local wall-clock time, used to evaluate a
// manifest's execution window. *mssql.Conn satisfies it via ServerNow.
type ServerClock interface {
	ServerNow(ctx context.Context) (time.Time, error)
}

var _ ServerClock = (*mssql.Conn)(nil)

// WithServerClock wires the server clock so manifests carrying a window are gated
// against server time. Without it, a windowed manifest fails with a clear error.
func WithServerClock(c ServerClock) EngineOption { return func(e *Engine) { e.serverClock = c } }

// windowOpen reports whether window w is currently open in server time. A nil
// window is always open. A non-nil window with no server clock wired is a
// configuration error. A clock read error is returned to the caller, which
// applies the conservative fallback (defer / stop).
func (e *Engine) windowOpen(ctx context.Context, w *ddl.Window) (bool, error) {
	if w == nil {
		return true, nil
	}
	if e.serverClock == nil {
		return false, fmt.Errorf("manifest declares a window but no server clock is configured")
	}
	now, err := e.serverClock.ServerNow(ctx)
	if err != nil {
		// One retry: a transient scan/connection blip often clears immediately.
		now, err = e.serverClock.ServerNow(ctx)
		if err != nil {
			return false, err
		}
	}
	return w.Contains(now), nil
}

// windowStopReason describes why a windowed run stopped: a failed server-clock read
// (so an operator is not told "window closed" when the real cause was connectivity), or
// the window itself closing at the given point.
func windowStopReason(err error, closedAt string) string {
	if err != nil {
		return fmt.Sprintf("could not read server time (%v)", err)
	}
	return "window closed " + closedAt
}

// finalizeWindowClosed records a graceful stop because the manifest's execution window
// closed (or its clock could not be read). Like a drain, the manifest stays in processing
// with its resume cursor so the next run inside the window continues.
func (e *Engine) finalizeWindowClosed(ctx context.Context, name string, rep *report.RunReport, start time.Time, reason string, quarantined []ddl.Operation) runOutcome {
	return e.finalizeGracefulStop(ctx, name, rep, start, quarantined,
		reason+" — resumes in the next window",
		fmt.Sprintf("-- %s: %s — left in processing, resumes next window", name, reason))
}
