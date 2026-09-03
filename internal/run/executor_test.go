package run

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	action   Action
	pressure Pressure
	err      error
}

// awaitTimeout bounds every rendezvous with a goroutine under test. A stuck
// goroutine then fails its own test in seconds, instead of running out the
// package's ten-minute timeout and burying the cause in a process-wide dump.
const awaitTimeout = 5 * time.Second

// observedClock is a ManualClock that reports every Now(), so a test can advance
// it only once the goroutine under test has taken its timer baseline from it.
// The sample channel does not order those two: its rendezvous returns as soon as
// the sample is handed over, while supervise and waitForRelief read the clock
// afterwards. An advance that lands first makes the baseline the advanced time,
// so the deadline can never elapse and the goroutine blocks on the next sample
// for good.
type observedClock struct {
	*ManualClock
	lateBaseline time.Duration // widens that window, so a test does not pass on scheduler luck
	reads        chan struct{}
}

func newObservedClock() *observedClock {
	return &observedClock{ManualClock: NewManualClock(testStart), reads: make(chan struct{}, 1)}
}

func (c *observedClock) Now() time.Time {
	if c.lateBaseline > 0 {
		time.Sleep(c.lateBaseline)
	}
	now := c.ManualClock.Now()
	select {
	case c.reads <- struct{}{}:
	default: // only the first read is an ordering point; later ones need no room
	}
	return now
}

// awaitBaseline blocks until the goroutine under test has read the clock. Only
// then may the test move it.
func (c *observedClock) awaitBaseline(t *testing.T) {
	t.Helper()
	select {
	case <-c.reads:
	case <-time.After(awaitTimeout):
		t.Fatal("the goroutine under test never read the clock")
	}
}

// sendSample hands a monitoring sample to the goroutine under test.
func sendSample(t *testing.T, samples chan Sample, s Sample) {
	t.Helper()
	select {
	case samples <- s:
	case <-time.After(awaitTimeout):
		t.Fatalf("no goroutine took sample %+v", s)
	}
}

// sendDone reports the statement's completion to the goroutine under test.
func sendDone(t *testing.T, done chan error, err error) {
	t.Helper()
	select {
	case done <- err:
	case <-time.After(awaitTimeout):
		t.Fatalf("no goroutine took done = %v", err)
	}
}

// awaitSupervise returns supervise's result.
func awaitSupervise(t *testing.T, out <-chan superviseResult) superviseResult {
	t.Helper()
	select {
	case res := <-out:
		return res
	case <-time.After(awaitTimeout):
		t.Fatal("supervise never returned")
		return superviseResult{}
	}
}

// awaitRelief returns waitForRelief's result.
func awaitRelief(t *testing.T, out <-chan error) error {
	t.Helper()
	select {
	case err := <-out:
		return err
	case <-time.After(awaitTimeout):
		t.Fatal("waitForRelief never returned")
		return nil
	}
}

// runSupervise starts supervise in a goroutine and returns its result channel.
func runSupervise(clk Clock, caps Capabilities, samples chan Sample, done chan error) <-chan superviseResult {
	out := make(chan superviseResult, 1)
	go func() {
		a, p, e := supervise(context.Background(), clk, caps, 60*time.Second, samples, done)
		out <- superviseResult{a, p, e}
	}()
	return out
}

func TestSuperviseContinuesUntilDone(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	out := runSupervise(NewManualClock(testStart), Capabilities{}, samples, done)

	sendSample(t, samples, Sample{}) // no pressure
	sendDone(t, done, nil)

	got := awaitSupervise(t, out)
	if got.action != Continue || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Continue, nil)", got.action, got.err)
	}
}

