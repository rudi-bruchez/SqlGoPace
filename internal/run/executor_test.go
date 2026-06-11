package run

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestReactionEvent(t *testing.T) {
	p := Pressure{BlockingOthers: true}

	if ev := reactionEvent(Pause, p, Capabilities{Resumable: true}); ev.Kind != "pause" || ev.Detail != p.Detail() {
		t.Errorf("pause event = %+v, want kind=pause detail=%q", ev, p.Detail())
	}

	plain := reactionEvent(Cancel, p, Capabilities{})
	if plain.Kind != "cancel" || plain.Detail != p.Detail() {
		t.Errorf("cancel event = %+v, want kind=cancel with no rollback note", plain)
	}
	if strings.Contains(plain.Detail, "incremental") {
		t.Errorf("non-cancel-safe cancel must not claim incremental: %q", plain.Detail)
	}

	safe := reactionEvent(Cancel, p, Capabilities{CancelSafe: true})
	if safe.Kind != "cancel" || !strings.Contains(safe.Detail, "incremental") {
		t.Errorf("cancel-safe cancel = %+v, want a clean-stop note", safe)
	}
}

func TestCancelSafe(t *testing.T) {
	safe := []ddl.Operation{
		ddl.ReorganizeIndex{Schema: "dbo", Table: "T", Index: "IX"},
		ddl.CheckDB{Database: "MYDB"},
		ddl.UpdateStatistics{Schema: "dbo", Table: "T"},
	}
	for _, op := range safe {
		if !cancelSafe(op) {
			t.Errorf("cancelSafe(%s) = false, want true", op.CommandType())
		}
	}
	unsafe := []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"},
		ddl.RebuildHeap{Schema: "dbo", Table: "T"},
		ddl.CreateIndex{Schema: "dbo", Table: "T", Index: "IX", Columns: []string{"C"}},
	}
	for _, op := range unsafe {
		if cancelSafe(op) {
			t.Errorf("cancelSafe(%s) = true, want false (heavy builder, rollback on cancel)", op.CommandType())
		}
	}
}

var testStart = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

type superviseResult struct {
	action Action
	err    error
}

// runSupervise starts supervise in a goroutine and returns its result channel.
func runSupervise(clk Clock, caps Capabilities, samples chan Sample, done chan error) <-chan superviseResult {
	out := make(chan superviseResult, 1)
	go func() {
		a, _, e := supervise(context.Background(), clk, caps, 60*time.Second, samples, done)
		out <- superviseResult{a, e}
	}()
	return out
}

func TestSuperviseContinuesUntilDone(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	out := runSupervise(NewManualClock(testStart), Capabilities{}, samples, done)

	samples <- Sample{} // no pressure
	done <- nil

	got := <-out
	if got.action != Continue || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Continue, nil)", got.action, got.err)
	}
}

func TestSuperviseCancelOnSustainedBlocking(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	clk := NewManualClock(testStart)
	out := runSupervise(clk, Capabilities{}, samples, done) // not resumable

	samples <- Sample{BlockingOthers: true} // starts the blocking timer
	clk.Advance(90 * time.Second)
	samples <- Sample{BlockingOthers: true} // 90s >= 60s, not resumable -> cancel

	got := <-out
	if got.action != Cancel || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Cancel, nil)", got.action, got.err)
	}
}

func TestSupervisePauseOnSustainedBlockingResumable(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	clk := NewManualClock(testStart)
	out := runSupervise(clk, Capabilities{Resumable: true}, samples, done)

	samples <- Sample{BlockingOthers: true}
	clk.Advance(90 * time.Second)
	samples <- Sample{BlockingOthers: true} // pressure + resumable -> pause

	got := <-out
	if got.action != Pause || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Pause, nil)", got.action, got.err)
	}
}

func TestSuperviseContextCancel(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	out := make(chan superviseResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		a, _, e := supervise(ctx, NewManualClock(testStart), Capabilities{}, 60*time.Second, samples, done)
		out <- superviseResult{a, e}
	}()

	cancel()
	got := <-out
	if got.action != Continue || !errors.Is(got.err, context.Canceled) {
		t.Errorf("supervise() = (%v, %v), want (Continue, context.Canceled)", got.action, got.err)
	}
}

// stmtRun records the statements runLoop ran and replays a scripted sequence of
// reactions, one per statement.
type stmtRun struct {
	ran       []string
	actions   []Action
	errs      []error
	relief    int
	reliefErr error
}

