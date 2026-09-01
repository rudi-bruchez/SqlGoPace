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

// scriptedBatchDriver returns a scripted BatchDMLResult so the engine's handling of a
// batch that stopped early can be verified without a database.
type scriptedBatchDriver struct {
	result   run.BatchDMLResult
	err      error
	calls    int
	wmCalls  int
	clearedW bool
}

func (f *scriptedBatchDriver) Run(_ context.Context, op ddl.BatchDML, _ ddl.ResolvedOptions, _ run.IgnoreSource, wm run.WatermarkStore, _ run.ReactionSink) (run.BatchDMLResult, error) {
	f.calls++
	f.result.Schema, f.result.Table, f.result.Verb = op.Schema, op.Table, op.Verb
	return f.result, f.err
}

const batchDeleteManifest = `operations:
  - operation: batch_delete
    schema: dbo
    table: AuditLog
    where_raw: "CreatedAt < '2024-01-01'"
`

// TestProcessAllBatchStoppedShortIsIncomplete is the batch twin of
// TestProcessAllShrinkIncompleteStopsShort. A batch that stops on log pressure, on
// blocking, or on its self-wait budget returns a nil error with a Reason, so the
// engine recorded it as a clean success: the manifest was finalized into 03.done/ and
// an operator draining a queue overnight saw a completed purge that had abandoned most
// of its rows. The shrink driver has routed the same class of event to INCOMPLETE
// since 0.17.0; this reuses that path.
func TestProcessAllBatchStoppedShortIsIncomplete(t *testing.T) {
	driver := &scriptedBatchDriver{result: run.BatchDMLResult{
		Rows: 40000, Batches: 8, FinalRows: 5000,
		Reason: "stopped: log did not drain before timeout (committed batches preserved)",
	}}
	eng, dirs := setupShrinkEngineOpts(t, batchDeleteManifest, &fakeOpRunner{}, run.WithBatchDMLRunner(driver))

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if driver.calls != 1 {
		t.Fatalf("driver called %d times, want 1", driver.calls)
	}
	if sum.Incomplete != 1 || sum.Done != 0 {
		t.Errorf("Summary = %+v, want Incomplete:1 Done:0", sum)
	}
	if _, err := os.Stat(filepath.Join(dirs.Done, "010_shrink.yaml.log")); !os.IsNotExist(err) {
		t.Errorf("a batch that stopped short must not land in done/ (err=%v)", err)
	}
	b, err := os.ReadFile(filepath.Join(dirs.Failed, "010_shrink.yaml.log"))
	if err != nil {
		t.Fatalf("read failed log: %v", err)
	}
	log := string(b)
	for _, want := range []string{"INCOMPLETE", "log did not drain"} {
		if !strings.Contains(log, want) {
			t.Errorf("run log missing %q\n--- log ---\n%s", want, log)
		}
	}
	if strings.Contains(log, "SUCCESS") {
		t.Errorf("a batch that stopped short must not be labeled SUCCESS\n--- log ---\n%s", log)
	}
}

// TestProcessAllBatchCompleteIsSuccess: a batch that exhausted its predicate has no
// Reason and must stay a clean success, or every purge would be filed as incomplete.
func TestProcessAllBatchCompleteIsSuccess(t *testing.T) {
	driver := &scriptedBatchDriver{result: run.BatchDMLResult{Rows: 120000, Batches: 24, FinalRows: 5000}}
	eng, dirs := setupShrinkEngineOpts(t, batchDeleteManifest, &fakeOpRunner{}, run.WithBatchDMLRunner(driver))

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Done != 1 || sum.Incomplete != 0 {
		t.Errorf("Summary = %+v, want Done:1 Incomplete:0", sum)
	}
	if _, err := os.Stat(filepath.Join(dirs.Done, "010_shrink.yaml.log")); err != nil {
		t.Errorf("a completed batch must land in done/: %v", err)
	}
}

// TestKeyRangeWatermarkKeptWhenStoppedShort is the second half of the same defect. The
// watermark was cleared on any nil-error return, and a stopped-short batch returns nil,
// so the walk that abandoned most of its rows could not be resumed either — the manifest
// said done and the resume point was gone.
func TestKeyRangeWatermarkKeptWhenStoppedShort(t *testing.T) {
	driver := &scriptedBatchDriver{result: run.BatchDMLResult{
		Rows: 40000, Batches: 8,
		Reason: "stopped: log did not drain before timeout (committed batches preserved)",
	}}
	eng, dirs := setupShrinkEngineOpts(t, batchKeyRangeManifest, &fakeOpRunner{}, run.WithBatchDMLRunner(driver))

	wm := filepath.Join(dirs.Processing, "010_shrink.yaml.op0.wm")
	if err := os.WriteFile(wm, []byte("4200"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if _, err := os.Stat(wm); os.IsNotExist(err) {
		t.Error("the watermark was cleared on a batch that stopped short; the walk cannot be resumed")
	}
}