func TestSuperviseCancelOnSustainedBlocking(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	clk := newObservedClock()
	out := runSupervise(clk, Capabilities{}, samples, done) // not resumable

	sendSample(t, samples, Sample{BlockingOthers: true}) // starts the blocking timer
	clk.awaitBaseline(t)
	clk.Advance(90 * time.Second)
	sendSample(t, samples, Sample{BlockingOthers: true}) // 90s >= 60s, not resumable -> cancel

	got := awaitSupervise(t, out)
	if got.action != Cancel || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Cancel, nil)", got.action, got.err)
	}
}

func TestSupervisePauseOnSustainedBlockingResumable(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	clk := newObservedClock()
	out := runSupervise(clk, Capabilities{Resumable: true}, samples, done)

	sendSample(t, samples, Sample{BlockingOthers: true})
	clk.awaitBaseline(t)
	clk.Advance(90 * time.Second)
	sendSample(t, samples, Sample{BlockingOthers: true}) // pressure + resumable -> pause

	got := awaitSupervise(t, out)
	if got.action != Pause || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Pause, nil)", got.action, got.err)
	}
}

func TestSuperviseIgnoreBlockingHoldsLock(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	clk := newObservedClock()
	out := runSupervise(clk, Capabilities{IgnoreBlocking: true}, samples, done)

	sendSample(t, samples, Sample{BlockingOthers: true}) // starts the (suppressed) blocking timer
	clk.awaitBaseline(t)
	clk.Advance(90 * time.Second)
	sendSample(t, samples, Sample{BlockingOthers: true}) // well past timeout, but ignore_blocking -> no reaction
	sendDone(t, done, nil)                               // the statement finishes on its own

	got := awaitSupervise(t, out)
	if got.action != Continue || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Continue, nil) — ignore_blocking holds the lock", got.action, got.err)
	}
}

func TestSuperviseMaxBlockCapYieldsThroughIgnored(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	clk := newObservedClock()
	// ignore_blocking would hold the lock forever, but max_block caps it.
	out := runSupervise(clk, Capabilities{IgnoreBlocking: true, MaxBlock: 5 * time.Minute}, samples, done)

	sendSample(t, samples, Sample{Blocking: true}) // blocking only an ignored session (no BlockingOthers)
	clk.awaitBaseline(t)
	clk.Advance(6 * time.Minute)
	sendSample(t, samples, Sample{Blocking: true}) // 6m >= 5m cap -> yield even though it is ignored

	got := awaitSupervise(t, out)
	if got.action != Cancel || !got.pressure.Capped {
		t.Errorf("supervise() = (%v, capped=%v), want (Cancel, capped=true)", got.action, got.pressure.Capped)
	}
}

func TestSuperviseMaxBlockCapNotReachedHoldsLock(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	clk := newObservedClock()
	out := runSupervise(clk, Capabilities{IgnoreBlocking: true, MaxBlock: 30 * time.Minute}, samples, done)

	sendSample(t, samples, Sample{Blocking: true})
	clk.awaitBaseline(t)
	clk.Advance(5 * time.Minute)
	sendSample(t, samples, Sample{Blocking: true}) // 5m < 30m cap -> still holding
	sendDone(t, done, nil)                         // statement finishes on its own

	got := awaitSupervise(t, out)
	if got.action != Continue || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Continue, nil) — under the cap", got.action, got.err)
	}
}

func TestSuperviseIgnoreBlockingStillHonorsLog(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	clk := newObservedClock()
	out := runSupervise(clk, Capabilities{IgnoreBlocking: true}, samples, done) // not resumable

	// Blocking is ignored, but transaction-log pressure must still stop the op.
	sendSample(t, samples, Sample{BlockingOthers: true, LogOverCap: true})

	got := awaitSupervise(t, out)
	if got.action != Cancel || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Cancel, nil) — log pressure is still honored", got.action, got.err)
	}
}

