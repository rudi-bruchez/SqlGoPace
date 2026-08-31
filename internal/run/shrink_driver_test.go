package run

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	mssqldb "github.com/microsoft/go-mssqldb"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// fakeServer models just enough of a SQL Server for the shrink driver tests: a
// single file whose size moves toward a chunk's target when ExecDDL runs it, plus
// scripted log-reuse state. It implements both Executor and ShrinkReader.
type fakeServer struct {
	fileType string // mssql.FileTypeRows | mssql.FileTypeLog
	name     string
	sizeMB   int
	usedMB   int

	truncateToMB  *int  // size after a TRUNCATEONLY pass, if set
	noProgress    bool  // chunk moves never change the size (simulate 49516 / data at end)
	floorMB       int   // a chunk cannot shrink below this (also the active-log floor)
	blockTruncate bool  // model a long-running TRUNCATEONLY: block until the context is canceled
	chunkErr      error // if set, every page-moving chunk returns this error (a shrink that cannot move a page)

	// chunkErrs scripts transient chunk errors (e.g. Msg 845): each page-moving chunk exec
	// pops and returns the front error instead of its normal behavior, until the queue is
	// empty, then chunks proceed normally. Mirrors the reuse queue below.
	chunkErrs []error

	recovery string   // recovery_model_desc for LogReuse
	reuse    []string // log_reuse_wait_desc returned per LogReuse call (last value repeats)

	waits []mssql.SessionWait // returned by SessionWaits (constant across calls)

	sessions []mssql.Session // returned by ActiveSessions (constant across calls)

	// sampledOnce, if non-nil, is closed the first time ActiveSessions is called, so a test can
	// hold a chunk open until the in-flight self-block sampler has read.
	sampledOnce chan struct{}

	percentComplete float64 // dm_exec_requests percent_complete for the running chunk (0 = no active request)

	tail      mssql.TailObject // scripted FindTailObject result
	tailFound bool
	tailCalls int

	execLog  []string
	killed   bool
	reuseIdx int

	onExec func(sql string) // test hook, called at the start of each ExecDDL
}

func (s *fakeServer) SPID() int { return 99 }

func (s *fakeServer) ExecDDL(ctx context.Context, sql string) error {
	if s.onExec != nil {
		s.onExec(sql)
	}
	s.execLog = append(s.execLog, sql)
	switch {
	case strings.Contains(sql, "TRUNCATEONLY"):
		if s.blockTruncate {
			<-ctx.Done() // a long TRUNCATEONLY that only ends when the driver cancels it
			return ctx.Err()
		}
		if s.truncateToMB != nil && *s.truncateToMB < s.sizeMB {
			s.sizeMB = *s.truncateToMB
		}
	case strings.Contains(sql, "CHECKPOINT"):
		// no size change
	case strings.Contains(sql, "DBCC SHRINKFILE"):
		if len(s.chunkErrs) > 0 {
			err := s.chunkErrs[0]
			s.chunkErrs = s.chunkErrs[1:]
			return err
		}
		if s.chunkErr != nil {
			return s.chunkErr
		}
		if s.noProgress {
			return nil
		}
		target := max(parseChunkTarget(sql), s.floorMB)
		if target < s.sizeMB {
			s.sizeMB = target
		}
	}
	return nil
}

func (s *fakeServer) Kill(_ context.Context, _ int) error { s.killed = true; return nil }

func (s *fakeServer) FileSpace(_ context.Context, fileType string) ([]mssql.FileSpace, error) {
	if fileType != s.fileType {
		return nil, nil
	}
	return []mssql.FileSpace{{
		Name: s.name, TypeDesc: fileType,
		SizeMB: s.sizeMB, UsedMB: s.usedMB, FreeMB: s.sizeMB - s.usedMB,
	}}, nil
}

func (s *fakeServer) FileSizeMB(_ context.Context, _ string) (int, error) { return s.sizeMB, nil }

func (s *fakeServer) LogReuse(_ context.Context) (string, string, error) {
	r := "NOTHING"
	if len(s.reuse) > 0 {
		if s.reuseIdx < len(s.reuse) {
			r = s.reuse[s.reuseIdx]
		} else {
			r = s.reuse[len(s.reuse)-1]
		}
	}
	s.reuseIdx++
	return s.recovery, r, nil
}

func (s *fakeServer) ActiveLogFloorMB(_ context.Context) (int, error) { return s.floorMB, nil }

func (s *fakeServer) SessionWaits(_ context.Context, _ int) ([]mssql.SessionWait, error) {
	return s.waits, nil
}

func (s *fakeServer) ActiveSessions(context.Context) ([]mssql.Session, error) {
	if s.sampledOnce != nil {
		select {
		case <-s.sampledOnce:
		default:
			close(s.sampledOnce)
		}
	}
	return s.sessions, nil
}

func (s *fakeServer) Progress(_ context.Context, _ int) (mssql.Progress, bool, error) {
	if s.percentComplete <= 0 {
		return mssql.Progress{}, false, nil
	}
	return mssql.Progress{PercentComplete: s.percentComplete, Command: "DbccFilesCompact"}, true, nil
}

