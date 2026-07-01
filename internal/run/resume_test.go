package run_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
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

func TestResumeAdoptsOwnPausedResumable(t *testing.T) {
	runner := &sqlCapturingRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithResumeCheck(&fakeResumeCheck{paused: true}))
	// A prior run recorded that operation 0 left its own paused resumable on dbo.T.IX
	// (setupEngine seeded a single rebuild_index of that index); the server still holds it.
	writeSidecarState(t, dirs, "010_a.yaml", run.State{
		Manifest: "010_a.yaml",
		Paused:   &run.PausedResumable{Op: 0, Schema: "dbo", Table: "T", Index: "IX"},
	})

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if len(runner.sqls) != 1 {
		t.Fatalf("runner ran %d statements, want 1", len(runner.sqls))
	}
	if !strings.Contains(runner.sqls[0], "RESUME") {
		t.Errorf("the recorded own paused resumable should RESUME, got: %q", runner.sqls[0])
	}
}

// indexAwareResumeCheck reports a paused resumable only for a specific index, so a test can
// model a server where one index (not another) holds a paused rebuild.
type indexAwareResumeCheck struct{ pausedIndex string }

func (c indexAwareResumeCheck) PausedResumable(_ context.Context, _, _, index string) (bool, error) {
	return strings.EqualFold(index, c.pausedIndex), nil
}

func TestOwnPausedResumedWhenNotAtCursorBoundary(t *testing.T) {
	// The headline #1 case: a continue-on-failure gap left the cursor behind the operation
	// that actually paused its own resumable (op 1, index IX2), while op 0 (IX) is not paused.
	// The paused op must be RESUMEd by recorded identity even though it is not at the cursor,
	// and op 0 must run normally rather than being treated as a foreign blocker.
	runner := &sqlCapturingRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithResumeCheck(indexAwareResumeCheck{pausedIndex: "IX2"}))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSidecarState(t, dirs, "010_a.yaml", run.State{
		Manifest: "010_a.yaml",
		Paused:   &run.PausedResumable{Op: 1, Schema: "dbo", Table: "T", Index: "IX2"},
	})

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Done != 1 {
		t.Fatalf("Summary = %+v, want Done:1 (op 0 runs, op 1 resumes — neither rejected)", sum)
	}
	if len(runner.sqls) != 2 {
		t.Fatalf("runner ran %d statements, want 2", len(runner.sqls))
	}
	if strings.Contains(runner.sqls[0], "RESUME") {
		t.Errorf("op 0 (IX, not paused) should run a fresh REBUILD, got: %q", runner.sqls[0])
	}
	if !strings.Contains(runner.sqls[1], "RESUME") {
		t.Errorf("op 1 (IX2, our own paused resumable) should RESUME, got: %q", runner.sqls[1])
	}
}

func TestForeignPausedNotAdoptedWithoutOwnRecord(t *testing.T) {
	// #5: a manifest drained at an op boundary leaves no paused-resumable record. On re-run a
	// foreign paused resumable happens to sit on the next op's index. It must NOT be adopted
	// (never RESUME); without the opt-in it fails with the actionable message.
	runner := &sqlCapturingRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		run.WithResumeCheck(indexAwareResumeCheck{pausedIndex: "IX2"}))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// Cursor past op 0 (drained after it), but NO Paused record: op 1's paused resumable is foreign.
	writeSidecarState(t, dirs, "010_a.yaml", run.State{Manifest: "010_a.yaml", ResumeFromOp: 1})

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 1 {
		t.Errorf("Summary = %+v, want Failed:1 (foreign paused resumable not adopted)", sum)
	}
	for _, s := range runner.sqls {
		if strings.Contains(s, "RESUME") {
			t.Errorf("a foreign paused resumable must never be RESUMEd, got: %q", s)
		}
	}
	if log := readFailLog(t, dirs, "010_a.yaml"); !strings.Contains(log, "abort-resumable") {
		t.Errorf("failure should point at abort-resumable, got:\n%s", log)
	}
}

func TestInterruptedResumableRecordsPausedIdentity(t *testing.T) {
	// A resumable operation paused by a graceful stop records its own identity in the sidecar,
	// so the next run can resume it by identity.
	drain := &run.DrainFlag{}
	runner := &stopOnRunRunner{drain: drain}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithDrainSignal(drain.Draining),
		run.WithSession(fakeSession{spid: 70}))
	// setupEngine seeded a single rebuild_index of dbo.T.IX.

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Interrupted != 1 {
		t.Fatalf("Summary = %+v, want Interrupted:1", sum)
	}
	st := readSidecarState(t, dirs, "010_a.yaml")
	if st.Paused == nil {
		t.Fatal("interrupted resumable did not record a Paused identity")
	}
	if st.Paused.Op != 0 || st.Paused.Index != "IX" {
		t.Errorf("Paused = %+v, want {Op:0 … Index:IX}", st.Paused)
	}
}

