package run

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

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

	truncateToMB *int // size after a TRUNCATEONLY pass, if set
	noProgress   bool // chunk moves never change the size (simulate 49516 / data at end)
	floorMB      int  // a chunk cannot shrink below this (also the active-log floor)

	recovery string   // recovery_model_desc for LogReuse
	reuse    []string // log_reuse_wait_desc returned per LogReuse call (last value repeats)

	waits []mssql.SessionWait // returned by SessionWaits (constant across calls)

	execLog  []string
	killed   bool
	reuseIdx int

	onExec func(sql string) // test hook, called at the start of each ExecDDL
}

func (s *fakeServer) SPID() int { return 99 }

func (s *fakeServer) ExecDDL(_ context.Context, sql string) error {
	if s.onExec != nil {
		s.onExec(sql)
	}
	s.execLog = append(s.execLog, sql)
	switch {
	case strings.Contains(sql, "TRUNCATEONLY"):
		if s.truncateToMB != nil && *s.truncateToMB < s.sizeMB {
			s.sizeMB = *s.truncateToMB
		}
	case strings.Contains(sql, "CHECKPOINT"):
		// no size change
	case strings.Contains(sql, "DBCC SHRINKFILE"):
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
	stop := make(chan struct{})
	s := &fakeServer{fileType: mssql.FileTypeRows, name: "Data", sizeMB: 2000, usedMB: 200}
	// Close the graceful-stop signal when the first chunk runs, so the loop finishes that
	// chunk and stops before the next one.
	closed := false
	s.onExec = func(sql string) {
		if !closed && strings.Contains(sql, "DBCC SHRINKFILE") && !strings.Contains(sql, "TRUNCATEONLY") {
			closed = true
			close(stop)
		}
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.stop = stop

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

func TestShrinkStepAdjustsUnderIOPressure(t *testing.T) {
	// High WRITELOG average between snapshots must halve the step (pure calc path
	// fed by the driver's waitDeltas).
	before := []mssql.SessionWait{{WaitType: "WRITELOG", WaitTimeMS: 0, WaitingTasksCount: 0}}
	after := []mssql.SessionWait{{WaitType: "WRITELOG", WaitTimeMS: 300, WaitingTasksCount: 10}} // 30 ms avg
	d := waitDeltas(before, after)
	if d.WriteLogAvgMs != 30 {
		t.Fatalf("waitDeltas WriteLogAvgMs = %v, want 30", d.WriteLogAvgMs)
	}
	if got := AdjustStepMB(400, time.Second, d, testTuning()); got != 200 {
		t.Errorf("AdjustStepMB under WRITELOG pressure = %d, want 200", got)
	}
}