func (s *fakeServer) FindTailObject(_ context.Context, _, _ int) (mssql.TailObject, bool, error) {
	s.tailCalls++
	return s.tail, s.tailFound, nil
}

// signalSampler is noPressureSampler that also signals every blocking poll, so a test can
// assert which phases of the driver actually sample the server.
type signalSampler struct {
	noPressureSampler
	polled chan struct{}
}

func (s *signalSampler) Blocking(context.Context, IgnoredSessions) (BlockState, error) {
	select {
	case s.polled <- struct{}{}:
	default:
	}
	return BlockState{}, nil
}

// noPressureSampler never reports blocking or log pressure.
type noPressureSampler struct{}

func (noPressureSampler) Blocking(context.Context, IgnoredSessions) (BlockState, error) {
	return BlockState{}, nil
}
func (noPressureSampler) Log(context.Context) (LogSample, error) { return LogSample{}, nil }

// parseChunkTarget extracts the integer target from a chunk statement of the form
// "DBCC SHRINKFILE (N'file', 800) WITH ...".
func parseChunkTarget(sql string) int {
	open := strings.Index(sql, "(")
	close := strings.Index(sql, ")")
	if open < 0 || close < 0 || close < open {
		return 0
	}
	parts := strings.Split(sql[open+1:close], ",")
	n, _ := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1]))
	return n
}

func testTuning() ShrinkTuning {
	return ShrinkTuning{
		InitialStepSmallMB:   100,
		InitialStepMediumMB:  250,
		InitialStepLargeMB:   500,
		MinStepMB:            50,
		MaxStepMB:            1024,
		TargetBatch:          5 * time.Second,
		MaxNoProgress:        3,
		NoProgressBackoff:    1 * time.Second,
		NoProgressBackoffMax: 8 * time.Second,
		SelfWaitTimeout:      10 * time.Minute,
		LogReuseWaitTimeout:  3 * time.Minute,
	}
}

// newTestRunner wires a ShrinkRunner against the fake server with large poll
// intervals (so the pump never fires during these instant chunks — only the done
// channel resolves) and a fake waiter that advances the manual clock.
func newTestRunner(s *fakeServer, clk *ManualClock) *ShrinkRunner {
	r := NewShrinkRunner(s, s, noPressureSampler{}, clk, ShrinkRunnerConfig{
		Tuning:          testTuning(),
		PollInterval:    time.Hour,
		LogPollInterval: time.Hour,
		BlockingTimeout: time.Minute,
		LogDrainTimeout: time.Minute,
		KillGrace:       time.Second,
	})
	r.wait = func(_ context.Context, d time.Duration) error { clk.Advance(d); return nil }
	return r
}

func discard(ReactionEvent) {}

func TestShrinkStopsBetweenChunks(t *testing.T) {
	drain := &DrainFlag{}
	s := &fakeServer{fileType: mssql.FileTypeRows, name: "Data", sizeMB: 2000, usedMB: 200}
	// Request the graceful stop when the first chunk runs, so the loop finishes that chunk
	// and stops before the next one.
	s.onExec = func(sql string) {
		if strings.Contains(sql, "DBCC SHRINKFILE") && !strings.Contains(sql, "TRUNCATEONLY") {
			drain.Request()
		}
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.stop = drain.Draining

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"} // small target → many chunks
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("Run() error = %v, want ErrStopped", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1 (the partial file)", len(res))
	}
	if res[0].Chunks != 1 {
		t.Errorf("Chunks = %d, want 1 (the current chunk finished, the next did not start)", res[0].Chunks)
	}
	if !strings.Contains(res[0].Reason, "graceful stop") {
		t.Errorf("Reason = %q, want it to mention the graceful stop", res[0].Reason)
	}
}

func TestShrinkTruncateOnlyStopsOnDrain(t *testing.T) {
	drain := &DrainFlag{}
	// A big file whose TRUNCATEONLY runs long: the fake blocks until the driver cancels it.
	s := &fakeServer{fileType: mssql.FileTypeRows, name: "Data", sizeMB: 8_000_000, usedMB: 200, blockTruncate: true}
	// The operator requests the graceful stop while the TRUNCATEONLY is running.
	s.onExec = func(sql string) {
		if strings.Contains(sql, "TRUNCATEONLY") {
			drain.Request()
		}
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.pollIntv = time.Millisecond // poll the stop flag promptly instead of the 1h default
	r.stop = drain.Draining

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("Run() error = %v, want ErrStopped (graceful stop during TRUNCATEONLY)", err)
	}
	if len(res) != 1 || res[0].Chunks != 0 {
		t.Fatalf("got %+v, want one partial result with 0 chunks (stopped before Phase B)", res)
	}
	if !strings.Contains(res[0].Reason, "graceful stop") {
		t.Errorf("Reason = %q, want it to mention the graceful stop", res[0].Reason)
	}
	// It must have canceled the running statement, not issued any page-moving chunk.
	for _, stmt := range s.execLog {
		if strings.Contains(stmt, "DBCC SHRINKFILE") && !strings.Contains(stmt, "TRUNCATEONLY") {
			t.Errorf("no page-moving chunk should run after a stop in Phase A; got %q", stmt)
		}
	}
}

// TestShrinkEmitsPercentCompleteDuringTruncateOnly verifies the driver polls SQL Server's
// percent_complete during the TRUNCATEONLY pass too, not only during the page-moving chunks.
// On a large file that pass runs for a long time, and the console would otherwise sit on a
// frozen line with no completion at all. The fake holds the TRUNCATEONLY open until the test
// requests a graceful stop, so the progress pump has time to fire.
func TestShrinkEmitsPercentCompleteDuringTruncateOnly(t *testing.T) {
	drain := &DrainFlag{}
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 8_000_000, usedMB: 200, blockTruncate: true, percentComplete: 17,
	}
	seen := make(chan ShrinkProgress, 1)
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.pollIntv = 2 * time.Millisecond // fast enough that the pump fires during the held pass
	r.stop = drain.Draining
	r.progress = func(p ShrinkProgress) {
		if p.PercentComplete > 0 && strings.Contains(p.Statement, "TRUNCATEONLY") {
			select {
			case seen <- p:
			default:
			}
		}
	}

	done := make(chan struct{})
	go func() {
		op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
		_, _ = r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
		close(done)
	}()

	select {
	case p := <-seen:
		if p.PercentComplete != 17 {
			t.Errorf("emitted PercentComplete = %v, want 17", p.PercentComplete)
		}
		if p.File != "Data" || p.CurrentMB != 8_000_000 {
			t.Errorf("progress lost its base fields: got %+v", p)
		}
	case <-time.After(2 * time.Second):
		drain.Request()
		<-done
		t.Fatal("no TRUNCATEONLY progress carrying server percent_complete within 2s")
	}
	drain.Request() // cancel the held TRUNCATEONLY and let the run finish
	<-done
}