func (s *stmtRun) runStatement(stmt string) (Action, error) {
	i := len(s.ran)
	s.ran = append(s.ran, stmt)
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return s.actions[i], err
}

func (s *stmtRun) waitForRelief() error { s.relief++; return s.reliefErr }

func (s *stmtRun) resumeSQL() (string, error) { return "ALTER INDEX [IX] ON [dbo].[T] RESUME;", nil }

func TestRunLoopCompletes(t *testing.T) {
	s := &stmtRun{actions: []Action{Continue}, errs: []error{nil}}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL); err != nil {
		t.Fatalf("runLoop() = %v, want nil", err)
	}
	if len(s.ran) != 1 || s.ran[0] != "REBUILD" {
		t.Errorf("ran = %v, want [REBUILD]", s.ran)
	}
}

func TestRunLoopReturnsStatementError(t *testing.T) {
	boom := errors.New("real failure")
	s := &stmtRun{actions: []Action{Continue}, errs: []error{boom}}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL); !errors.Is(err, boom) {
		t.Errorf("runLoop() = %v, want %v", err, boom)
	}
}

func TestRunLoopCancelIsRetryable(t *testing.T) {
	s := &stmtRun{actions: []Action{Cancel}, errs: []error{nil}}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL); !errors.Is(err, ErrCancelled) {
		t.Errorf("runLoop() = %v, want ErrCancelled", err)
	}
	if s.relief != 0 {
		t.Errorf("waitForRelief called %d times on cancel, want 0", s.relief)
	}
}

func TestRunLoopPauseThenResumeToCompletion(t *testing.T) {
	// First statement pauses, second (the RESUME) completes.
	s := &stmtRun{actions: []Action{Pause, Continue}, errs: []error{nil, nil}}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL); err != nil {
		t.Fatalf("runLoop() = %v, want nil", err)
	}
	if len(s.ran) != 2 {
		t.Fatalf("ran %d statements, want 2", len(s.ran))
	}
	if s.ran[0] != "REBUILD" || s.ran[1] != "ALTER INDEX [IX] ON [dbo].[T] RESUME;" {
		t.Errorf("ran = %v, want [REBUILD, RESUME]", s.ran)
	}
	if s.relief != 1 {
		t.Errorf("waitForRelief called %d times, want 1", s.relief)
	}
}

func TestRunLoopPausesRepeatedly(t *testing.T) {
	// Pause, resume, pause again, then complete.
	s := &stmtRun{actions: []Action{Pause, Pause, Continue}, errs: []error{nil, nil, nil}}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL); err != nil {
		t.Fatalf("runLoop() = %v, want nil", err)
	}
	if s.relief != 2 {
		t.Errorf("waitForRelief called %d times, want 2", s.relief)
	}
}

func TestRunLoopReliefErrorPropagates(t *testing.T) {
	boom := errors.New("ctx cancelled while waiting")
	s := &stmtRun{actions: []Action{Pause}, errs: []error{nil}, reliefErr: boom}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL); !errors.Is(err, boom) {
		t.Errorf("runLoop() = %v, want %v", err, boom)
	}
}

func TestProbeResumableConclusive(t *testing.T) {
	clk := NewManualClock(testStart)
	paused := probeResumable(func() (bool, error) { return true, nil }, clk, time.Minute, func() {})
	if !paused.conclusive || !paused.paused {
		t.Errorf("probeResumable(paused) = %+v, want conclusive paused", paused)
	}
	notPaused := probeResumable(func() (bool, error) { return false, nil }, clk, time.Minute, func() {})
	if !notPaused.conclusive || notPaused.paused {
		t.Errorf("probeResumable(not paused) = %+v, want conclusive not-paused", notPaused)
	}
}

func TestProbeResumableRecoversAfterRetries(t *testing.T) {
	clk := NewManualClock(testStart)
	calls := 0
	check := func() (bool, error) {
		calls++
		if calls < 3 {
			return false, errors.New("server unreachable")
		}
		return true, nil // server is back; the op is paused
	}
	res := probeResumable(check, clk, time.Minute, func() { clk.Advance(time.Second) })
	if !res.conclusive || !res.paused {
		t.Errorf("probeResumable = %+v, want conclusive paused after reconnect", res)
	}
	if calls != 3 {
		t.Errorf("check called %d times, want 3", calls)
	}
}

