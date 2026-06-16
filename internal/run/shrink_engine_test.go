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

// fakeShrinkDriver records that it was called and returns scripted results, so the
// engine routing can be verified without a database.
type fakeShrinkDriver struct {
	results []run.ShrinkResult
	err     error
	calls   int
	gotType string
	emit    []run.ReactionEvent
}

func (f *fakeShrinkDriver) Run(_ context.Context, op ddl.Shrink, _ ddl.ResolvedOptions, sink run.ReactionSink) ([]run.ShrinkResult, error) {
	f.calls++
	f.gotType = op.Type
	for _, ev := range f.emit {
		sink(ev)
	}
	return f.results, f.err
}

// setupShrinkEngine builds an engine over a temp queue holding a single shrink
// manifest, routed to the given shrink driver. The OpRunner must never be called.
func setupShrinkEngine(t *testing.T, manifest string, runner run.OpRunner, driver run.ShrinkDriver) (*run.Engine, run.Dirs) {
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
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_shrink.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	matrix, err := ddl.LoadFile(filepath.FromSlash("../../ddl_compatibility.yaml"))
	if err != nil {
		t.Fatalf("load matrix: %v", err)
	}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}
	eng := run.NewEngine(dirs, target, matrix, ddl.Policy{}, fakePreflighter{}, runner,
		run.WithShrinkRunner(driver))
	return eng, dirs
}

const shrinkDataManifest = `
description: shrink data
operations:
  - operation: shrink
    type: data
    files: MyDb_Data
    targetfreespace: 10%
`

const shrinkLogManifest = `
description: shrink log
operations:
  - operation: shrink
    type: log
    files: MyDb_Log
    targetfreespace: 50MB
`

func TestProcessAllRoutesShrinkToDriver(t *testing.T) {
	runner := &fakeOpRunner{}
	driver := &fakeShrinkDriver{
		results: []run.ShrinkResult{
			{File: "MyDb_Data", Type: "data", InitialMB: 1000, FinalMB: 440, TargetMB: 440, Chunks: 4},
		},
		emit: []run.ReactionEvent{{Kind: "pause", Detail: "log over cap"}},
	}
	eng, dirs := setupShrinkEngine(t, shrinkDataManifest, runner, driver)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Done != 1 {
		t.Errorf("Summary = %+v, want Done:1", sum)
	}
	if driver.calls != 1 {
		t.Errorf("shrink driver calls = %d, want 1", driver.calls)
	}
	if runner.calls != 0 {
		t.Errorf("OpRunner calls = %d, want 0 (shrink must not go through the OpRunner)", runner.calls)
	}

	log := readDoneLog(t, dirs, "010_shrink.yaml.log")
	for _, want := range []string{
		"shrink MyDb_Data (data): 1000 MB -> 440 MB (gained 560 MB)",
		"4 chunks",
		"reaction: pause",
		`"gained_mb": 560`, // JSON block carries the structured outcome
	} {
		if !strings.Contains(log, want) {
			t.Errorf("run log missing %q\n--- log ---\n%s", want, log)
		}
	}
}

func TestProcessAllShrinkLogRouting(t *testing.T) {
	runner := &fakeOpRunner{}
	driver := &fakeShrinkDriver{
		results: []run.ShrinkResult{
			{File: "MyDb_Log", Type: "log", InitialMB: 800, FinalMB: 150, TargetMB: 150},
		},
	}
	eng, dirs := setupShrinkEngine(t, shrinkLogManifest, runner, driver)

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if driver.gotType != "log" {
		t.Errorf("driver received type %q, want log", driver.gotType)
	}
	log := readDoneLog(t, dirs, "010_shrink.yaml.log")
	if !strings.Contains(log, "shrink MyDb_Log (log): 800 MB -> 150 MB (gained 650 MB)") {
		t.Errorf("run log missing the log-shrink summary\n--- log ---\n%s", log)
	}
}

func readDoneLog(t *testing.T, dirs run.Dirs, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dirs.Done, name))
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	return string(b)
}