// TestShrinkPollsBlockingDuringTruncateOnly verifies the driver samples the server during
// the TRUNCATEONLY pass, not only during the page-moving chunks. Both killers hang off
// ServerSampler.Blocking (BlockerKiller for sessions blocking us, VictimKiller for the
// amplifying victims we block), so a phase that never polls can never kill anything. The
// fake holds the pass open until the test drains, so any poll observed here is necessarily
// Phase A: no chunk has run yet.
func TestShrinkPollsBlockingDuringTruncateOnly(t *testing.T) {
	drain := &DrainFlag{}
	s := &fakeServer{fileType: mssql.FileTypeRows, name: "Data", sizeMB: 8_000_000, usedMB: 200, blockTruncate: true}
	sampler := &signalSampler{polled: make(chan struct{}, 1)}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.pollIntv = 2 * time.Millisecond
	r.sampler = sampler
	r.stop = drain.Draining

	done := make(chan struct{})
	go func() {
		op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
		_, _ = r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
		close(done)
	}()

	select {
	case <-sampler.polled:
	case <-time.After(2 * time.Second):
		drain.Request()
		<-done
		t.Fatal("no blocking poll during the TRUNCATEONLY pass within 2s — killers cannot run")
	}
	drain.Request() // cancel the held TRUNCATEONLY and let the run finish
	<-done
}

// TestShrinkLogMonitorsItsStatement verifies the log shrink runs its DBCC SHRINKFILE under
// the same monitoring as the other phases: the blocking poll runs (the killers hang off it)
// and the server's percent_complete reaches the console. The log statement is a single
// unchunked shrink to a VLF boundary, and it used to run completely dark. The fake holds it
// open until the test releases it.
func TestShrinkLogMonitorsItsStatement(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeLog, name: "Log", recovery: "SIMPLE",
		sizeMB: 4000, usedMB: 400, floorMB: 400, percentComplete: 23,
	}
	release := make(chan struct{})
	s.onExec = func(sql string) {
		if strings.Contains(sql, "DBCC SHRINKFILE") {
			<-release // hold the log shrink open so the pumps have time to fire
		}
	}
	sampler := &signalSampler{polled: make(chan struct{}, 1)}
	seen := make(chan ShrinkProgress, 1)
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.pollIntv = 2 * time.Millisecond
	r.sampler = sampler
	r.progress = func(p ShrinkProgress) {
		if p.PercentComplete > 0 {
			select {
			case seen <- p:
			default:
			}
		}
	}

	done := make(chan struct{})
	go func() {
		op := ddl.Shrink{Type: "log", Files: "Log", TargetFreeSpace: "10%"}
		_, _ = r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
		close(done)
	}()

	fail := func(msg string) {
		t.Helper()
		close(release)
		<-done
		t.Fatal(msg)
	}
	select {
	case <-sampler.polled:
	case <-time.After(2 * time.Second):
		fail("no blocking poll during the log shrink within 2s — killers cannot run")
	}
	select {
	case p := <-seen:
		if p.Type != "log" || p.File != "Log" {
			t.Errorf("progress = %+v, want the log file's own progress", p)
		}
		if p.PercentComplete != 23 {
			t.Errorf("emitted PercentComplete = %v, want 23", p.PercentComplete)
		}
	case <-time.After(2 * time.Second):
		fail("no log-shrink progress carrying server percent_complete within 2s")
	}
	close(release) // let the log shrink (and the run) finish
	<-done
}

