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
// the supervisor, which reacts (pause/resume or cancel) under pressure. A
// cancelled operation is killed after a grace period and retried up to MaxRetries.
type MonitoredRunner struct {
	exec            Executor
	sampler         Sampler
	clk             Clock
	pollInterval    time.Duration
	blockingTimeout time.Duration
	killGrace       time.Duration
	maxRetries      int
	capsFor         func(ddl.Operation) Capabilities
}

// RunnerConfig configures a MonitoredRunner.
type RunnerConfig struct {
	PollInterval    time.Duration
	BlockingTimeout time.Duration
	KillGrace       time.Duration
	MaxRetries      int
	// CapsFor reports the reaction capabilities for an operation (e.g. whether
	// RESUMABLE was injected and whether ADR is on).
	CapsFor func(ddl.Operation) Capabilities
}

// NewMonitoredRunner builds a MonitoredRunner.
func NewMonitoredRunner(exec Executor, sampler Sampler, clk Clock, cfg RunnerConfig) *MonitoredRunner {
	capsFor := cfg.CapsFor
	if capsFor == nil {
		capsFor = func(ddl.Operation) Capabilities { return Capabilities{} }
	}
	return &MonitoredRunner{
		exec:            exec,
		sampler:         sampler,
		clk:             clk,
		pollInterval:    cfg.PollInterval,
		blockingTimeout: cfg.BlockingTimeout,
		killGrace:       cfg.KillGrace,
		maxRetries:      cfg.MaxRetries,
		capsFor:         capsFor,
	}
}

// Run executes op, retrying after a pressure cancel up to MaxRetries.
func (r *MonitoredRunner) Run(ctx context.Context, op ddl.Operation, sql string) error {
	for attempt := 0; ; attempt++ {
		err := r.runOnce(ctx, op, sql)
		if !errors.Is(err, ErrCancelled) {
			return err
		}
		if attempt >= r.maxRetries {
			return fmt.Errorf("retries exhausted after %d attempt(s): %w", attempt+1, err)
		}
	}
}

// runOnce executes the DDL under monitoring once.
func (r *MonitoredRunner) runOnce(ctx context.Context, op ddl.Operation, sql string) error {
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	done := make(chan error, 1)
	go func() { done <- r.exec.ExecDDL(execCtx, sql) }()

	sampleCtx, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	samples := make(chan Sample)
	go r.pump(sampleCtx, samples)

	caps := r.capsFor(op)
	err := supervise(ctx, r.clk, r.exec, op, caps, r.blockingTimeout, samples, done)
	if !errors.Is(err, ErrCancelled) {
		return err
	}

	// Pressure cancel: stop the Go side, then KILL if it does not stop in time.
	cancelExec()
	select {
	case <-done:
		// stopped on its own
	case <-time.After(r.killGrace):
		_ = r.exec.Kill(context.Background(), r.exec.SPID())
		<-done
	}
	return ErrCancelled
}

// pump samples the server at the poll interval and forwards snapshots.
func (r *MonitoredRunner) pump(ctx context.Context, samples chan<- Sample) {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s, err := r.sampler.Sample(ctx)
			if err != nil {
				continue
			}
			select {
			case samples <- s:
			case <-ctx.Done():
				return
			}
		}
	}
}