func TestSuperviseContextCancel(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	out := make(chan superviseResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		a, p, e := supervise(ctx, NewManualClock(testStart), Capabilities{}, 60*time.Second, samples, done)
		out <- superviseResult{a, p, e}
	}()

	cancel()
	got := awaitSupervise(t, out)
	if got.action != Continue || !errors.Is(got.err, context.Canceled) {
		t.Errorf("supervise() = (%v, %v), want (Continue, context.Canceled)", got.action, got.err)
	}
}

func TestSuperviseStopPausesResumable(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	draining := func() bool { return true } // a graceful stop is requested
	out := runSupervise(NewManualClock(testStart), Capabilities{Resumable: true, Stop: draining}, samples, done)

	sendSample(t, samples, Sample{}) // the stop is observed on a monitoring poll (so a cancel could precede it)
	got := awaitSupervise(t, out)
	if got.action != Stop || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Stop, nil)", got.action, got.err)
	}
}

func TestSuperviseStopIgnoredWhenNotResumable(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	draining := func() bool { return true }
	out := runSupervise(NewManualClock(testStart), Capabilities{Stop: draining}, samples, done)

	// A non-resumable operation cannot be paused, so the stop is ignored (even across a
	// poll) and the op runs to completion (the drain stops the run at the next boundary).
	sendSample(t, samples, Sample{})
	sendDone(t, done, nil)
	got := awaitSupervise(t, out)
	if got.action != Continue || got.err != nil {
		t.Errorf("supervise() = (%v, %v), want (Continue, nil) — stop ignored for a non-resumable op", got.action, got.err)
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
	reissued  int
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

// reissued counts paced re-issues; reissueSQL is the paced closure passed to runLoop
// (returns the original statement, mirroring how a reorg resumes from persisted progress).
func (s *stmtRun) reissueSQL() (string, error) { s.reissued++; return "REORGANIZE", nil }

func TestRunLoopCompletes(t *testing.T) {
	s := &stmtRun{actions: []Action{Continue}, errs: []error{nil}}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL, nil); err != nil {
		t.Fatalf("runLoop() = %v, want nil", err)
	}
	if len(s.ran) != 1 || s.ran[0] != "REBUILD" {
		t.Errorf("ran = %v, want [REBUILD]", s.ran)
	}
}

func TestRunLoopReturnsStatementError(t *testing.T) {
	boom := errors.New("real failure")
	s := &stmtRun{actions: []Action{Continue}, errs: []error{boom}}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL, nil); !errors.Is(err, boom) {
		t.Errorf("runLoop() = %v, want %v", err, boom)
	}
}

func TestRunLoopCancelIsRetryable(t *testing.T) {
	s := &stmtRun{actions: []Action{Cancel}, errs: []error{nil}}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL, nil); !errors.Is(err, ErrCancelled) {
		t.Errorf("runLoop() = %v, want ErrCancelled", err)
	}
	if s.relief != 0 {
		t.Errorf("waitForRelief called %d times on cancel, want 0", s.relief)
	}
}

func TestRunLoopStopReturnsErrStopped(t *testing.T) {
	// A graceful stop pauses the statement and returns without resuming.
	s := &stmtRun{actions: []Action{Stop}, errs: []error{nil}}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL, nil); !errors.Is(err, ErrStopped) {
		t.Errorf("runLoop() = %v, want ErrStopped", err)
	}
	if s.relief != 0 {
		t.Errorf("waitForRelief called %d times on stop, want 0 (no resume)", s.relief)
	}
}

func TestRunLoopPauseThenResumeToCompletion(t *testing.T) {
	// First statement pauses, second (the RESUME) completes.
	s := &stmtRun{actions: []Action{Pause, Continue}, errs: []error{nil, nil}}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL, nil); err != nil {
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
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL, nil); err != nil {
		t.Fatalf("runLoop() = %v, want nil", err)
	}
	if s.relief != 2 {
		t.Errorf("waitForRelief called %d times, want 2", s.relief)
	}
}

