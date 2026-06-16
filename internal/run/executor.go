package run

import (
	"context"
	"errors"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// Executor runs and controls a single DDL operation against the server. Pausing a
// resumable operation is done by aborting the running statement (canceling its
// context), not by a separate ALTER INDEX PAUSE, so the interface only needs to
// run, identify, and kill the execution session.
type Executor interface {
	SPID() int
	ExecDDL(ctx context.Context, sql string) error
	Kill(ctx context.Context, spid int) error
}

var _ Executor = (*mssql.Conn)(nil)

// Sample is one monitoring snapshot taken while a DDL operation runs. Blocking and
// log state are polled on independent intervals, so a snapshot carries the latest
// known value of each.
type Sample struct {
	BlockingOthers bool   // our DDL is blocking other sessions
	LogOverCap     bool   // the transaction log is over its configured cap
	LogReuseWait   string // why the log cannot truncate (only set when over cap)
}

// LogSample is the transaction-log half of a monitoring snapshot.
type LogSample struct {
	OverCap   bool
	ReuseWait string // log_reuse_wait_desc, populated only when over cap
}

// Sampler reads the two monitored dimensions on independent cadences: blocking
// (frequent) and transaction-log pressure (less frequent).
type Sampler interface {
	Blocking(ctx context.Context) (bool, error)
	Log(ctx context.Context) (LogSample, error)
}

// ErrCancelled signals the operation was canceled under pressure and may be
// retried by the caller.
var ErrCancelled = errors.New("operation canceled under pressure")

// supervise monitors one running statement and returns the reaction to take,
// along with the pressure that triggered it. It returns (Continue, _, err) when
// the statement finishes on its own (err is nil on success) or the context is
// canceled, and (Pause|Cancel, pressure, nil) when sustained pressure warrants
// stopping the statement. samples streams snapshots; done delivers the statement
// result. Blocking pressure is debounced over blockingTimeout.
func supervise(
	ctx context.Context,
	clk Clock,
	caps Capabilities,
	blockingTimeout time.Duration,
	samples <-chan Sample,
	done <-chan error,
) (Action, Pressure, error) {
	var blockingStart time.Time

	for {
		select {
		case <-ctx.Done():
			return Continue, Pressure{}, ctx.Err()
		case err := <-done:
			return Continue, Pressure{}, err
		case s := <-samples:
			if s.BlockingOthers {
				if blockingStart.IsZero() {
					blockingStart = clk.Now()
				}
			} else {
				blockingStart = time.Time{}
			}

			pressure := Pressure{
				BlockingOthers: !blockingStart.IsZero() && clk.Since(blockingStart) >= blockingTimeout,
				LogOverCap:     s.LogOverCap,
				LogReuseWait:   s.LogReuseWait,
			}
			if action := DecideReaction(pressure, caps); action != Continue {
				return action, pressure, nil
			}
		}
	}
}

// sampleProbe is the narrow set of server reads ServerSampler needs.
type sampleProbe interface {
	LogSpace(ctx context.Context) (mssql.LogSpace, error)
	LogReuseWait(ctx context.Context) (string, error)
	ActiveSessions(ctx context.Context) ([]mssql.Session, error)
}

// ServerSampler builds a Sample from live server state for the DDL session.
type ServerSampler struct {
	probe         sampleProbe
	spid          int
	logMaxBytes   int64
	logMaxPercent int
}

// NewServerSampler returns a sampler for the given DDL session and log thresholds.
func NewServerSampler(probe sampleProbe, spid int, logMaxBytes int64, logMaxPercent int) *ServerSampler {
	return &ServerSampler{probe: probe, spid: spid, logMaxBytes: logMaxBytes, logMaxPercent: logMaxPercent}
}

var _ Sampler = (*ServerSampler)(nil)

// Blocking reports whether any active session is blocked by our DDL.
func (s *ServerSampler) Blocking(ctx context.Context) (bool, error) {
	sessions, err := s.probe.ActiveSessions(ctx)
	if err != nil {
		return false, err
	}
	for _, sess := range sessions {
		if sess.BlockingSPID == s.spid {
			return true, nil
		}
	}
	return false, nil
}

// Log reports whether the transaction log is over its cap and, when it is, why it
// cannot truncate (log_reuse_wait_desc). The reuse-wait query is only run when
// over cap, to keep the steady-state poll cheap.
func (s *ServerSampler) Log(ctx context.Context) (LogSample, error) {
	ls, err := s.probe.LogSpace(ctx)
	if err != nil {
		return LogSample{}, err
	}
	overCap := ls.UsedBytes() >= s.logMaxBytes || int(ls.UsedPercent) >= s.logMaxPercent
	if !overCap {
		return LogSample{}, nil
	}
	reuseWait, err := s.probe.LogReuseWait(ctx)
	if err != nil {
		return LogSample{}, err
	}
	return LogSample{OverCap: true, ReuseWait: reuseWait}, nil
}
