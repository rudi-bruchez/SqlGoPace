package run

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// repinExec is an execution connection that repairs itself before running the
// statement, the way *mssql.Conn does when the statement before it left the pinned
// connection unusable. The SPID changes because the repaired connection is a new
// server session.
type repinExec struct {
	mu   sync.Mutex
	spid int
}

func (e *repinExec) SPID() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.spid
}

func (e *repinExec) ExecDDL(context.Context, string) error {
	e.mu.Lock()
	e.spid = 88
	e.mu.Unlock()
	return nil
}

func (e *repinExec) Kill(context.Context, int) error { return nil }

// quietSampler reports a server under no pressure at all.
type quietSampler struct{}

func (quietSampler) Blocking(context.Context, IgnoredSessions) (BlockState, error) {
	return BlockState{}, nil
}
func (quietSampler) Log(context.Context) (LogSample, error) { return LogSample{}, nil }

// A repaired connection is a different server session, and the run carries on under
// it. Leaving that silent makes the report lie to anyone correlating it with a DMV
// capture, and hides that the operation before it broke the connection.
func TestRunStatementNarratesARePinnedConnection(t *testing.T) {
	exec := &repinExec{spid: 57}
	r := NewMonitoredRunner(exec, quietSampler{}, NewManualClock(testStart), RunnerConfig{
		PollInterval:    time.Millisecond,
		BlockingTimeout: time.Minute,
		KillGrace:       time.Minute,
	})

	var events []ReactionEvent
	action, err := r.runStatement(context.Background(), "ALTER INDEX [PK_A] ON [dbo].[A] REBUILD;", Capabilities{}, func(e ReactionEvent) {
		events = append(events, e)
	})
	if action != Continue || err != nil {
		t.Fatalf("runStatement() = (%v, %v), want (Continue, nil)", action, err)
	}

	var got string
	for _, e := range events {
		if strings.Contains(e.Detail, "re-pinned") {
			got = e.Kind + ": " + e.Detail
		}
	}
	if got == "" {
		t.Fatalf("no event reported the re-pinned connection; got %+v", events)
	}
	if !strings.Contains(got, "57") || !strings.Contains(got, "88") {
		t.Errorf("re-pin event = %q, want it to name both the old (57) and the new (88) session", got)
	}
}
