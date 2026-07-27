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

func (f *fakeShrinkDriver) Run(_ context.Context, op ddl.Shrink, _ ddl.ResolvedOptions, _ run.IgnoreSource, sink run.ReactionSink) ([]run.ShrinkResult, error) {
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
	return setupShrinkEngineOpts(t, manifest, runner, run.WithShrinkRunner(driver))
}

// fakeTempdbShrinkDriver records that it was called and returns scripted results, so
// shrink_tempdb routing can be verified without a database — the tempdb twin of
// fakeShrinkDriver.
type fakeTempdbShrinkDriver struct {
	results []run.ShrinkResult
	err     error
	calls   int
	emit    []run.ReactionEvent
}

func (f *fakeTempdbShrinkDriver) RunTempdb(_ context.Context, _ ddl.ShrinkTempdb, _ ddl.ResolvedOptions, _ run.IgnoreSource, sink run.ReactionSink) ([]run.ShrinkResult, error) {
	f.calls++
	for _, ev := range f.emit {
		sink(ev)
	}
	return f.results, f.err
}

// setupShrinkEngineOpts is setupShrinkEngine generalized over EngineOptions, so the
// tempdb routing tests can wire WithTempdbShrinkRunner instead of WithShrinkRunner.
func setupShrinkEngineOpts(t *testing.T, manifest string, runner run.OpRunner, opts ...run.EngineOption) (*run.Engine, run.Dirs) {
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
	eng := run.NewEngine(dirs, target, matrix, ddl.Policy{}, fakePreflighter{}, runner, opts...)
	return eng, dirs
}

const shrinkTempdbManifest = `
description: shrink tempdb
operations:
  - operation: shrink_tempdb
    targetsizemb: 20480
`

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

func TestProcessAllShrinkIncompleteStopsShort(t *testing.T) {
	runner := &fakeOpRunner{}
	// The shrink finished (no error) but gained nothing and did not reach target: a stall
	// with work preserved. It must be recorded as INCOMPLETE, not a clean success.
	driver := &fakeShrinkDriver{
		results: []run.ShrinkResult{
			{File: "MyDb_Data", Type: "data", InitialMB: 1000, FinalMB: 1000, TargetMB: 440,
				Reason: "no further progress (work preserved)"},
		},
	}
	eng, dirs := setupShrinkEngine(t, shrinkDataManifest, runner, driver)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Incomplete != 1 || sum.Done != 0 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want Incomplete:1 (not done/failed)", sum)
	}
	// The manifest and its log move to failed/, never done/.
	if _, err := os.Stat(filepath.Join(dirs.Done, "010_shrink.yaml.log")); !os.IsNotExist(err) {
		t.Errorf("incomplete shrink must not land in done/ (err=%v)", err)
	}
	b, err := os.ReadFile(filepath.Join(dirs.Failed, "010_shrink.yaml.log"))
	if err != nil {
		t.Fatalf("read failed log: %v", err)
	}
	log := string(b)
	for _, want := range []string{"INCOMPLETE", "stopped short of target", "no further progress (work preserved)"} {
		if !strings.Contains(log, want) {
			t.Errorf("run log missing %q\n--- log ---\n%s", want, log)
		}
	}
	if strings.Contains(log, "SUCCESS") {
		t.Errorf("incomplete run must not be labeled SUCCESS\n--- log ---\n%s", log)
	}
}

func TestProcessAllShrinkNoOpIsSuccess(t *testing.T) {
	// A no-op file (nothing to reclaim) has a reason but is legitimately complete: the
	// target was already satisfied, so the run is a success, not incomplete.
	runner := &fakeOpRunner{}
	driver := &fakeShrinkDriver{
		results: []run.ShrinkResult{
			{File: "MyDb_Data", Type: "data", InitialMB: 440, FinalMB: 440, TargetMB: 440,
				NoOp: true, Reason: "nothing to reclaim"},
		},
	}
	eng, dirs := setupShrinkEngine(t, shrinkDataManifest, runner, driver)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Done != 1 || sum.Incomplete != 0 {
		t.Errorf("Summary = %+v, want Done:1 (a no-op is complete)", sum)
	}
	if _, err := os.Stat(filepath.Join(dirs.Done, "010_shrink.yaml.log")); err != nil {
		t.Errorf("no-op shrink should land in done/: %v", err)
	}
}

func TestProcessAllRoutesShrinkTempdbToDriver(t *testing.T) {
	runner := &fakeOpRunner{}
	driver := &fakeTempdbShrinkDriver{
		results: []run.ShrinkResult{
			{File: "tempdev", Type: "data", InitialMB: 40960, FinalMB: 20480, TargetMB: 20480, Chunks: 3},
		},
	}
	eng, dirs := setupShrinkEngineOpts(t, shrinkTempdbManifest, runner, run.WithTempdbShrinkRunner(driver))

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Done != 1 {
		t.Errorf("Summary = %+v, want Done:1", sum)
	}
	if driver.calls != 1 {
		t.Errorf("tempdb shrink driver calls = %d, want 1", driver.calls)
	}
	if runner.calls != 0 {
		t.Errorf("OpRunner calls = %d, want 0 (shrink_tempdb must not go through the OpRunner)", runner.calls)
	}
	if _, err := os.Stat(filepath.Join(dirs.Done, "010_shrink.yaml.log")); err != nil {
		t.Errorf("shrink_tempdb success should land in done/: %v", err)
	}
}

func TestProcessAllShrinkTempdbIncompleteStopsShort(t *testing.T) {
	runner := &fakeOpRunner{}
	// The tempdb shrink finished (no error) but a file stopped short of target: not a
	// clean success — it must route to INCOMPLETE, exactly like a plain shrink.
	driver := &fakeTempdbShrinkDriver{
		results: []run.ShrinkResult{
			{File: "tempdev", Type: "data", InitialMB: 40960, FinalMB: 40960, TargetMB: 20480,
				Reason: "no further progress (work preserved)"},
		},
	}
	eng, dirs := setupShrinkEngineOpts(t, shrinkTempdbManifest, runner, run.WithTempdbShrinkRunner(driver))

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Incomplete != 1 || sum.Done != 0 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want Incomplete:1 (not done/failed)", sum)
	}
	b, err := os.ReadFile(filepath.Join(dirs.Failed, "010_shrink.yaml.log"))
	if err != nil {
		t.Fatalf("read failed log: %v", err)
	}
	log := string(b)
	for _, want := range []string{"INCOMPLETE", "stopped short of target", "no further progress (work preserved)"} {
		if !strings.Contains(log, want) {
			t.Errorf("run log missing %q\n--- log ---\n%s", want, log)
		}
	}
}

func TestProcessAllShrinkTempdbWithoutDriverFails(t *testing.T) {
	// No WithTempdbShrinkRunner wired: shrink_tempdb has no OpRunner fallback (unlike a
	// plain shrink, whose indicative SQL is at least runnable), so it must fail loudly
	// rather than silently no-op.
	runner := &fakeOpRunner{}
	eng, dirs := setupShrinkEngineOpts(t, shrinkTempdbManifest, runner)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 1 {
		t.Errorf("Summary = %+v, want Failed:1 (no tempdb shrink driver wired)", sum)
	}
	b, err := os.ReadFile(filepath.Join(dirs.Failed, "010_shrink.yaml.log"))
	if err != nil {
		t.Fatalf("read failed log: %v", err)
	}
	if !strings.Contains(string(b), "not configured") {
		t.Errorf("run log missing the not-configured reason\n--- log ---\n%s", string(b))
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