func TestShrinkDataNoOp(t *testing.T) {
	s := &fakeServer{fileType: mssql.FileTypeRows, name: "Data", sizeMB: 1000, usedMB: 1000} // no free space
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(res) != 1 || !res[0].NoOp {
		t.Fatalf("got %+v, want a single no-op result", res)
	}
	for _, stmt := range s.execLog {
		if strings.Contains(stmt, "DBCC SHRINKFILE") {
			t.Errorf("no-op should not execute any DBCC SHRINKFILE; got %q", stmt)
		}
	}
}

func TestShrinkDataTruncateOnlyEnough(t *testing.T) {
	trunc := 500
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, truncateToMB: &trunc, floorMB: 400,
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))

	// target free 10% of used(400) => final 440; truncate to 500 > 440 still needs chunks,
	// so make truncate land at/below final by using a larger free target.
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "100%"} // final = 800
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res[0].FinalMB != 500 || res[0].Chunks != 0 {
		t.Fatalf("got final=%d chunks=%d, want final=500 chunks=0 (truncate-only sufficed)", res[0].FinalMB, res[0].Chunks)
	}
}

func TestShrinkDataConverges(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, // no truncate effect
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"} // final = 440
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res[0].FinalMB != 440 {
		t.Errorf("FinalMB = %d, want 440", res[0].FinalMB)
	}
	if res[0].Chunks == 0 {
		t.Errorf("Chunks = 0, want > 0 (converged via chunk loop)")
	}
	if s.sizeMB != 440 {
		t.Errorf("server size = %d, want 440", s.sizeMB)
	}
}

// TestShrinkEmitsServerPercentComplete verifies the driver re-emits progress with SQL
// Server's own percent_complete (dm_exec_requests) while a chunk runs. A short poll interval
// drives the progress pump, and the chunk statement blocks until the test has observed one
// such emit, so the assertion is deterministic rather than timing-dependent.
func TestShrinkEmitsServerPercentComplete(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 2000, usedMB: 1800, floorMB: 1800, percentComplete: 42,
	}
	seen := make(chan float64, 1)
	release := make(chan struct{})
	s.onExec = func(sql string) {
		if strings.Contains(sql, "DBCC SHRINKFILE") && !strings.Contains(sql, "TRUNCATEONLY") {
			<-release // hold the chunk open so the progress pump has time to fire
		}
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := NewShrinkRunner(s, s, noPressureSampler{}, clk, ShrinkRunnerConfig{
		Tuning:          testTuning(),
		PollInterval:    2 * time.Millisecond, // fast enough that the pump fires during the held chunk
		LogPollInterval: time.Hour,
		BlockingTimeout: time.Minute,
		LogDrainTimeout: time.Minute,
		KillGrace:       time.Second,
	}, WithShrinkProgress(func(p ShrinkProgress) {
		if p.PercentComplete > 0 {
			select {
			case seen <- p.PercentComplete:
			default:
			}
		}
	}))
	r.wait = func(_ context.Context, d time.Duration) error { clk.Advance(d); return nil }

	done := make(chan struct{})
	go func() {
		op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"} // final = 1980, one chunk
		_, _ = r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
		close(done)
	}()

	select {
	case got := <-seen:
		if got != 42 {
			t.Errorf("emitted PercentComplete = %v, want 42", got)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("no progress carrying server percent_complete within 2s")
	}
	close(release) // let the chunk (and the run) finish
	<-done
}

func TestShrinkDataNoProgressStops(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true, // chunks never shrink
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)

	var pauses int
	sink := func(e ReactionEvent) {
		if e.Kind == "pause" {
			pauses++
		}
	}
	// Even a shrink that never gains space must report progress to the console, so it is
	// visible (target, current size, step) instead of appearing frozen — the regression
	// behind "I don't see the shrink in the TUI".
	var progress []ShrinkProgress
	r.progress = func(p ShrinkProgress) { progress = append(progress, p) }

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res[0].FinalMB != 1000 || res[0].Reason == "" {
		t.Errorf("got final=%d reason=%q, want final=1000 with a stop reason", res[0].FinalMB, res[0].Reason)
	}
	// MaxNoProgress = 3 → stops on the 3rd no-progress chunk, having backed off twice.
	if pauses < 2 {
		t.Errorf("pause events = %d, want >= 2 (no-progress backoff)", pauses)
	}
	if len(progress) == 0 {
		t.Fatal("no ShrinkProgress emitted for a no-progress shrink; the TUI would show no shrink line")
	}
	for _, p := range progress {
		if p.File != "Data" || p.FinalMB != 440 || p.StartMB <= 0 {
			t.Errorf("progress snapshot = %+v, want File=Data FinalMB=440 with a positive StartMB", p)
		}
	}
}

