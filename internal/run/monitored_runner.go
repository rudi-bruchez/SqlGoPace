package run

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// MonitoredRunner is the production OpRunner: it executes a DDL statement on the
// execution connection while a sampling goroutine feeds monitoring snapshots to
// the supervisor, which reacts under pressure. To pause a resumable operation it
// aborts the running statement (a context cancel that pauses the operation while
// preserving its work), waits for relief, then issues ALTER INDEX ... RESUME; a
// non-resumable operation is canceled and retried up to MaxRetries. An explicit
// KILL is only a fallback when the abort does not stop the statement in time.
type MonitoredRunner struct {
	exec            Executor
	sampler         Sampler
	clk             Clock
	pollInterval    time.Duration // blocking poll cadence
	logPollInterval time.Duration // transaction-log poll cadence (coarser)
	blockingTimeout time.Duration
	logDrainTimeout time.Duration
	killGrace       time.Duration
	maxRetries      int
}

// RunnerConfig configures a MonitoredRunner.
type RunnerConfig struct {
	PollInterval    time.Duration // blocking poll cadence
	LogPollInterval time.Duration // transaction-log poll cadence
	BlockingTimeout time.Duration
	LogDrainTimeout time.Duration // abort a paused op if the log does not drain within this
	KillGrace       time.Duration
	MaxRetries      int
}

// NewMonitoredRunner builds a MonitoredRunner. A zero LogPollInterval falls back
// to the blocking poll interval.
func NewMonitoredRunner(exec Executor, sampler Sampler, clk Clock, cfg RunnerConfig) *MonitoredRunner {
	logPoll := cfg.LogPollInterval
	if logPoll <= 0 {
		logPoll = cfg.PollInterval
	}
	return &MonitoredRunner{
		exec:            exec,
		sampler:         sampler,
		clk:             clk,
		pollInterval:    cfg.PollInterval,
		logPollInterval: logPoll,
		blockingTimeout: cfg.BlockingTimeout,
		logDrainTimeout: cfg.LogDrainTimeout,
		killGrace:       cfg.KillGrace,
		maxRetries:      cfg.MaxRetries,
	}
}

// ErrLogDrainTimeout signals a paused operation was aborted because the
// transaction log did not drain within log_drain_timeout_minutes.
var ErrLogDrainTimeout = errors.New("transaction log did not drain within timeout")

// Run executes op, retrying after a pressure cancel up to MaxRetries. Reactions
// (pause/resume/cancel/kill) are reported through sink as they happen.
func (r *MonitoredRunner) Run(ctx context.Context, op ddl.Operation, sql string, caps Capabilities, sink ReactionSink) error {
	for attempt := 0; ; attempt++ {
		err := r.runOnce(ctx, op, sql, caps, sink)
		if !errors.Is(err, ErrCancelled) {
			return err
		}
		if attempt >= r.maxRetries {
			return fmt.Errorf("retries exhausted after %d attempt(s): %w", attempt+1, err)
		}
	}
}

// runOnce executes the operation under monitoring, pausing and resuming a
// resumable operation under pressure until it completes. It returns ErrCancelled
// when a non-resumable operation is canceled under pressure, so Run retries it.
func (r *MonitoredRunner) runOnce(ctx context.Context, op ddl.Operation, sql string, caps Capabilities, sink ReactionSink) error {
	return runLoop(
		sql,
		func(stmt string) (Action, error) { return r.runStatement(ctx, stmt, caps, sink) },
		func() error { return r.awaitRelief(ctx, caps.Ignore, sink) },
		func() (string, error) {
			sink(ReactionEvent{Kind: "resume", Detail: "pressure cleared"})
			return ddl.ResumableControlSQL(op, "RESUME")
		},
	)
}

// awaitRelief samples the server (via its own pump) until the pressure that
// triggered a pause clears, enforcing the log-drain timeout.
func (r *MonitoredRunner) awaitRelief(ctx context.Context, ignore IgnoreSource, sink ReactionSink) error {
	sampleCtx, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	samples := make(chan Sample)
	go pumpSamples(sampleCtx, samples, r.sampler, r.pollInterval, r.logPollInterval, ignore)

	return waitForRelief(ctx, r.clk, r.logDrainTimeout, samples, sink)
}

