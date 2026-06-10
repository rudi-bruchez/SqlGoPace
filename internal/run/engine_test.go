package run_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

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
}

func (f *fakeOpRunner) Run(context.Context, ddl.Operation, string, run.Capabilities) error {
	f.calls++
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

func setupEngine(t *testing.T, pf run.Preflighter, runner run.OpRunner) (*run.Engine, run.Dirs) {
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
	eng := run.NewEngine(dirs, target, matrix, ddl.Policy{}, pf, runner)
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

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file %s: %v", path, err)
	}
}