func TestShrinkAbsorbsAllocationError(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400,
		// Msg 3140 is the real "could not adjust the space allocation" error a shrink raises
		// on a page it cannot move (not Msg 5240, which is a different message).
		chunkErr: mssqldb.Error{Number: 3140, Message: "Could not adjust the space allocation for file 'Data'."},
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"} // final 440
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("Msg 3140 must not fail the run, got err = %v", err)
	}
	if len(res) != 1 || res[0].Reason == "" {
		t.Fatalf("got %+v, want a clean stop with a reason (work preserved)", res)
	}
	if !strings.Contains(res[0].Reason, "adjust") {
		t.Errorf("Reason = %q, want it to carry the underlying error text", res[0].Reason)
	}
	if res[0].FinalMB != 1000 { // nothing moved, but the file size is honestly reported
		t.Errorf("FinalMB = %d, want the unchanged 1000", res[0].FinalMB)
	}
}

// TestShrinkAbsorbsUnknownChunkError proves the driver decides by progress, not by matching
// a specific message number: an arbitrary, unrecognized DBCC error is absorbed as a
// no-progress event (bounded stall, work preserved) with its text carried into the reason,
// never a hard failure.
func TestShrinkAbsorbsUnknownChunkError(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400,
		chunkErr: mssqldb.Error{Number: 999999, Message: "some brand-new shrink error"},
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("an unrecognized chunk error must not fail the run, got err = %v", err)
	}
	if len(res) != 1 || !strings.Contains(res[0].Reason, "brand-new shrink error") {
		t.Fatalf("got %+v, want a clean stop whose reason carries the unknown error", res)
	}
}

func TestShrinkLogSimpleCheckpoint(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeLog, name: "Log",
		sizeMB: 800, usedMB: 100, floorMB: 100, recovery: "SIMPLE",
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))

	op := ddl.Shrink{Type: "log", Files: "Log", TargetFreeSpace: "10%"} // final = 110
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res[0].FinalMB != 110 {
		t.Errorf("FinalMB = %d, want 110", res[0].FinalMB)
	}
	var sawCheckpoint bool
	for _, stmt := range s.execLog {
		if strings.Contains(stmt, "CHECKPOINT") {
			sawCheckpoint = true
		}
	}
	if !sawCheckpoint {
		t.Errorf("SIMPLE log shrink should issue a CHECKPOINT; exec log = %v", s.execLog)
	}
}

func TestShrinkLogFullWaitsThenShrinks(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeLog, name: "Log",
		sizeMB: 800, usedMB: 100, floorMB: 100, recovery: "FULL",
		// LOG_BACKUP for the first two reads, then NOTHING once a backup "runs".
		reuse: []string{"LOG_BACKUP", "LOG_BACKUP", "NOTHING"},
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)

	op := ddl.Shrink{Type: "log", Files: "Log", TargetFreeSpace: "10%"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res[0].FinalMB != 110 {
		t.Errorf("FinalMB = %d, want 110 (shrank after the log freed)", res[0].FinalMB)
	}
	for _, stmt := range s.execLog {
		if strings.Contains(stmt, "BACKUP LOG") {
			t.Fatalf("driver must NEVER issue BACKUP LOG; got %q", stmt)
		}
	}
}

func TestShrinkLogFullTimesOutCleanly(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeLog, name: "Log",
		sizeMB: 800, usedMB: 100, floorMB: 100, recovery: "FULL",
		reuse: []string{"LOG_BACKUP"}, // never clears
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)

	var aborts int
	sink := func(e ReactionEvent) {
		if e.Kind == "abort" {
			aborts++
		}
	}
	op := ddl.Shrink{Type: "log", Files: "Log", TargetFreeSpace: "10%"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res[0].FinalMB != 800 || res[0].Reason == "" {
		t.Errorf("got final=%d reason=%q, want unchanged 800 with a clean-abort reason", res[0].FinalMB, res[0].Reason)
	}
	if aborts != 1 {
		t.Errorf("abort events = %d, want exactly 1", aborts)
	}
	for _, stmt := range s.execLog {
		if strings.Contains(stmt, "DBCC SHRINKFILE") || strings.Contains(stmt, "BACKUP LOG") {
			t.Fatalf("timed-out log shrink should not shrink or back up; got %q", stmt)
		}
	}
}

