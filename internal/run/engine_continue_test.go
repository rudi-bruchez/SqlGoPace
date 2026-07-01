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
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// fakeExpander stands in for the production index expander. The continue manifest
// uses named indexes, so RebuildableIndexes is never called — but wiring the
// expander still routes the manifest through ExpandAll, the path that once dropped
// on_failure and silently reverted continue mode to fail-fast.
type fakeExpander struct{}

func (fakeExpander) RebuildableIndexes(context.Context, string, string) ([]mssql.IndexInfo, error) {
	return nil, nil
}

// seqOpRunner returns errs[i] on call i (nil past the end), so a multi-operation
// manifest can have specific operations fail while the rest succeed.
type seqOpRunner struct {
	errs  []error
	calls int
}

func (f *seqOpRunner) Run(_ context.Context, _ ddl.Operation, _ string, _ run.Capabilities, _ run.ReactionSink) error {
	i := f.calls
	f.calls++
	if i < len(f.errs) {
		return f.errs[i]
	}
	return nil
}

// continueManifest is a 3-operation manifest in continue-on-failure mode. The
// second operation carries options, to prove they survive into the recovery file.
const continueManifest = `
description: continue test
database: DB1
on_failure: continue
operations:
  - operation: rebuild_index
    schema: dbo
    table: T1
    index: IX1
  - operation: rebuild_index
    schema: dbo
    table: T2
    index: IX2
    data_compression: PAGE
    options:
      maxdop: 2
  - operation: rebuild_index
    schema: dbo
    table: T3
    index: IX3
`

// writeOnly replaces the queue with a single manifest, removing the default one
// setupEngine seeds, so a test drives exactly the manifest it writes.
func writeOnly(t *testing.T, dirs run.Dirs, name, body string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dirs.ToRun, "010_a.yaml")); err != nil {
		t.Fatalf("remove default manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirs.ToRun, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be absent, stat err = %v", path, err)
	}
}