func TestRunLoopReliefErrorPropagates(t *testing.T) {
	boom := errors.New("ctx canceled while waiting")
	s := &stmtRun{actions: []Action{Pause}, errs: []error{nil}, reliefErr: boom}
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL, nil); !errors.Is(err, boom) {
		t.Errorf("runLoop() = %v, want %v", err, boom)
	}
}

func TestRunLoopPacedReissuesUntilComplete(t *testing.T) {
	// Cancel under pressure, then complete: the paced branch waits for relief and
	// re-issues the SAME statement (not a RESUME).
	s := &stmtRun{actions: []Action{Cancel, Continue}, errs: []error{nil, nil}}
	if err := runLoop("REORGANIZE", s.runStatement, s.waitForRelief, s.resumeSQL, s.reissueSQL); err != nil {
		t.Fatalf("runLoop() = %v, want nil", err)
	}
	if len(s.ran) != 2 || s.ran[0] != "REORGANIZE" || s.ran[1] != "REORGANIZE" {
		t.Errorf("ran = %v, want [REORGANIZE REORGANIZE] (re-issue the original, not a RESUME)", s.ran)
	}
	if s.relief != 1 {
		t.Errorf("waitForRelief called %d times, want 1", s.relief)
	}
	if s.reissued != 1 {
		t.Errorf("reissue called %d times, want 1", s.reissued)
	}
}

func TestRunLoopPacedLoopsUnbounded(t *testing.T) {
	// Many cancels then complete: the paced loop keeps re-issuing and never returns
	// ErrCancelled. Run only retries (and counts toward MaxRetries) when runOnce returns
	// ErrCancelled, so this runLoop-level property is exactly what makes a paced reorg
	// immune to MaxRetries.
	s := &stmtRun{
		actions: []Action{Cancel, Cancel, Cancel, Continue},
		errs:    []error{nil, nil, nil, nil},
	}
	if err := runLoop("REORGANIZE", s.runStatement, s.waitForRelief, s.resumeSQL, s.reissueSQL); err != nil {
		t.Fatalf("runLoop() = %v, want nil (unbounded re-issue, never ErrCancelled)", err)
	}
	if s.relief != 3 || s.reissued != 3 {
		t.Errorf("relief=%d reissued=%d, want 3 and 3", s.relief, s.reissued)
	}
}

func TestRunLoopPacedReliefErrorPropagates(t *testing.T) {
	// If relief cannot be reached (e.g. log-drain timeout), the paced branch returns
	// that error instead of looping forever.
	boom := errors.New("log did not drain")
	s := &stmtRun{actions: []Action{Cancel}, errs: []error{nil}, reliefErr: boom}
	if err := runLoop("REORGANIZE", s.runStatement, s.waitForRelief, s.resumeSQL, s.reissueSQL); !errors.Is(err, boom) {
		t.Errorf("runLoop() = %v, want %v", err, boom)
	}
	if s.reissued != 0 {
		t.Errorf("reissue called %d times, want 0 (relief failed first)", s.reissued)
	}
}

func TestRunLoopPacedStopWins(t *testing.T) {
	// A graceful stop after a paced re-issue ends the loop with ErrStopped.
	s := &stmtRun{actions: []Action{Cancel, Stop}, errs: []error{nil, nil}}
	if err := runLoop("REORGANIZE", s.runStatement, s.waitForRelief, s.resumeSQL, s.reissueSQL); !errors.Is(err, ErrStopped) {
		t.Errorf("runLoop() = %v, want ErrStopped", err)
	}
	if s.reissued != 1 {
		t.Errorf("reissue called %d times, want 1", s.reissued)
	}
}

