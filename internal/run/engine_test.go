package run_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

type fakeSession struct{ spid int }

func (f fakeSession) SPID() int                                 { return f.spid }
func (f fakeSession) LoginTime(context.Context) (string, error) { return "2026-01-01T00:00:00", nil }
func (f fakeSession) SetMarker(context.Context, [16]byte) error { return nil }

type fakeResumeCheck struct {
	paused bool
	err    error // non-nil simulates an unreachable server (e.g. restarting)
}

func (f fakeResumeCheck) PausedResumable(context.Context, string, string, string) (bool, error) {
	return f.paused, f.err
}

type fakeProgress struct {
	pct   float64
	found bool
}

func (f fakeProgress) Progress(context.Context, int) (mssql.Progress, bool, error) {
	return mssql.Progress{PercentComplete: f.pct}, f.found, nil
}

// fakeWaits returns a scripted sequence of session-wait snapshots, one per call,
// so before/after diffing produces a deterministic per-operation delta.
type fakeWaits struct {
	snapshots [][]mssql.SessionWait
	calls     int
}

func (f *fakeWaits) SessionWaits(context.Context, int) ([]mssql.SessionWait, error) {
	i := f.calls
	f.calls++
	switch {
	case i < len(f.snapshots):
		return f.snapshots[i], nil
	case len(f.snapshots) > 0:
		return f.snapshots[len(f.snapshots)-1], nil
	default:
		return nil, nil
	}
}

// fakeBlockerReader returns a fixed set of active sessions for the blocked-session
// capture.
type fakeBlockerReader struct{ sessions []mssql.Session }

func (f fakeBlockerReader) ActiveSessions(context.Context) ([]mssql.Session, error) {
	return f.sessions, nil
}

type fakePreflighter struct {
	report preflight.Report
	err    error
}

func (f fakePreflighter) Check(context.Context, *ddl.Manifest) (preflight.Report, error) {
	return f.report, f.err
}

type fakeOpRunner struct {
	err   error
	calls int
	emit  []run.ReactionEvent
}

func (f *fakeOpRunner) Run(_ context.Context, _ ddl.Operation, _ string, _ run.Capabilities, sink run.ReactionSink) error {
	f.calls++
	for _, ev := range f.emit {
		sink(ev)
	}
	return f.err
}

const engineManifest = `
description: engine test
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`

func setupEngine(t *testing.T, pf run.Preflighter, runner run.OpRunner, opts ...run.EngineOption) (*run.Engine, run.Dirs) {
	t.Helper()
	root := t.TempDir()
	dirs := run.Dirs{
		ToRun:      filepath.Join(root, "01.to_run"),
		Processing: filepath.Join(root, "02.processing"),
		Done:       filepath.Join(root, "03.done"),
		Failed:     filepath.Join(root, "04.failed"),
	}
	for _, d := range []string{dirs.ToRun, dirs.Processing, dirs.Done, dirs.Failed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(engineManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	matrix, err := ddl.LoadFile(filepath.FromSlash("../../ddl_compatibility.yaml"))
	if err != nil {
		t.Fatalf("load matrix: %v", err)
	}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}
	eng := run.NewEngine(dirs, target, matrix, ddl.Policy{}, pf, runner, opts...)
	return eng, dirs
}

func TestProcessAllSuccess(t *testing.T) {
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Done != 1 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want {Done:1 Failed:0}", sum)
	}
	if runner.calls != 1 {
		t.Errorf("runner.calls = %d, want 1", runner.calls)
	}
	mustExist(t, filepath.Join(dirs.Done, "010_a.yaml"))
	mustExist(t, filepath.Join(dirs.Done, "010_a.yaml.log"))
}

func TestProcessAllDatabaseFilter(t *testing.T) {
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithDatabase("DB1"))

	dbManifest := func(db string) []byte {
		return []byte("database: " + db + "\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n")
	}
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "020_db1.yaml"), dbManifest("DB1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "030_db2.yaml"), dbManifest("DB2"), 0o644); err != nil {
		t.Fatal(err)
	}

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	// 010_a (no database → owned by any) + 020_db1 (matches) processed; 030_db2 skipped.
	if sum.Done != 2 {
		t.Errorf("Summary.Done = %d, want 2 (no-database + DB1 manifests)", sum.Done)
	}
	if runner.calls != 2 {
		t.Errorf("runner.calls = %d, want 2", runner.calls)
	}
	// The other database's manifest is left in the queue for its own engine.
	mustExist(t, filepath.Join(dirs.ToRun, "030_db2.yaml"))
	if _, err := os.Stat(filepath.Join(dirs.Done, "030_db2.yaml")); err == nil {
		t.Errorf("030_db2.yaml should not have been processed (wrong database)")
	}
}