func TestProbeResumableInconclusiveOnTimeout(t *testing.T) {
	clk := NewManualClock(testStart)
	check := func() (bool, error) { return false, errors.New("server still down") }
	res := probeResumable(check, clk, 3*time.Second, func() { clk.Advance(time.Second) })
	if res.conclusive {
		t.Errorf("probeResumable = %+v, want inconclusive (server never came back)", res)
	}
}

type fakeProbe struct {
	ls        mssql.LogSpace
	reuseWait string
	sessions  []mssql.Session
}

func (f fakeProbe) LogSpace(context.Context) (mssql.LogSpace, error) { return f.ls, nil }
func (f fakeProbe) LogReuseWait(context.Context) (string, error)     { return f.reuseWait, nil }
func (f fakeProbe) ActiveSessions(context.Context) ([]mssql.Session, error) {
	return f.sessions, nil
}

func TestServerSamplerBlocking(t *testing.T) {
	t.Run("blocked by our spid", func(t *testing.T) {
		probe := fakeProbe{sessions: []mssql.Session{{SPID: 60, BlockingSPID: 57}}}
		got, err := NewServerSampler(probe, 57, 1000, 80).Blocking(context.Background())
		if err != nil || !got {
			t.Errorf("Blocking() = (%v, %v), want (true, nil)", got, err)
		}
	})
	t.Run("blocked by someone else", func(t *testing.T) {
		probe := fakeProbe{sessions: []mssql.Session{{SPID: 60, BlockingSPID: 99}}}
		got, err := NewServerSampler(probe, 57, 1000, 80).Blocking(context.Background())
		if err != nil || got {
			t.Errorf("Blocking() = (%v, %v), want (false, nil)", got, err)
		}
	})
}

func TestServerSamplerLog(t *testing.T) {
	t.Run("over percent cap reads reuse wait", func(t *testing.T) {
		probe := fakeProbe{ls: mssql.LogSpace{TotalBytes: 100, UsedPercent: 90}, reuseWait: "ACTIVE_TRANSACTION"}
		got, err := NewServerSampler(probe, 57, 1000, 80).Log(context.Background())
		if err != nil {
			t.Fatalf("Log() error = %v", err)
		}
		if !got.OverCap || got.ReuseWait != "ACTIVE_TRANSACTION" {
			t.Errorf("Log() = %+v, want OverCap=true ReuseWait=ACTIVE_TRANSACTION", got)
		}
	})
	t.Run("healthy leaves reuse wait unread", func(t *testing.T) {
		probe := fakeProbe{ls: mssql.LogSpace{TotalBytes: 100, UsedPercent: 10}, reuseWait: "ACTIVE_TRANSACTION"}
		got, err := NewServerSampler(probe, 57, 1000, 80).Log(context.Background())
		if err != nil {
			t.Fatalf("Log() error = %v", err)
		}
		if got.OverCap || got.ReuseWait != "" {
			t.Errorf("Log() = %+v, want OverCap=false ReuseWait empty (not queried)", got)
		}
	})
}

// runRelief starts waitForRelief in a goroutine and returns its result channel.
func runRelief(clk Clock, drain time.Duration, samples chan Sample, sink ReactionSink) <-chan error {
	out := make(chan error, 1)
	go func() { out <- waitForRelief(context.Background(), clk, drain, samples, sink) }()
	return out
}

func TestWaitForReliefReturnsWhenClear(t *testing.T) {
	samples := make(chan Sample)
	out := runRelief(NewManualClock(testStart), time.Hour, samples, func(ReactionEvent) {})

	samples <- Sample{LogOverCap: true} // still under pressure
	samples <- Sample{}                 // cleared
	if err := <-out; err != nil {
		t.Errorf("waitForRelief() = %v, want nil", err)
	}
}

func TestWaitForReliefDrainTimeout(t *testing.T) {
	samples := make(chan Sample)
	clk := NewManualClock(testStart)
	var events []ReactionEvent
	out := runRelief(clk, 30*time.Minute, samples, func(ev ReactionEvent) { events = append(events, ev) })

	samples <- Sample{LogOverCap: true, LogReuseWait: "LOG_BACKUP"} // starts the drain timer
	clk.Advance(31 * time.Minute)
	samples <- Sample{LogOverCap: true, LogReuseWait: "LOG_BACKUP"} // 31m >= 30m -> abort

	err := <-out
	if !errors.Is(err, ErrLogDrainTimeout) {
		t.Errorf("waitForRelief() = %v, want ErrLogDrainTimeout", err)
	}
	if len(events) != 1 || events[0].Kind != "abort" {
		t.Errorf("events = %+v, want one abort event", events)
	}
}
