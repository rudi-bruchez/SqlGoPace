package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// alwaysCancelExec never lets its statement finish on its own: ExecDDL only returns
// when its context is canceled, which is what the runner does to stop a statement it
// decided to cancel. So every attempt Run makes is canceled.
type alwaysCancelExec struct {
	mu    sync.Mutex
	calls int
}

func (e *alwaysCancelExec) SPID() int { return 1 }

func (e *alwaysCancelExec) ExecDDL(ctx context.Context, sql string) error {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (e *alwaysCancelExec) KillSelf(context.Context) error { return nil }

func (e *alwaysCancelExec) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// blockedSampler reports our DDL blocking a session not allowed to stay blocked on
// every poll, so with BlockingTimeout 0 supervise decides Cancel on the first sample.
type blockedSampler struct{}

func (blockedSampler) Blocking(context.Context, IgnoredSessions) (BlockState, error) {
	return BlockState{Any: true, Unignored: true}, nil
}
func (blockedSampler) Log(context.Context) (LogSample, error) { return LogSample{}, nil }

// TestRunRetriesImmediatelyThenErrorsAfterMaxRetries pins MonitoredRunner.Run's
// bounded retry (docs/specs/CANCEL-ONLY.md Testing: "no unit test today"; this design
// deliberately keeps the retry as-is). A non-resumable, always-canceled operation is
// retried with no wait between attempts, and Run gives up after MaxRetries+1 attempts.
func TestRunRetriesImmediatelyThenErrorsAfterMaxRetries(t *testing.T) {
	exec := &alwaysCancelExec{}
	r := NewMonitoredRunner(exec, blockedSampler{}, NewManualClock(testStart), RunnerConfig{
		PollInterval:    time.Millisecond,
		BlockingTimeout: 0,
		KillGrace:       time.Minute,
		MaxRetries:      2,
	})
	op := ddl.RebuildHeap{Schema: "dbo", Table: "T"}

	var cancels int
	var mu sync.Mutex
	sink := func(ev ReactionEvent) {
		if ev.Kind == "cancel" {
			mu.Lock()
			cancels++
			mu.Unlock()
		}
	}

	start := time.Now()
	err := r.Run(context.Background(), op, "ALTER TABLE [dbo].[T] REBUILD;", Capabilities{}, sink)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("Run() error = %v, want it to wrap ErrCancelled", err)
	}
	if !strings.Contains(err.Error(), "3 attempt(s)") {
		t.Errorf("Run() error = %q, want it to name 3 attempts (MaxRetries=2 -> 3 tries)", err)
	}
	if calls := exec.callCount(); calls != 3 {
		t.Errorf("ExecDDL called %d times, want 3 (MaxRetries=2 -> 3 attempts)", calls)
	}
	mu.Lock()
	gotCancels := cancels
	mu.Unlock()
	if gotCancels != 3 {
		t.Errorf("cancel reactions = %d, want 3 (one per attempt)", gotCancels)
	}
	// Retries immediately: Run has no wait between attempts (only waitForRelief, which
	// this path never reaches — reissue is nil for a non-cancel-safe op). Generous
	// bound: this is a property test, not a timing benchmark.
	if elapsed > 2*time.Second {
		t.Errorf("Run() took %s across 3 immediate retries, want well under 2s", elapsed)
	}
}