func TestProcessAllPreflightFailure(t *testing.T) {
	runner := &fakeOpRunner{}
	report := preflight.Report{Checks: []preflight.Check{{Name: "server", Severity: preflight.Fail, Detail: "nope"}}}
	eng, dirs := setupEngine(t, fakePreflighter{report: report}, runner)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 1 {
		t.Errorf("Summary = %+v, want Failed:1", sum)
	}
	if runner.calls != 0 {
		t.Errorf("runner.calls = %d, want 0 (preflight failed before execution)", runner.calls)
	}
	mustExist(t, filepath.Join(dirs.Failed, "010_a.yaml"))
}

func TestProcessAllOperationError(t *testing.T) {
	runner := &fakeOpRunner{err: io.ErrUnexpectedEOF}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 1 {
		t.Errorf("Summary = %+v, want Failed:1", sum)
	}
	mustExist(t, filepath.Join(dirs.Failed, "010_a.yaml"))
}

func TestProcessAllRecordsReactions(t *testing.T) {
	runner := &fakeOpRunner{emit: []run.ReactionEvent{
		{Kind: "pause", Detail: "blocking other sessions"},
		{Kind: "resume", Detail: "pressure cleared"},
	}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}

	logBytes, err := os.ReadFile(filepath.Join(dirs.Done, "010_a.yaml.log"))
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	log := string(logBytes)
	for _, want := range []string{"reaction: pause", "reaction: resume", "blocking other sessions"} {
		if !strings.Contains(log, want) {
			t.Errorf("run log missing %q\n--- log ---\n%s", want, log)
		}
	}
}

func TestProcessAllRecordsPercentOnInterruptionOnly(t *testing.T) {
	runner := &fakeOpRunner{emit: []run.ReactionEvent{
		{Kind: "pause", Detail: "blocking other sessions"}, // interruption -> percent
		{Kind: "resume", Detail: "pressure cleared"},       // not an interruption -> no percent
	}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithProgress(fakeProgress{pct: 42, found: true}))

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}

	logBytes, err := os.ReadFile(filepath.Join(dirs.Done, "010_a.yaml.log"))
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "blocking other sessions (at 42%)") {
		t.Errorf("pause interruption missing completion percent\n%s", log)
	}
	if strings.Contains(log, "pressure cleared (at") {
		t.Errorf("resume (not an interruption) should not carry a percent\n%s", log)
	}
}

func TestProcessAllRecordsWaitsDelta(t *testing.T) {
	runner := &fakeOpRunner{}
	waits := &fakeWaits{snapshots: [][]mssql.SessionWait{
		{ // before the operation
			{WaitType: "WRITELOG", WaitTimeMS: 100, WaitingTasksCount: 5},
		},
		{ // after the operation
			{WaitType: "WRITELOG", WaitTimeMS: 700, WaitingTasksCount: 50},   // +600
			{WaitType: "LCK_M_SCH_M", WaitTimeMS: 200, WaitingTasksCount: 2}, // new +200
			{WaitType: "SLEEP_TASK", WaitTimeMS: 9000},                       // noise -> dropped
		},
	}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithWaits(waits))

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}

	logBytes, err := os.ReadFile(filepath.Join(dirs.Done, "010_a.yaml.log"))
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	log := string(logBytes)
	for _, want := range []string{"waits (total 800ms)", "Transaction log", "Locking"} {
		if !strings.Contains(log, want) {
			t.Errorf("run log missing %q\n--- log ---\n%s", want, log)
		}
	}
	if strings.Contains(log, "SLEEP_TASK") {
		t.Errorf("noise wait SLEEP_TASK should not be in the log\n%s", log)
	}
}

func TestProcessAllInterruptedResumableLeftForRecovery(t *testing.T) {
	runner := &fakeOpRunner{err: io.ErrUnexpectedEOF} // simulates a killed/lost session
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithResumeCheck(fakeResumeCheck{paused: true}))

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Interrupted != 1 || sum.Failed != 0 || sum.Done != 0 {
		t.Errorf("Summary = %+v, want Interrupted:1", sum)
	}

	// The manifest and its sidecar stay in processing for the next run to resume.
	mustExist(t, filepath.Join(dirs.Processing, "010_a.yaml"))
	mustExist(t, filepath.Join(dirs.Processing, "010_a.yaml.state.json"))
	if _, err := os.Stat(filepath.Join(dirs.Failed, "010_a.yaml")); !os.IsNotExist(err) {
		t.Errorf("interrupted manifest must not be moved to failed")
	}

	// The sidecar carries a CONTEXT_INFO marker for reliable orphan correlation.
	st, err := run.ReadState(filepath.Join(dirs.Processing, "010_a.yaml.state.json"))
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if !strings.HasPrefix(st.Marker, "0x") || len(st.Marker) != 34 {
		t.Errorf("sidecar Marker = %q, want a 0x + 32-hex marker", st.Marker)
	}
}

