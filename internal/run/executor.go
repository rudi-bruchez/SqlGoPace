package run

import (
	"context"
	"errors"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// Executor runs and controls a single DDL operation against the server.
type Executor interface {
	SPID() int
	ExecDDL(ctx context.Context, sql string) error
	Kill(ctx context.Context, spid int) error
	Pause(ctx context.Context, op ddl.Operation) error
	Resume(ctx context.Context, op ddl.Operation) error
}

var _ Executor = (*mssql.Conn)(nil)

// Sample is one monitoring snapshot taken while a DDL operation runs.
type Sample struct {
	BlockingOthers bool // our DDL is blocking other sessions
	LogOverCap     bool // the transaction log is over its configured cap
}

// Sampler produces monitoring snapshots.
type Sampler interface {
	Sample(ctx context.Context) (Sample, error)
}

// ErrCancelled signals the operation was cancelled under pressure and may be
// retried by the caller.
var ErrCancelled = errors.New("operation cancelled under pressure")

// supervise drives one running operation, reacting to monitoring samples until
// the DDL completes. samples streams snapshots; done delivers the DDL result.
// It returns the DDL error on completion, ErrCancelled when a cancel is decided,
// or the context error. Blocking pressure is debounced over blockingTimeout.
func supervise(
	ctx context.Context,
	clk Clock,
	exec Executor,
	op ddl.Operation,
	caps Capabilities,
	blockingTimeout time.Duration,
	samples <-chan Sample,
	done <-chan error,
) error {
	var blockingStart time.Time
	paused := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			return err
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
			}

			if paused {
				if !pressure.Any() {
					if err := exec.Resume(ctx, op); err != nil {
						return err
					}
					paused = false
				}
				continue
			}

			switch DecideReaction(pressure, caps) {
			case Continue:
				// keep running
			case Pause:
				if err := exec.Pause(ctx, op); err != nil {
					return err
				}
				paused = true
			case Cancel:
				return ErrCancelled
			}
		}
	}
}

// sampleProbe is the narrow set of server reads ServerSampler needs.
type sampleProbe interface {
	LogSpace(ctx context.Context) (mssql.LogSpace, error)
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

// Sample reads the log usage and active sessions and reports whether our DDL is
// blocking others and whether the log is over its cap.
func (s *ServerSampler) Sample(ctx context.Context) (Sample, error) {
	ls, err := s.probe.LogSpace(ctx)
	if err != nil {
		return Sample{}, err
	}
	sessions, err := s.probe.ActiveSessions(ctx)
	if err != nil {
		return Sample{}, err
	}

	blocking := false
	for _, sess := range sessions {
		if sess.BlockingSPID == s.spid {
			blocking = true
			break
		}
	}
	overCap := ls.UsedBytes() >= s.logMaxBytes || int(ls.UsedPercent) >= s.logMaxPercent
	return Sample{BlockingOthers: blocking, LogOverCap: overCap}, nil
}