// runLoop drives the pause/resume state machine for one operation. It runs the
// current statement; on Pause it waits for relief and resumes (running the RESUME
// statement next); on Cancel it reports ErrCancelled; otherwise it returns the
// statement's terminal result. The function arguments isolate the loop logic from
// the live execution and timing so it can be tested deterministically.
func runLoop(sql string, runStatement func(string) (Action, error), waitForRelief func() error, resumeSQL func() (string, error)) error {
	stmt := sql
	for {
		action, err := runStatement(stmt)
		switch action {
		case Cancel:
			return ErrCancelled
		case Stop:
			// Graceful stop: the statement was paused (runStatement stopped it, preserving
			// the resumable's work). Return without resuming; the next run continues it.
			return ErrStopped
		case Pause:
			if err := waitForRelief(); err != nil {
				return err
			}
			next, err := resumeSQL()
			if err != nil {
				return err
			}
			stmt = next
		default: // Continue: the statement finished (success or real failure)
			return err
		}
	}
}

// runStatement runs one statement under monitoring. On a Pause or Cancel decision
// it stops the statement by canceling the execution context — an attention that
// aborts the statement, which pauses a resumable operation while keeping the
// pinned connection alive for a later RESUME. If the statement does not stop
// within the grace period, it issues an explicit KILL as a fallback. Reactions
// are reported through sink.
func (r *MonitoredRunner) runStatement(ctx context.Context, sql string, caps Capabilities, sink ReactionSink) (Action, error) {
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	done := make(chan error, 1)
	go func() { done <- r.exec.ExecDDL(execCtx, sql) }()

	sampleCtx, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	samples := make(chan Sample)
	go pumpSamples(sampleCtx, samples, r.sampler, r.pollInterval, r.logPollInterval, caps.Ignore)

	action, pressure, err := supervise(ctx, r.clk, caps, r.blockingTimeout, samples, done)
	if action == Continue {
		return Continue, err
	}

	sink(reactionEvent(action, pressure, caps))

	// Stop the running statement: abort via context cancel first, KILL as a
	// fallback if it does not stop in time.
	cancelExec()
	select {
	case <-done:
		// stopped on its own (paused for a resumable op, rolled back otherwise)
	case <-time.After(r.killGrace):
		sink(ReactionEvent{Kind: "kill", Detail: "abort did not stop the statement within the grace period"})
		_ = r.exec.Kill(context.Background(), r.exec.SPID())
		<-done
	}
	return action, nil
}

// waitForRelief consumes monitoring snapshots until the pressure that triggered a
// pause has cleared, so the operation can be resumed. If the transaction log stays
// over cap for longer than logDrainTimeout, it aborts with ErrLogDrainTimeout
// (recording the observed log_reuse_wait). It is pure (channel + clock driven) so
// it can be tested deterministically.
func waitForRelief(ctx context.Context, clk Clock, logDrainTimeout time.Duration, samples <-chan Sample, sink ReactionSink) error {
	var logOverCapSince time.Time

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case s := <-samples:
			if s.LogOverCap {
				if logOverCapSince.IsZero() {
					logOverCapSince = clk.Now()
				}
				if logDrainTimeout > 0 && clk.Since(logOverCapSince) >= logDrainTimeout {
					detail := fmt.Sprintf("transaction log did not drain within timeout%s", reuseWaitSuffix(s.LogReuseWait))
					sink(ReactionEvent{Kind: "abort", Detail: detail})
					return fmt.Errorf("%w (reuse_wait=%s)", ErrLogDrainTimeout, s.LogReuseWait)
				}
			} else {
				logOverCapSince = time.Time{}
			}
			if !s.BlockingOthers && !s.LogOverCap {
				return nil
			}
		}
	}
}