func TestProcessAllResumableServerUnreachableIsInterrupted(t *testing.T) {
	// The resumable check itself errors (server restarting / unreachable) and the
	// reconnect window elapses: with no conclusive answer, a resumable operation is
	// kept for recovery rather than failed. ReconnectTimeout 0 keeps the test fast.
	runner := &fakeOpRunner{err: io.ErrUnexpectedEOF}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithResumeCheck(fakeResumeCheck{err: io.ErrClosedPipe}),
		run.WithReconnectTimeout(0))

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Interrupted != 1 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want Interrupted:1 (inconclusive → recoverable)", sum)
	}
	mustExist(t, filepath.Join(dirs.Processing, "010_a.yaml"))
}

func TestProcessAllResumableErrorWithoutPauseFails(t *testing.T) {
	// Same error, but no paused resumable op exists -> a genuine failure.
	runner := &fakeOpRunner{err: io.ErrUnexpectedEOF}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithResumeCheck(fakeResumeCheck{paused: false}))

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 1 || sum.Interrupted != 0 {
		t.Errorf("Summary = %+v, want Failed:1", sum)
	}
	mustExist(t, filepath.Join(dirs.Failed, "010_a.yaml"))
}

func TestProcessAllCapturesBlockedSessions(t *testing.T) {
	runner := &fakeOpRunner{err: io.ErrUnexpectedEOF, emit: []run.ReactionEvent{
		{Kind: "cancel", Detail: "blocking other sessions"},
	}}
	blockers := fakeBlockerReader{sessions: []mssql.Session{
		{SPID: 53, BlockingSPID: 70, Login: "svc_report", Host: "BATCH01", Program: "ReportingService", ActiveQuery: "SELECT 1"},
		{SPID: 54, BlockingSPID: 99}, // blocked by someone else — must not be captured
	}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithBlockerReader(blockers))

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dirs.Failed, "010_a.yaml.blocked.yaml"))
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	out := string(data)
	for _, want := range []string{"# ignore_blocked_sessions:", "session_id: 53", "ReportingService", "observed:", "active_query"} {
		if !strings.Contains(out, want) {
			t.Errorf("capture missing %q\n--- capture ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "session_id: 54") {
		t.Errorf("capture must not include a session blocked by someone else\n%s", out)
	}
}

func TestProcessAllCaptureExcludesIgnoredBlocker(t *testing.T) {
	runner := &fakeOpRunner{err: io.ErrUnexpectedEOF, emit: []run.ReactionEvent{
		{Kind: "cancel", Detail: "blocking other sessions"},
	}}
	blockers := fakeBlockerReader{sessions: []mssql.Session{
		{SPID: 53, BlockingSPID: 70, Program: "ReportingService"},
	}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithBlockerReader(blockers))
	writeOnly(t, dirs, "200_ig.yaml", "description: ignore test\n"+
		"ignore_blocked_sessions:\n  - app_name: \"^ReportingService$\"\n"+
		"operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n")

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	// The only blocker matches an ignore rule, so nothing is captured.
	mustNotExist(t, filepath.Join(dirs.Failed, "200_ig.yaml.blocked.yaml"))
}

func TestRecoveryManifestCarriesIgnoreRules(t *testing.T) {
	// A continue-mode run with a failed op writes a recovery manifest; it must carry
	// the manifest's ignore_blocked_sessions so a resumed run still ignores them.
	runner := &fakeOpRunner{err: io.ErrUnexpectedEOF}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	writeOnly(t, dirs, "300_ig.yaml", "description: ig\non_failure: continue\n"+
		"ignore_blocked_sessions:\n  - app_name: \"^SQLAgent\"\n"+
		"operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n")

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}

	rec, err := ddl.LoadManifestFile(filepath.Join(dirs.Failed, "300_ig.yaml.recovery.yaml"))
	if err != nil {
		t.Fatalf("load recovery manifest: %v", err)
	}
	if len(rec.IgnoreBlockedSessions) != 1 || rec.IgnoreBlockedSessions[0].AppName != "^SQLAgent" {
		t.Errorf("recovery ignore rules = %+v, want a single ^SQLAgent rule", rec.IgnoreBlockedSessions)
	}
}

func TestManifestObserverReportsInflightPath(t *testing.T) {
	// The observer must see the in-processing path while a manifest runs and "" after,
	// so the TUI knows which file to edit.
	var seen []string
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithManifestObserver(func(p string) { seen = append(seen, p) }))

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}

	want := filepath.Join(dirs.Processing, "010_a.yaml")
	if len(seen) != 2 || seen[0] != want || seen[1] != "" {
		t.Errorf("observer saw %v, want [%q, \"\"]", seen, want)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file %s: %v", path, err)
	}
}