func TestEstimateShrink(t *testing.T) {
	// 400 MB reclaimed over 10 chunks in 100s, 600 MB left: avg 40 MB/chunk → 15 chunks,
	// rate 4 MB/s → 150s with blocking. Of the 100s, 60s was blocked → 40s productive → 10
	// MB/s → 60s without blocking. Wall-clock cadence: 100s / 10 chunks → 10s per chunk.
	if chunks, eta, etaNB, avg := estimateShrink(1000, 600, 0, 10, 100*time.Second, 60*time.Second); chunks != 15 || eta != 150 || etaNB != 60 || avg != 10 {
		t.Errorf("estimateShrink = (%d chunks, %d s, %d s no-block, %d s/chunk), want (15, 150, 60, 10)", chunks, eta, etaNB, avg)
	}
	// No blocking: the two ETAs coincide.
	if _, eta, etaNB, _ := estimateShrink(1000, 600, 0, 10, 100*time.Second, 0); eta != 150 || etaNB != 150 {
		t.Errorf("estimateShrink unblocked = (%d, %d), want both 150", eta, etaNB)
	}
	// No progress yet (no completed chunk): every projection, average included, is zero.
	if chunks, eta, etaNB, avg := estimateShrink(1000, 1000, 0, 0, 0, 0); chunks != 0 || eta != 0 || etaNB != 0 || avg != 0 {
		t.Errorf("estimateShrink with no progress = (%d, %d, %d, %d), want all 0", chunks, eta, etaNB, avg)
	}
	// Nothing left to reclaim: no ETA, but the average survives from the completed chunks
	// (50s / 5 chunks → 10s/chunk) so the last frame still shows the cadence.
	if chunks, eta, _, avg := estimateShrink(1000, 500, 500, 5, 50*time.Second, 0); chunks != 0 || eta != 0 || avg != 10 {
		t.Errorf("estimateShrink at target = (%d, %d, %d s/chunk), want (0, 0, 10)", chunks, eta, avg)
	}
}

func TestShrinkStepAdjustsUnderIOPressure(t *testing.T) {
	// High WRITELOG average between snapshots must halve the step (pure calc path
	// fed by the driver's waitDeltas).
	before := []mssql.SessionWait{{WaitType: "WRITELOG", WaitTimeMS: 0, WaitingTasksCount: 0}}
	after := []mssql.SessionWait{{WaitType: "WRITELOG", WaitTimeMS: 300, WaitingTasksCount: 10}} // 30 ms avg
	d := waitDeltas(before, after)
	if d.WriteLogAvgMs != 30 {
		t.Fatalf("waitDeltas WriteLogAvgMs = %v, want 30", d.WriteLogAvgMs)
	}
	if got := AdjustStepMB(400, time.Second, d, testTuning(), testTuning().MaxStepMB); got != 200 {
		t.Errorf("AdjustStepMB under WRITELOG pressure = %d, want 200", got)
	}
}

// TestTempdbFlushesOnceOnPersistentStall drives chunkLoop directly with a file that never
// gains space (fakeServer.noProgress), so the stall ladder engages every chunk. With
// NoProgressBeforeFlush reached before MaxNoProgress, the tempdb cache flush must fire
// exactly once, and the once-per-run guard must be set so a later stall never repeats it.
func TestTempdbFlushesOnceOnPersistentStall(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "tempdev",
		sizeMB: 1000, usedMB: 500, floorMB: 1000, noProgress: true, // chunks never shrink
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)
	r.tuning.NoProgressBeforeFlush = 2 // reached well before testTuning's MaxNoProgress (3)

	flushed := false
	prof := &TempdbProfile{FlushCaches: true, flushed: &flushed}

	f := mssql.FileSpace{Name: "tempdev", SizeMB: 1000, UsedMB: 500}
	res, err := r.chunkLoop(context.Background(), f, 1000, 500, ddl.ResolvedOptions{}, nil, discard, prof, nil)
	if err != nil {
		t.Fatalf("chunkLoop() error = %v", err)
	}
	if res.Reason == "" {
		t.Errorf("got Reason = %q, want a stop reason (persistent stall)", res.Reason)
	}

	var flushCount int
	for _, stmt := range s.execLog {
		if strings.Contains(stmt, "FREESYSTEMCACHE ('Temporary Tables & Table Variables')") {
			flushCount++
		}
	}
	if flushCount != 1 {
		t.Errorf("flush ran %d times, want exactly 1; exec log = %v", flushCount, s.execLog)
	}
	if !flushed {
		t.Errorf("flushed guard not set")
	}
}

// tempdbFakeServer models tempdb's several data files for RunTempdb tests (unlike
// fakeServer, which models a single file): each named file tracks its own size and
// used space, and ExecDDL dispatches TRUNCATEONLY / DBCC SHRINKFILE by the file name
// literal embedded in the statement, so the two-phase (truncate-all-then-chunk)
// ordering and the per-file clamp can be observed across multiple files.
type tempdbFakeServer struct {
	sizes map[string]int
	used  map[string]int
	order []string // file names, in FileSpace() order

	floorMB map[string]int // per-file chunk floor (defaults to 0 if absent)

	execLog []string
	onExec  func(sql string)
}

func newTempdbFakeServer(files ...mssql.FileSpace) *tempdbFakeServer {
	s := &tempdbFakeServer{sizes: map[string]int{}, used: map[string]int{}, floorMB: map[string]int{}}
	for _, f := range files {
		s.order = append(s.order, f.Name)
		s.sizes[f.Name] = f.SizeMB
		s.used[f.Name] = f.UsedMB
	}
	return s
}

func (s *tempdbFakeServer) SPID() int { return 99 }