func TestReissueForOnlyReorganize(t *testing.T) {
	var events []ReactionEvent
	sink := func(ev ReactionEvent) { events = append(events, ev) }

	// reorganize_index → non-nil closure that narrates and returns the original SQL.
	f := reissueFor(ddl.ReorganizeIndex{Schema: "dbo", Table: "T", Index: "IX"}, "REORG SQL", sink)
	if f == nil {
		t.Fatal("reissueFor(ReorganizeIndex) = nil, want non-nil")
	}
	got, err := f()
	if err != nil || got != "REORG SQL" {
		t.Errorf("reissue() = (%q, %v), want (\"REORG SQL\", nil)", got, err)
	}
	if len(events) != 1 || events[0].Kind != "resume" || !strings.Contains(events[0].Detail, "re-issuing REORGANIZE") {
		t.Errorf("emitted %+v, want one resume event mentioning re-issuing REORGANIZE", events)
	}

	// Other cancel-safe ops and a non-cancel-safe op → nil (not paced).
	for _, op := range []ddl.Operation{
		ddl.CheckDB{Database: "DB"},
		ddl.UpdateStatistics{Schema: "dbo", Table: "T"},
		ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"},
	} {
		if reissueFor(op, "SQL", sink) != nil {
			t.Errorf("reissueFor(%T) != nil, want nil (only reorganize_index is paced)", op)
		}
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
		got, err := NewServerSampler(probe, staticSession(57), 1000, 80).Blocking(context.Background(), nil)
		if err != nil || !got.Unignored || !got.Any {
			t.Errorf("Blocking() = (%+v, %v), want {Any:true Unignored:true}", got, err)
		}
	})
	t.Run("blocked by someone else", func(t *testing.T) {
		probe := fakeProbe{sessions: []mssql.Session{{SPID: 60, BlockingSPID: 99}}}
		got, err := NewServerSampler(probe, staticSession(57), 1000, 80).Blocking(context.Background(), nil)
		if err != nil || got.Any || got.Unignored {
			t.Errorf("Blocking() = (%+v, %v), want all false", got, err)
		}
	})
	t.Run("ignored blocker counts toward Any but not Unignored", func(t *testing.T) {
		probe := fakeProbe{sessions: []mssql.Session{{SPID: 60, BlockingSPID: 57, Program: "ReportingService"}}}
		ignore, err := CompileIgnoredSessions([]ddl.IgnoredSession{{AppName: "Reporting.*"}})
		if err != nil {
			t.Fatalf("CompileIgnoredSessions() error = %v", err)
		}
		got, err := NewServerSampler(probe, staticSession(57), 1000, 80).Blocking(context.Background(), ignore)
		if err != nil || got.Unignored || !got.Any {
			t.Errorf("Blocking() = (%+v, %v), want {Any:true Unignored:false} — held through an ignored session", got, err)
		}
	})
	t.Run("non-ignored blocker counts as Unignored", func(t *testing.T) {
		probe := fakeProbe{sessions: []mssql.Session{
			{SPID: 60, BlockingSPID: 57, Program: "ReportingService"},
			{SPID: 61, BlockingSPID: 57, Program: "CriticalApp"},
		}}
		ignore, err := CompileIgnoredSessions([]ddl.IgnoredSession{{AppName: "Reporting.*"}})
		if err != nil {
			t.Fatalf("CompileIgnoredSessions() error = %v", err)
		}
		got, err := NewServerSampler(probe, staticSession(57), 1000, 80).Blocking(context.Background(), ignore)
		if err != nil || !got.Unignored {
			t.Errorf("Blocking() = (%+v, %v), want Unignored:true — a non-ignored session is blocked", got, err)
		}
	})
}

