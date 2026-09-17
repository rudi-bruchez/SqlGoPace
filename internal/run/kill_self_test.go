package run

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// unstoppableExec ignores the attention: its statement returns only when release is
// closed, never when the context is canceled. That is the case the grace period and the
// fallback KILL exist for.
type unstoppableExec struct {
	release  chan struct{}
	mu       sync.Mutex
	kills    int
	declined bool
}

func (e *unstoppableExec) SPID() int { return 57 }

func (e *unstoppableExec) ExecDDL(context.Context, string) error {
	<-e.release
	return nil
}

func (e *unstoppableExec) KillSelf(context.Context) error {
	e.mu.Lock()
	e.kills++
	declined := e.declined
	e.mu.Unlock()
	if declined {
		return fmt.Errorf("%w for SPID 57: the session id now belongs to a different session (login_time 2026-09-17T09:15:00, expected 2026-09-17T08:00:00)", mssql.ErrKillDeclined)
	}
	return nil
}

func (e *unstoppableExec) killCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.kills
}

// A declined fallback KILL is not a failed one, and must not read like it. The session the
// runner was going to kill is no longer provably ours, so nothing is killed; the statement
// is still out there, so the runner keeps waiting for it; and the operator is told both
// facts, because an operation that stops reacting while a statement runs on is exactly
// what they would otherwise have to infer from silence.
func TestRunnerKeepsWaitingWhenTheFallbackKillIsDeclined(t *testing.T) {
	exec := &unstoppableExec{release: make(chan struct{}), declined: true}
	r := NewMonitoredRunner(exec, blockedSampler{}, NewManualClock(testStart), RunnerConfig{
		PollInterval:    time.Millisecond,
		BlockingTimeout: 0,
		KillGrace:       5 * time.Millisecond,
		MaxRetries:      0,
	})

	var mu sync.Mutex
	var events []ReactionEvent
	sink := func(e ReactionEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }

	done := make(chan error, 1)
	go func() {
		done <- r.Run(context.Background(), ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "PK_MEASUREMENT"},
			"ALTER INDEX PK_MEASUREMENT ON dbo.MEASUREMENT REBUILD", Capabilities{}, sink)
	}()

	// The declined kill must not end the wait: Run stays blocked on the statement.
	select {
	case err := <-done:
		t.Fatalf("Run() returned %v while the statement was still running; a declined kill must not abandon it", err)
	case <-time.After(200 * time.Millisecond):
	}

	mu.Lock()
	var declined ReactionEvent
	for _, e := range events {
		if strings.Contains(e.Detail, "declined") {
			declined = e
		}
	}
	mu.Unlock()
	if declined.Detail == "" {
		t.Fatalf("no event reporting the declined kill: %+v", events)
	}
	if declined.Kind != "warn" {
		t.Errorf("declined kill reported as %q, want warn", declined.Kind)
	}
	if !strings.Contains(declined.Detail, "login_time") {
		t.Errorf("declined kill detail = %q, want the reason the session could not be confirmed", declined.Detail)
	}
	if !strings.Contains(declined.Detail, "still running") {
		t.Errorf("declined kill detail = %q, want it to say the statement is still running", declined.Detail)
	}

	close(exec.release)
	<-done

	if n := exec.killCount(); n != 1 {
		t.Errorf("KillSelf called %d time(s), want exactly 1 — a declined kill must not be retried in a loop", n)
	}
}