// fileNameFromStmt extracts the N'...' file literal from a shrink statement.
func fileNameFromStmt(sql string) string {
	i := strings.Index(sql, "N'")
	if i < 0 {
		return ""
	}
	rest := sql[i+2:]
	j := strings.Index(rest, "'")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func (s *tempdbFakeServer) ExecDDL(_ context.Context, sql string) error {
	if s.onExec != nil {
		s.onExec(sql)
	}
	s.execLog = append(s.execLog, sql)
	name := fileNameFromStmt(sql)
	switch {
	case strings.Contains(sql, "TRUNCATEONLY"):
		// No truncate-only gain modeled here; the chunk loop tests exercise Phase B.
	case strings.Contains(sql, "DBCC SHRINKFILE"):
		target := max(parseChunkTarget(sql), s.floorMB[name])
		if target < s.sizes[name] {
			s.sizes[name] = target
		}
	}
	return nil
}

func (s *tempdbFakeServer) Kill(_ context.Context, _ int) error { return nil }

func (s *tempdbFakeServer) FileSpace(_ context.Context, fileType string) ([]mssql.FileSpace, error) {
	if fileType != mssql.FileTypeRows {
		return nil, nil
	}
	out := make([]mssql.FileSpace, len(s.order))
	for i, name := range s.order {
		out[i] = mssql.FileSpace{Name: name, TypeDesc: fileType, SizeMB: s.sizes[name], UsedMB: s.used[name], FreeMB: s.sizes[name] - s.used[name]}
	}
	return out, nil
}

func (s *tempdbFakeServer) FileSizeMB(_ context.Context, file string) (int, error) {
	return s.sizes[file], nil
}

func (s *tempdbFakeServer) LogReuse(_ context.Context) (string, string, error) {
	return "SIMPLE", "NOTHING", nil
}
func (s *tempdbFakeServer) ActiveLogFloorMB(_ context.Context) (int, error) { return 0, nil }
func (s *tempdbFakeServer) SessionWaits(_ context.Context, _ int) ([]mssql.SessionWait, error) {
	return nil, nil
}
func (s *tempdbFakeServer) ActiveSessions(context.Context) ([]mssql.Session, error) {
	return nil, nil
}
func (s *tempdbFakeServer) Progress(_ context.Context, _ int) (mssql.Progress, bool, error) {
	return mssql.Progress{}, false, nil
}

func (s *tempdbFakeServer) FindTailObject(_ context.Context, _, _ int) (mssql.TailObject, bool, error) {
	return mssql.TailObject{}, false, nil
}

// newTestTempdbRunner wires a ShrinkRunner over a tempdbFakeServer, mirroring
// newTestRunner's large poll intervals and manual-clock wait.
func newTestTempdbRunner(s *tempdbFakeServer, clk *ManualClock) *ShrinkRunner {
	r := NewShrinkRunner(s, s, noPressureSampler{}, clk, ShrinkRunnerConfig{
		Tuning:          testTuning(),
		PollInterval:    time.Hour,
		LogPollInterval: time.Hour,
		BlockingTimeout: time.Minute,
		LogDrainTimeout: time.Minute,
		KillGrace:       time.Second,
	})
	r.wait = func(_ context.Context, d time.Duration) error { clk.Advance(d); return nil }
	return r
}

// assertTruncateOnlyBeforeAnyChunk asserts every TRUNCATEONLY statement in the log
// precedes any page-moving DBCC SHRINKFILE statement, and that there is at least one
// TRUNCATEONLY per expected file count.
func assertTruncateOnlyBeforeAnyChunk(t *testing.T, log []string, wantTruncateOnly int) {
	t.Helper()
	var sawChunk bool
	var truncCount int
	for _, stmt := range log {
		isTrunc := strings.Contains(stmt, "TRUNCATEONLY")
		isChunk := strings.Contains(stmt, "DBCC SHRINKFILE") && !isTrunc
		if isTrunc {
			truncCount++
			if sawChunk {
				t.Fatalf("TRUNCATEONLY ran after a page-moving chunk; exec log = %v", log)
			}
		}
		if isChunk {
			sawChunk = true
		}
	}
	if truncCount != wantTruncateOnly {
		t.Fatalf("got %d TRUNCATEONLY statements, want %d (one per file); exec log = %v", truncCount, wantTruncateOnly, log)
	}
}

func TestRunTempdbTruncatesAllFilesBeforeChunking(t *testing.T) {
	s := newTempdbFakeServer(
		mssql.FileSpace{Name: "tempdev", SizeMB: 1000, UsedMB: 100},
		mssql.FileSpace{Name: "tempdev2", SizeMB: 1000, UsedMB: 100},
	)
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestTempdbRunner(s, clk)

	res, err := r.RunTempdb(context.Background(), ddl.ShrinkTempdb{TargetSizeMB: 200}, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("RunTempdb() error = %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	assertTruncateOnlyBeforeAnyChunk(t, s.execLog, 2)
}

func TestRunTempdbClampsTargetToUsed(t *testing.T) {
	// A single file whose used space (500) exceeds the requested per-file target (200):
	// the chunk target floor must be 500, never below it.
	s := &fakeServer{fileType: mssql.FileTypeRows, name: "tempdev", sizeMB: 1000, usedMB: 500, floorMB: 500}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)

	res, err := r.RunTempdb(context.Background(), ddl.ShrinkTempdb{TargetSizeMB: 200}, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("RunTempdb() error = %v", err)
	}
	if len(res) != 1 || res[0].TargetMB != 500 || res[0].FinalMB != 500 {
		t.Fatalf("got %+v, want a single result clamped to TargetMB=FinalMB=500", res)
	}
	for _, stmt := range s.execLog {
		if strings.Contains(stmt, "DBCC SHRINKFILE") && !strings.Contains(stmt, "TRUNCATEONLY") {
			if target := parseChunkTarget(stmt); target < 500 {
				t.Errorf("chunk targeted %d MB, want >= 500 (used space floor); stmt = %q", target, stmt)
			}
		}
	}
}

// unbalancedWarning reports whether sink recorded the "Unbalanced tempdb files" warning.
func unbalancedWarning(events []ReactionEvent) bool {
	for _, e := range events {
		if e.Kind == "tempdb" && strings.Contains(e.Detail, "Unbalanced tempdb files") {
			return true
		}
	}
	return false
}

func TestRunTempdbWarnsOnUnbalancedFinalSizes(t *testing.T) {
	s := newTempdbFakeServer(
		mssql.FileSpace{Name: "tempdev", SizeMB: 1000, UsedMB: 100},  // final clamps to target 200
		mssql.FileSpace{Name: "tempdev2", SizeMB: 1000, UsedMB: 350}, // final clamps to used 350
	)
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestTempdbRunner(s, clk)

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }

	res, err := r.RunTempdb(context.Background(), ddl.ShrinkTempdb{TargetSizeMB: 200}, ddl.ResolvedOptions{}, nil, sink)
	if err != nil {
		t.Fatalf("RunTempdb() error = %v", err)
	}
	if len(res) != 2 || res[0].FinalMB == res[1].FinalMB {
		t.Fatalf("got %+v, want two results with different FinalMB (unbalanced)", res)
	}
	if !unbalancedWarning(events) {
		t.Errorf("no 'Unbalanced tempdb files' warning emitted; events = %+v", events)
	}
}

func TestRunTempdbNoWarningWhenBalanced(t *testing.T) {
	s := newTempdbFakeServer(
		mssql.FileSpace{Name: "tempdev", SizeMB: 1000, UsedMB: 100},
		mssql.FileSpace{Name: "tempdev2", SizeMB: 1000, UsedMB: 100},
	)
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestTempdbRunner(s, clk)

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }

	res, err := r.RunTempdb(context.Background(), ddl.ShrinkTempdb{TargetSizeMB: 200}, ddl.ResolvedOptions{}, nil, sink)
	if err != nil {
		t.Fatalf("RunTempdb() error = %v", err)
	}
	if len(res) != 2 || res[0].FinalMB != res[1].FinalMB {
		t.Fatalf("got %+v, want two results with equal FinalMB (balanced)", res)
	}
	if unbalancedWarning(events) {
		t.Errorf("unexpected 'Unbalanced tempdb files' warning for balanced files; events = %+v", events)
	}
}