func TestSkipIfSatisfiedDoesNotOrphanOwnPausedResumable(t *testing.T) {
	// #6: skip_if_satisfied would skip a rebuild already at its target compression, but this
	// op left its own paused resumable — skipping would orphan it. It must RESUME instead.
	runner := &sqlCapturingRunner{}
	comp := &fakeCompression{parts: []mssql.PartitionCompression{{Partition: 1, Desc: "PAGE"}}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithCompressionReader(comp),
		run.WithSession(fakeSession{spid: 70}),
		run.WithResumeCheck(&fakeResumeCheck{paused: true}))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(skipCompressManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSidecarState(t, dirs, "010_a.yaml", run.State{
		Manifest: "010_a.yaml",
		Paused:   &run.PausedResumable{Op: 0, Schema: "dbo", Table: "T", Index: "IX"},
	})

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if len(runner.sqls) != 1 || !strings.Contains(runner.sqls[0], "RESUME") {
		t.Errorf("op should RESUME its own paused resumable rather than be skipped, ran: %v", runner.sqls)
	}
}

// fakeBatchDriver is a BatchDMLDriver returning a fixed error, to test how the engine treats
// the key_range watermark on completion versus failure.
type fakeBatchDriver struct{ err error }

func (d fakeBatchDriver) Run(_ context.Context, op ddl.BatchDML, _ ddl.ResolvedOptions, _ run.IgnoreSource, _ run.WatermarkStore, _ run.ReactionSink) (run.BatchDMLResult, error) {
	return run.BatchDMLResult{Schema: op.Schema, Table: op.Table, Verb: op.Verb}, d.err
}

const batchKeyRangeManifest = `
description: batch key_range test
operations:
  - operation: batch_update
    schema: dbo
    table: T
    set: { A: 1 }
    confirm_full_table: true
    batch: { strategy: key_range, key: Id }
`

func TestKeyRangeWatermarkKeptOnFailure(t *testing.T) {
	// #8: a key_range batch that fails with a non-ErrStopped error must keep its watermark so
	// a manual re-run resumes mid-table (only true completion clears it).
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithBatchDMLRunner(fakeBatchDriver{err: errors.New("boom")}))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(batchKeyRangeManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	wm := filepath.Join(dirs.Processing, "010_a.yaml.op0.wm")
	if err := os.WriteFile(wm, []byte("999"), 0o644); err != nil { // a prior partial walk
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	mustExist(t, wm)
}

func TestKeyRangeWatermarkClearedOnSuccess(t *testing.T) {
	// The success path still clears the watermark (guard against over-correcting #8).
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithBatchDMLRunner(fakeBatchDriver{err: nil}))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(batchKeyRangeManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	wm := filepath.Join(dirs.Processing, "010_a.yaml.op0.wm")
	if err := os.WriteFile(wm, []byte("999"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	mustNotExist(t, wm)
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

func TestResumeCursorBeyondPlanRestartsClean(t *testing.T) {
	// A stale sidecar carries a cursor past the current plan length (manifest replaced by a
	// shorter one, or a leftover from a different manifest). The run must NOT skip every
	// operation and report a false SUCCESS — it must restart from the first operation.
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSidecarState(t, dirs, "010_a.yaml", run.State{Manifest: "010_a.yaml", ResumeFromOp: 5})

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 2 {
		t.Errorf("runner ran %d ops, want 2 (cursor past the plan is ignored, run restarts clean)", runner.calls)
	}
	if sum.Done != 1 {
		t.Errorf("Summary = %+v, want Done:1 (both ops executed, not a false success)", sum)
	}
}

func TestResumeFingerprintMismatchRestarts(t *testing.T) {
	// The cursor was recorded against a different plan (fingerprint mismatch): the manifest
	// changed since it was interrupted, so the leading op is redone rather than skipped.
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSidecarState(t, dirs, "010_a.yaml", run.State{Manifest: "010_a.yaml", ResumeFromOp: 1, PlanFingerprint: "stale-does-not-match"})

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 2 {
		t.Errorf("runner ran %d ops, want 2 (fingerprint mismatch restarts from op 0)", runner.calls)
	}
}

func TestFingerprintMismatchSweepsWatermarks(t *testing.T) {
	// A stale key_range watermark from the prior (different) plan must be removed when the
	// resume is invalidated, so a fresh walk does not resume from a wrong position.
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSidecarState(t, dirs, "010_a.yaml", run.State{Manifest: "010_a.yaml", ResumeFromOp: 1, PlanFingerprint: "stale"})
	wm := filepath.Join(dirs.Processing, "010_a.yaml.op0.wm")
	if err := os.WriteFile(wm, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	mustNotExist(t, wm)
}

func TestResumeFingerprintMatchSkipsPrefix(t *testing.T) {
	// Round-trip: a first run drains after op 0, recording the cursor AND the plan
	// fingerprint. On the second run of the same (unchanged) manifest the fingerprint
	// matches, so the completed op is skipped — proving a matching fingerprint does not
	// over-correct into a clean restart.
	drain := &run.DrainFlag{}
	runner := &closeOnFirstRunner{drain: drain}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithDrainSignal(drain.Draining),
		run.WithSession(fakeSession{spid: 70}))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("first ProcessAll() error = %v", err)
	}
	// The sidecar now carries a real fingerprint bound to the plan.
	if st := readSidecarState(t, dirs, "010_a.yaml"); st.PlanFingerprint == "" || st.ResumeFromOp != 1 {
		t.Fatalf("after drain: State = %+v, want ResumeFromOp:1 and a non-empty fingerprint", st)
	}

	// Second run of the same manifest: cancel the drain and re-enqueue the manifest, leaving
	// its sidecar in processing (as recovery would).
	drain.Cancel()
	if err := os.Rename(filepath.Join(dirs.Processing, "010_a.yaml"), filepath.Join(dirs.ToRun, "010_a.yaml")); err != nil {
		t.Fatal(err)
	}

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("second ProcessAll() error = %v", err)
	}
	if runner.calls != 2 {
		t.Errorf("runner ran %d ops total, want 2 (op 0 in run 1, op 1 in run 2 — op 0 skipped on resume)", runner.calls)
	}
	if sum.Done != 1 {
		t.Errorf("Summary = %+v, want Done:1", sum)
	}
	mustExist(t, filepath.Join(dirs.Done, "010_a.yaml"))
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