func TestServerSamplerLog(t *testing.T) {
	t.Run("over percent cap reads reuse wait", func(t *testing.T) {
		probe := fakeProbe{ls: mssql.LogSpace{TotalBytes: 100, UsedPercent: 90}, reuseWait: "ACTIVE_TRANSACTION"}
		got, err := NewServerSampler(probe, staticSession(57), 1000, 80).Log(context.Background())
		if err != nil {
			t.Fatalf("Log() error = %v", err)
		}
		if !got.OverCap || got.ReuseWait != "ACTIVE_TRANSACTION" {
			t.Errorf("Log() = %+v, want OverCap=true ReuseWait=ACTIVE_TRANSACTION", got)
		}
	})
	t.Run("healthy leaves reuse wait unread", func(t *testing.T) {
		probe := fakeProbe{ls: mssql.LogSpace{TotalBytes: 100, UsedPercent: 10}, reuseWait: "ACTIVE_TRANSACTION"}
		got, err := NewServerSampler(probe, staticSession(57), 1000, 80).Log(context.Background())
		if err != nil {
			t.Fatalf("Log() error = %v", err)
		}
		if got.OverCap || got.ReuseWait != "" {
			t.Errorf("Log() = %+v, want OverCap=false ReuseWait empty (not queried)", got)
		}
	})
}

func TestServerSamplerSuppressesPendingVictim(t *testing.T) {
	snap := amplifierSnapshot(16)
	probe := fakeProbe{sessions: snap} // the existing fake in this file; a value type, not a pointer
	sampler := NewServerSampler(probe, staticSession(67), 1<<40, 100)

	clk := NewManualClock(time.Unix(1_800_000_000, 0))
	killer := NewVictimKiller(
		func(context.Context, int) error { return nil },
		nil, nil, clk, mssql.AppNamePrefix,
	)
	killer.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 60 * time.Second})
	sampler.SetVictimKiller(killer)

	st, err := sampler.Blocking(context.Background(), nil)
	if err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	if !st.Any {
		t.Error("BlockState.Any = false, want true — a pending victim still counts toward max_block")
	}
	if st.Unignored {
		t.Error("BlockState.Unignored = true for a pending kill, want false — the yield timer must not fire")
	}
}

func TestServerSamplerCountsVictimWhenKillFails(t *testing.T) {
	snap := amplifierSnapshot(16)
	probe := &fakeProbe{sessions: snap}
	sampler := NewServerSampler(probe, staticSession(67), 1<<40, 100)

	clk := NewManualClock(time.Unix(1_800_000_000, 0))
	killer := NewVictimKiller(
		func(context.Context, int) error { return errors.New("permission denied") },
		nil, nil, clk, mssql.AppNamePrefix,
	)
	killer.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 60 * time.Second})
	sampler.SetVictimKiller(killer)

	if _, err := sampler.Blocking(context.Background(), nil); err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	clk.Advance(61 * time.Second)
	st, err := sampler.Blocking(context.Background(), nil)
	if err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	if !st.Unignored {
		t.Error("BlockState.Unignored = false after a failed KILL, want true — we must fall back to yielding")
	}
}

func TestServerSamplerWithoutVictimKillerIsUnchanged(t *testing.T) {
	probe := fakeProbe{sessions: amplifierSnapshot(16)}
	sampler := NewServerSampler(probe, staticSession(67), 1<<40, 100)

	st, err := sampler.Blocking(context.Background(), nil)
	if err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	if !st.Any || !st.Unignored {
		t.Errorf("BlockState = %+v, want both true — without a killer the behavior is today's", st)
	}
}

// A suppressed victim must not mask an ordinary application session we are also
// blocking: the yield timer still has to fire for that one. This is what the dropped
// `break` in the rewritten loop buys, so it needs an explicit assertion.
func TestServerSamplerSuppressionDoesNotMaskAnotherBlockedSession(t *testing.T) {
	snap := append(amplifierSnapshot(16),
		mssql.Session{SPID: 500, Command: "SELECT", BlockingSPID: 67, Login: "app", Program: "PayrollApp"})
	probe := fakeProbe{sessions: snap}
	sampler := NewServerSampler(probe, staticSession(67), 1<<40, 100)

	clk := NewManualClock(time.Unix(1_800_000_000, 0))
	killer := NewVictimKiller(
		func(context.Context, int) error { return nil },
		nil, nil, clk, mssql.AppNamePrefix,
	)
	killer.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 60 * time.Second})
	sampler.SetVictimKiller(killer)

	st, err := sampler.Blocking(context.Background(), nil)
	if err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	if !st.Unignored {
		t.Error("BlockState.Unignored = false, want true — SPID 500 is an ordinary blocked " +
			"session and must still drive the yield even while the amplifier kill is pending")
	}
}