func TestIsShrinkOpCoversBoth(t *testing.T) {
	if !isShrinkOp(ddl.Shrink{}) {
		t.Error("isShrinkOp(ddl.Shrink{}) = false, want true")
	}
	if !isShrinkOp(ddl.ShrinkTempdb{}) {
		t.Error("isShrinkOp(ddl.ShrinkTempdb{}) = false, want true")
	}
	if isShrinkOp(ddl.CheckDB{}) {
		t.Error("isShrinkOp(ddl.CheckDB{}) = true, want false")
	}
}

// TestTempdb845RetriesWithoutFailing scripts a single Msg 845 (buffer latch time-out) on
// the first chunk, then lets subsequent chunks succeed normally. The chunk loop must treat
// it as a non-fatal, retryable no-progress event (the same path any chunk error takes) and
// still converge to the target — with FlushCaches false, proving 845 alone never triggers the flush.
func TestTempdb845RetriesWithoutFailing(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "tempdev",
		sizeMB: 1000, usedMB: 500, floorMB: 500,
		chunkErrs: []error{mssqldb.Error{Number: 845, Message: "Time-out occurred while waiting for buffer latch"}},
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)
	prof := &TempdbProfile{flushed: new(bool)} // FlushCaches false: 845 alone must not flush

	f := mssql.FileSpace{Name: "tempdev", SizeMB: 1000, UsedMB: 500}
	res, err := r.chunkLoop(context.Background(), f, 1000, 500, ddl.ResolvedOptions{}, nil, discard, prof, nil)
	if err != nil {
		t.Fatalf("845 must not be fatal, got err = %v", err)
	}
	if res.FinalMB != 500 {
		t.Errorf("FinalMB = %d, want 500 (converged after retrying past the 845)", res.FinalMB)
	}
	if res.Chunks == 0 {
		t.Errorf("Chunks = 0, want > 0 (progress made after the 845 retry)")
	}
	for _, stmt := range s.execLog {
		if strings.Contains(stmt, "FREESYSTEMCACHE") {
			t.Errorf("flush must not run when FlushCaches is false; exec log = %v", s.execLog)
		}
	}
}
