package run_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// writeSidecarState drops a State sidecar into processing so a test can seed a resume
// cursor as a previous drained run would have left it.
func writeSidecarState(t *testing.T, dirs run.Dirs, manifest string, st run.State) {
	t.Helper()
	if err := run.WriteState(filepath.Join(dirs.Processing, manifest+".state.json"), st); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

func readSidecarState(t *testing.T, dirs run.Dirs, manifest string) run.State {
	t.Helper()
	st, err := run.ReadState(filepath.Join(dirs.Processing, manifest+".state.json"))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	return st
}

func TestResumeCursorSkipsCompletedOps(t *testing.T) {
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// A previous run drained after operation 1: the sidecar carries a cursor.
	writeSidecarState(t, dirs, "010_a.yaml", run.State{Manifest: "010_a.yaml", ResumeFromOp: 1})

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("runner ran %d ops, want 1 (op 0 skipped by the resume cursor)", runner.calls)
	}

	data, err := os.ReadFile(filepath.Join(dirs.Done, "010_a.yaml.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if out := string(data); !strings.Contains(out, "skipped") {
		t.Errorf("log should mark the resumed op skipped\n%s", out)
	}
}

func TestDrainWritesResumeCursor(t *testing.T) {
	drain := &run.DrainFlag{}
	runner := &closeOnFirstRunner{drain: drain}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithDrainSignal(drain.Draining),
		run.WithSession(fakeSession{spid: 70}))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	// One op ran before the drain; the sidecar cursor must point at the next op.
	st := readSidecarState(t, dirs, "010_a.yaml")
	if st.ResumeFromOp != 1 {
		t.Errorf("ResumeFromOp = %d, want 1", st.ResumeFromOp)
	}
}

// cursorProbeRunner records the resume cursor persisted in the sidecar at the start of
// the second operation, proving the cursor advances progressively after each completed
// operation — not only when the run drains or finalizes (so a crash resumes correctly).
type cursorProbeRunner struct {
	dirs     run.Dirs
	manifest string
	calls    int
	seen     int
}

func (r *cursorProbeRunner) Run(_ context.Context, _ ddl.Operation, _ string, _ run.Capabilities, _ run.ReactionSink) error {
	r.calls++
	if r.calls == 2 {
		if st, err := run.ReadState(filepath.Join(r.dirs.Processing, r.manifest+".state.json")); err == nil {
			r.seen = st.ResumeFromOp
		}
	}
	return nil
}

func TestResumeCursorAdvancesProgressively(t *testing.T) {
	runner := &cursorProbeRunner{manifest: "010_a.yaml"}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithSession(fakeSession{spid: 70}))
	runner.dirs = dirs
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	// Operation 1 (index 0) completed before operation 2 started; its cursor must have
	// been written to the sidecar by then, without any drain or finalize.
	if runner.seen != 1 {
		t.Errorf("cursor at start of op 2 = %d, want 1 (op 1 persisted progressively)", runner.seen)
	}
}

// sqlCapturingRunner records the statement it is handed for each operation, so a test
// can assert the engine issued ALTER INDEX … RESUME rather than a fresh REBUILD.
type sqlCapturingRunner struct{ sqls []string }

func (r *sqlCapturingRunner) Run(_ context.Context, _ ddl.Operation, sql string, _ run.Capabilities, _ run.ReactionSink) error {
	r.sqls = append(r.sqls, sql)
	return nil
}

// fakeAborter records the ALTER INDEX … ABORT statements the engine issues to clear a
// blocking paused resumable.
type fakeAborter struct{ sqls []string }

func (f *fakeAborter) ExecDDL(_ context.Context, sql string) error {
	f.sqls = append(f.sqls, sql)
	return nil
}

func readFailLog(t *testing.T, dirs run.Dirs, manifest string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dirs.Failed, manifest+".log"))
	if err != nil {
		t.Fatalf("read fail log: %v", err)
	}
	return string(data)
}

const abortOptInManifest = `
description: abort opt-in
abort_blocking_resumable: true
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`

func TestResumeAdoptsPausedResumableAtBoundary(t *testing.T) {
	runner := &sqlCapturingRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithResumeCheck(&fakeResumeCheck{paused: true}))
	// A prior run of this manifest was interrupted mid-rebuild, leaving a sidecar; the
	// server still holds the paused resumable. setupEngine seeded a single rebuild_index.
	writeSidecarState(t, dirs, "010_a.yaml", run.State{Manifest: "010_a.yaml"})

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if len(runner.sqls) != 1 {
		t.Fatalf("runner ran %d statements, want 1", len(runner.sqls))
	}
	if !strings.Contains(runner.sqls[0], "RESUME") {
		t.Errorf("boundary op should RESUME the paused resumable, got: %q", runner.sqls[0])
	}
}

func TestBlockingResumableWithoutOptInFails(t *testing.T) {
	runner := &sqlCapturingRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithResumeCheck(&fakeResumeCheck{paused: true}),
		run.WithResumableAborter(&fakeAborter{}))
	// Fresh manifest (no sidecar): a paused resumable on the index is foreign. Without the
	// abort_blocking_resumable opt-in the run must NOT adopt it (never RESUME) — it fails
	// early with an actionable message and never runs the rebuild.
	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 1 {
		t.Errorf("Summary = %+v, want Failed:1", sum)
	}
	if len(runner.sqls) != 0 {
		t.Errorf("op must not run (least of all RESUME); ran: %v", runner.sqls)
	}
	if log := readFailLog(t, dirs, "010_a.yaml"); !strings.Contains(log, "abort-resumable") {
		t.Errorf("failure should point at abort-resumable, got:\n%s", log)
	}
}

func TestBlockingResumableOptInAborts(t *testing.T) {
	runner := &sqlCapturingRunner{}
	aborter := &fakeAborter{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithResumeCheck(&fakeResumeCheck{paused: true}),
		run.WithResumableAborter(aborter))
	// Fresh manifest that opts in: the blocking paused resumable is ABORTed, then the
	// rebuild runs with this manifest's own options.
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(abortOptInManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Done != 1 {
		t.Errorf("Summary = %+v, want Done:1", sum)
	}
	if len(aborter.sqls) != 1 || !strings.Contains(aborter.sqls[0], "ABORT") {
		t.Errorf("expected exactly one ALTER INDEX … ABORT, got: %v", aborter.sqls)
	}
	// The run statement is the fresh REBUILD (it carries RESUMABLE as an option, so match
	// on REBUILD rather than the absence of "RESUME"), not a bare ABORT/RESUME control.
	if len(runner.sqls) != 1 || !strings.Contains(runner.sqls[0], "REBUILD") {
		t.Errorf("expected the fresh REBUILD after the abort, got: %v", runner.sqls)
	}
}

func TestWriteSidecarPreservesCursor(t *testing.T) {
	// A fresh sidecar write on a manifest that carries a cursor must keep it (so a
	// re-run started by recovery does not lose the resume point).
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithSession(fakeSession{spid: 70}))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSidecarState(t, dirs, "010_a.yaml", run.State{Manifest: "010_a.yaml", ResumeFromOp: 1})

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	// The manifest completes (op 0 skipped, op 1 run), so the sidecar is removed on
	// success — but the run must have honored the cursor: only one op ran.
	if runner.calls != 1 {
		t.Errorf("runner ran %d ops, want 1 (cursor honored despite fresh sidecar write)", runner.calls)
	}
}