func TestContinueModeProceedsPastFailure(t *testing.T) {
	runner := &seqOpRunner{errs: []error{nil, io.ErrUnexpectedEOF, nil}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	writeOnly(t, dirs, "100_c.yaml", continueManifest)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 3 {
		t.Errorf("runner.calls = %d, want 3 (loop continued past the failed op)", runner.calls)
	}
	if sum.Failed != 1 || sum.Done != 0 {
		t.Errorf("Summary = %+v, want Failed:1 (PARTIAL counts as failed)", sum)
	}
	mustExist(t, filepath.Join(dirs.Failed, "100_c.yaml"))
	mustExist(t, filepath.Join(dirs.Failed, "100_c.yaml.log"))
	mustExist(t, filepath.Join(dirs.Failed, "100_c.yaml.recovery.yaml"))

	logBytes, err := os.ReadFile(filepath.Join(dirs.Failed, "100_c.yaml.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if log := string(logBytes); !strings.Contains(log, "PARTIAL") {
		t.Errorf("log missing PARTIAL outcome\n%s", log)
	}
}

func TestContinueModeWithExpanderWired(t *testing.T) {
	// Regression: in production the expander is wired (WithExpander). It must not
	// drop on_failure, or continue mode silently reverts to fail-fast.
	runner := &seqOpRunner{errs: []error{nil, io.ErrUnexpectedEOF, nil}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithExpander(fakeExpander{}))
	writeOnly(t, dirs, "100_c.yaml", continueManifest)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 3 {
		t.Errorf("runner.calls = %d, want 3 (continue survived expansion)", runner.calls)
	}
	if sum.Failed != 1 {
		t.Errorf("Summary = %+v, want Failed:1 (PARTIAL)", sum)
	}
	mustExist(t, filepath.Join(dirs.Failed, "100_c.yaml.recovery.yaml"))
}

func TestContinueModeRecoveryManifestRoundTrips(t *testing.T) {
	runner := &seqOpRunner{errs: []error{nil, io.ErrUnexpectedEOF, nil}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	writeOnly(t, dirs, "100_c.yaml", continueManifest)

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}

	rec, err := ddl.LoadManifestFile(filepath.Join(dirs.Failed, "100_c.yaml.recovery.yaml"))
	if err != nil {
		t.Fatalf("load recovery manifest: %v", err)
	}
	if !rec.Continue() {
		t.Errorf("recovery manifest OnFailure = %q, want continue (must round-trip)", rec.OnFailure)
	}
	if rec.Database != "DB1" {
		t.Errorf("recovery Database = %q, want DB1", rec.Database)
	}
	if len(rec.Operations) != 1 {
		t.Fatalf("recovery has %d operations, want 1 (only the failed op)", len(rec.Operations))
	}
	ri, ok := rec.Operations[0].(ddl.RebuildIndex)
	if !ok {
		t.Fatalf("recovery op type = %T, want ddl.RebuildIndex", rec.Operations[0])
	}
	if ri.Target().Name != "IX2" {
		t.Errorf("recovery op index = %q, want IX2 (the failed one)", ri.Target().Name)
	}
	if ri.DataCompression != "PAGE" {
		t.Errorf("recovery op data_compression = %q, want PAGE (options preserved)", ri.DataCompression)
	}
	if ri.Options.MaxDOP == nil || *ri.Options.MaxDOP != 2 {
		t.Errorf("recovery op maxdop = %v, want 2 (options preserved)", ri.Options.MaxDOP)
	}
}

func TestContinueModeAllSuccessNoRecovery(t *testing.T) {
	runner := &seqOpRunner{} // every op succeeds
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	writeOnly(t, dirs, "100_c.yaml", continueManifest)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Done != 1 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want Done:1", sum)
	}
	mustExist(t, filepath.Join(dirs.Done, "100_c.yaml"))
	mustNotExist(t, filepath.Join(dirs.Failed, "100_c.yaml.recovery.yaml"))
}

func TestContinueModeAllFailedRecoversAll(t *testing.T) {
	runner := &seqOpRunner{errs: []error{io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	writeOnly(t, dirs, "100_c.yaml", continueManifest)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 1 {
		t.Errorf("Summary = %+v, want Failed:1", sum)
	}
	rec, err := ddl.LoadManifestFile(filepath.Join(dirs.Failed, "100_c.yaml.recovery.yaml"))
	if err != nil {
		t.Fatalf("load recovery manifest: %v", err)
	}
	if len(rec.Operations) != 3 {
		t.Errorf("recovery has %d operations, want 3 (all failed)", len(rec.Operations))
	}
}

func TestStopModeFailureWritesNoRecovery(t *testing.T) {
	// The default manifest (010_a.yaml) has no on_failure → stop (fail-fast).
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
	mustNotExist(t, filepath.Join(dirs.Failed, "010_a.yaml.recovery.yaml"))

	logBytes, err := os.ReadFile(filepath.Join(dirs.Failed, "010_a.yaml.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if log := string(logBytes); strings.Contains(log, "PARTIAL") {
		t.Errorf("stop-mode failure must be FAILED, not PARTIAL\n%s", log)
	}
}

func TestContinueModeResumableInterruptionWins(t *testing.T) {
	// In continue mode, a paused-resumable interruption must still be treated as
	// recoverable (left in processing), not quarantined.
	runner := &seqOpRunner{errs: []error{io.ErrUnexpectedEOF}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithSession(fakeSession{spid: 70}),
		// Not paused when the op starts (no pre-run block), paused after the kill.
		run.WithResumeCheck(&fakeResumeCheck{becomesPaused: true}))
	writeOnly(t, dirs, "100_c.yaml", continueManifest)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Interrupted != 1 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want Interrupted:1 (resumable wins over continue)", sum)
	}
	mustExist(t, filepath.Join(dirs.Processing, "100_c.yaml"))
	mustNotExist(t, filepath.Join(dirs.Failed, "100_c.yaml.recovery.yaml"))
}