// Spec §1.6: with both killers armed on a snapshot showing a mutual block, exactly one
// KILL must be issued, whichever order they are consulted in. VictimKiller declines its
// own direct blocker, so BlockerKiller owns it.
func TestSamplerMutualBlockIssuesExactlyOneKill(t *testing.T) {
	snap := amplifierSnapshot(16)
	snap[0].BlockingSPID = 79 // 79 blocks us and we block 79
	probe := fakeProbe{sessions: snap}
	sampler := NewServerSampler(probe, staticSession(67), 1<<40, 100)

	var killed []int
	clk := NewManualClock(time.Unix(1_800_000_000, 0))

	victims := NewVictimKiller(
		func(_ context.Context, spid int) error { killed = append(killed, spid); return nil },
		nil, nil, clk, mssql.AppNamePrefix,
	)
	victims.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 0})
	sampler.SetVictimKiller(victims)

	blockers := NewBlockerKiller(
		func(_ context.Context, spid int) error { killed = append(killed, spid); return nil },
		nil, clk.Now)
	blockers.SetSource(staticKill{rules: []killRule{{match: sessionRule{sessionID: 79}, after: 0}}})
	sampler.SetKiller(blockers)

	if _, err := sampler.Blocking(context.Background(), nil); err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	if len(killed) != 1 || killed[0] != 79 {
		t.Errorf("killed = %v, want exactly one KILL of SPID 79 — the two killers must be disjoint", killed)
	}
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

	sendSample(t, samples, Sample{LogOverCap: true}) // still under pressure
	sendSample(t, samples, Sample{})                 // cleared
	if err := awaitRelief(t, out); err != nil {
		t.Errorf("waitForRelief() = %v, want nil", err)
	}
}

func TestWaitForReliefDrainTimeout(t *testing.T) {
	samples := make(chan Sample)
	clk := newObservedClock()
	// Take the baseline late on purpose: the abort must fire because the advance
	// below waits for it, not because the scheduler happened to get there first.
	clk.lateBaseline = 50 * time.Millisecond
	var events []ReactionEvent
	out := runRelief(clk, 30*time.Minute, samples, func(ev ReactionEvent) { events = append(events, ev) })

	sendSample(t, samples, Sample{LogOverCap: true, LogReuseWait: "LOG_BACKUP"}) // starts the drain timer
	clk.awaitBaseline(t)
	clk.Advance(31 * time.Minute)
	sendSample(t, samples, Sample{LogOverCap: true, LogReuseWait: "LOG_BACKUP"}) // 31m >= 30m -> abort

	err := awaitRelief(t, out)
	if !errors.Is(err, ErrLogDrainTimeout) {
		t.Errorf("waitForRelief() = %v, want ErrLogDrainTimeout", err)
	}
	if len(events) != 1 || events[0].Kind != "abort" {
		t.Errorf("events = %+v, want one abort event", events)
	}
}

// recurringBlockerProbe is a sampleProbe whose ActiveSessions returns a different blocker
// SPID on each call — one connection-pool retry or Agent job restart per poll.
type recurringBlockerProbe struct {
	calls    int
	blockers []int
}

func (p *recurringBlockerProbe) LogSpace(context.Context) (mssql.LogSpace, error) {
	return mssql.LogSpace{}, nil
}

func (p *recurringBlockerProbe) LogReuseWait(context.Context) (string, error) { return "NOTHING", nil }

