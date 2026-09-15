package run

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// logOverCapSampler reports the transaction log over cap on every poll, and no
// blocking pressure at all. It is used to prove pumpSamples reacts to log pressure
// before its first log_poll tick (H1, docs/specs/REVIEW-2026-09-15-harm.md): the test
// sets LogPollInterval far longer than the test itself, so a log sample reaching the
// supervisor can only come from the immediate sample pumpSamples must take at start.
type logOverCapSampler struct{}

func (logOverCapSampler) Blocking(context.Context, IgnoredSessions) (BlockState, error) {
	return BlockState{}, nil
}

func (logOverCapSampler) Log(context.Context) (LogSample, error) {
	return LogSample{OverCap: true, ReuseWait: "LOG_BACKUP"}, nil
}

// TestRunReactsToLogPressureBeforeFirstLogPollTick pins H1: a statement started while
// the log is already over cap must be canceled promptly, not only after the first
// log_poll_seconds tick. Before the fix, pumpSamples starts with cur.LogOverCap false
// and only reads the log on the first tick of a ticker set to an hour here, so the
// statement would run for the whole test timeout instead of being canceled.
func TestRunReactsToLogPressureBeforeFirstLogPollTick(t *testing.T) {
	exec := &alwaysCancelExec{}
	r := NewMonitoredRunner(exec, logOverCapSampler{}, NewManualClock(testStart), RunnerConfig{
		PollInterval:    time.Millisecond,
		LogPollInterval: time.Hour, // far longer than the test: only an immediate sample can win
		BlockingTimeout: time.Minute,
		KillGrace:       time.Minute,
		MaxRetries:      0,
	})
	op := ddl.RebuildHeap{Schema: "dbo", Table: "T"}

	var mu sync.Mutex
	var reactions []ReactionEvent
	sink := func(ev ReactionEvent) {
		mu.Lock()
		reactions = append(reactions, ev)
		mu.Unlock()
	}

	done := make(chan error, 1)
	go func() {
		done <- r.Run(context.Background(), op, "ALTER TABLE [dbo].[T] REBUILD;", Capabilities{}, sink)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("Run() error = %v, want it to wrap ErrCancelled", err)
		}
	case <-time.After(awaitTimeout):
		t.Fatal("statement was not canceled promptly — the pump waited for the first log_poll tick instead of sampling the log immediately")
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, ev := range reactions {
		if ev.Kind == "cancel" {
			found = true
		}
	}
	if !found {
		t.Errorf("no cancel reaction recorded; got %+v", reactions)
	}
}
