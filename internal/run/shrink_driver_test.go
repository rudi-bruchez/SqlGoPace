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

	truncateToMB  *int // size after a TRUNCATEONLY pass, if set
	noProgress    bool // chunk moves never change the size (simulate 49516 / data at end)
	floorMB       int  // a chunk cannot shrink below this (also the active-log floor)
	blockTruncate bool // model a long-running TRUNCATEONLY: block until the context is canceled
	allocError    bool // every page-moving chunk returns Msg 5240 (could not adjust allocation)

	recovery string   // recovery_model_desc for LogReuse
	reuse    []string // log_reuse_wait_desc returned per LogReuse call (last value repeats)

	waits []mssql.SessionWait // returned by SessionWaits (constant across calls)

	percentComplete float64 // dm_exec_requests percent_complete for the running chunk (0 = no active request)

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
		if s.allocError {
			return mssqldb.Error{Number: 5240, Message: "Could not adjust the space allocation for file 'Data'."}
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

func (s *fakeServer) Progress(_ context.Context, _ int) (mssql.Progress, bool, error) {
	if s.percentComplete <= 0 {
		return mssql.Progress{}, false, nil
	}
	return mssql.Progress{PercentComplete: s.percentComplete, Command: "DbccFilesCompact"}, true, nil
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
		sizeMB: 1000, usedMB: 400, floorMB: 400, allocError: true, // every chunk returns Msg 5240
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"} // final 440
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("Msg 5240 must not fail the run, got err = %v", err)
	}
	if len(res) != 1 || res[0].Reason == "" {
		t.Fatalf("got %+v, want a clean stop with a reason (work preserved)", res)
	}
	if !strings.Contains(res[0].Reason, "adjust") {
		t.Errorf("Reason = %q, want it to mention the allocation could not be adjusted", res[0].Reason)
	}
	if res[0].FinalMB != 1000 { // nothing moved, but the file size is honestly reported
		t.Errorf("FinalMB = %d, want the unchanged 1000", res[0].FinalMB)
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
	// MB/s → 60s without blocking.
	if chunks, eta, etaNB := estimateShrink(1000, 600, 0, 10, 100*time.Second, 60*time.Second); chunks != 15 || eta != 150 || etaNB != 60 {
		t.Errorf("estimateShrink = (%d chunks, %d s, %d s no-block), want (15, 150, 60)", chunks, eta, etaNB)
	}
	// No blocking: the two ETAs coincide.
	if _, eta, etaNB := estimateShrink(1000, 600, 0, 10, 100*time.Second, 0); eta != 150 || etaNB != 150 {
		t.Errorf("estimateShrink unblocked = (%d, %d), want both 150", eta, etaNB)
	}
	// No progress yet (nothing reclaimed): no estimate rather than a misleading one.
	if chunks, eta, etaNB := estimateShrink(1000, 1000, 0, 0, 0, 0); chunks != 0 || eta != 0 || etaNB != 0 {
		t.Errorf("estimateShrink with no progress = (%d, %d, %d), want (0, 0, 0)", chunks, eta, etaNB)
	}
	// Nothing left to reclaim: no estimate.
	if chunks, eta, _ := estimateShrink(1000, 500, 500, 5, 50*time.Second, 0); chunks != 0 || eta != 0 {
		t.Errorf("estimateShrink at target = (%d, %d), want (0, 0)", chunks, eta)
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