func (p *recurringBlockerProbe) ActiveSessions(context.Context) ([]mssql.Session, error) {
	spid := p.blockers[min(p.calls, len(p.blockers)-1)]
	p.calls++
	return []mssql.Session{
		{SPID: 100, BlockingSPID: spid, WaitType: "LCK_M_SCH_M"},
		{SPID: spid, Login: "SVC_RPT", Host: "BATCH01", Program: "Reporting"},
	}, nil
}

// The killers are consulted from ServerSampler.Blocking, which the shrink driver reaches
// through pumpSamples in both runChunk and runWatchedStatement. From a killer's point of
// view a chunk boundary is indistinguishable from any other gap between polls: it only ever
// sees a sequence of Blocking calls with time between them. Driving ServerSampler directly
// with a probe that returns a different blocker on each call therefore reproduces the shrink
// scenario on the real production path, without the shrink driver's execution fakes.
func TestSamplerKillsARecurringBlockerAcrossPollGaps(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := NewBlockerKiller(rec.kill, nil, func() time.Time { return now })
	compiled, err := compileKilledSessions([]ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)}, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	k.SetSource(staticKill{rules: compiled})

	probe := &recurringBlockerProbe{blockers: []int{104, 104, 155}}
	s := NewServerSampler(probe, staticSession(100), 0, 0)
	s.SetKiller(k)

	// Poll 1 and 2 are the first chunk: the blocker serves its dwell and is killed.
	if _, err := s.Blocking(context.Background(), nil); err != nil {
		t.Fatalf("Blocking: %v", err)
	}
	now = now.Add(60 * time.Second)
	if _, err := s.Blocking(context.Background(), nil); err != nil {
		t.Fatalf("Blocking: %v", err)
	}
	if len(rec.spids) != 1 || rec.spids[0] != 104 {
		t.Fatalf("expected SPID 104 killed after its dwell, got %v", rec.spids)
	}

	// The chunk ends; nothing is sampled for two minutes; the next chunk's first poll finds
	// the offender back under a new SPID. It must not buy a fresh 60 seconds.
	now = now.Add(2 * time.Minute)
	if _, err := s.Blocking(context.Background(), nil); err != nil {
		t.Fatalf("Blocking: %v", err)
	}
	if len(rec.spids) != 2 || rec.spids[1] != 155 {
		t.Fatalf("a blocker returning after a chunk gap must be killed at once, got %v", rec.spids)
	}
}

// movingSession is an execution connection whose session id changes, as it does
// when the pinned connection is re-pinned after a cancel poisoned it.
type movingSession struct {
	mu   sync.Mutex
	spid int
}

func (s *movingSession) SPID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spid
}

func (s *movingSession) set(spid int) {
	s.mu.Lock()
	s.spid = spid
	s.mu.Unlock()
}

// A repaired execution connection is a new server session. A sampler holding the id
// captured at startup would see nothing blocked by us and report no blocking at all,
// silently disabling every reaction for the rest of the run.
func TestServerSamplerFollowsTheExecutionSessionID(t *testing.T) {
	sess := &movingSession{spid: 57}
	probe := fakeProbe{sessions: []mssql.Session{{SPID: 60, BlockingSPID: 88}}}
	sampler := NewServerSampler(probe, sess, 1000, 80)

	got, err := sampler.Blocking(context.Background(), nil)
	if err != nil || got.Any {
		t.Fatalf("Blocking() = (%+v, %v), want no blocking: 88 is not our session yet", got, err)
	}

	sess.set(88) // the execution connection was re-pinned onto session 88

	got, err = sampler.Blocking(context.Background(), nil)
	if err != nil || !got.Any || !got.Unignored {
		t.Errorf("Blocking() = (%+v, %v), want {Any:true Unignored:true} — the sampler must follow the re-pinned session", got, err)
	}
}

// staticSession is an execution connection whose session id never changes.
type staticSession int

func (s staticSession) SPID() int { return int(s) }
