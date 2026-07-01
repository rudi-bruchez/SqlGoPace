package run_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// closeOnFirstRunner runs the operation normally, then requests a drain on its first call,
// so the engine sees the drain request at the top of the next iteration — modeling a
// drain requested while operation 1 was running.
type closeOnFirstRunner struct {
	drain *run.DrainFlag
	calls int
}

func (r *closeOnFirstRunner) Run(context.Context, ddl.Operation, string, run.Capabilities, run.ReactionSink) error {
	r.calls++
	if r.calls == 1 {
		r.drain.Request()
	}
	return nil
}

// stopOnRunRunner simulates Ctrl+C arriving while a resumable operation runs: it requests
// the drain and, seeing the stop predicate true in its capabilities, returns ErrStopped
// exactly as the real MonitoredRunner does after pausing the resumable.
type stopOnRunRunner struct {
	drain *run.DrainFlag
	calls int
}

func (r *stopOnRunRunner) Run(_ context.Context, _ ddl.Operation, _ string, caps run.Capabilities, _ run.ReactionSink) error {
	r.calls++
	r.drain.Request()
	if caps.Stop != nil && caps.Stop() {
		return run.ErrStopped
	}
	return nil
}

// drainThenCancelRunner requests a drain on its first call and immediately cancels it, so
// the engine's next top-of-loop check sees no drain and the run completes — modeling an
// operator who changes their mind before the current operation finishes.
type drainThenCancelRunner struct {
	drain *run.DrainFlag
	calls int
}

func (r *drainThenCancelRunner) Run(context.Context, ddl.Operation, string, run.Capabilities, run.ReactionSink) error {
	r.calls++
	if r.calls == 1 {
		r.drain.Request()
		r.drain.Cancel()
	}
	return nil
}

func TestDrainPausesRunningResumable(t *testing.T) {
	drain := &run.DrainFlag{}
	runner := &stopOnRunRunner{drain: drain}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithDrainSignal(drain.Draining),
		run.WithSession(fakeSession{spid: 70}))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("runner ran %d ops, want 1 (paused on the first; the second never starts)", runner.calls)
	}
	if sum.Interrupted != 1 || sum.Done != 0 {
		t.Errorf("Summary = %+v, want Interrupted:1 Done:0", sum)
	}
	// Paused mid-operation: the manifest and sidecar stay in processing for the next run
	// to continue via ALTER INDEX … RESUME.
	mustExist(t, filepath.Join(dirs.Processing, "010_a.yaml"))
	mustExist(t, filepath.Join(dirs.Processing, "010_a.yaml.state.json"))
	mustNotExist(t, filepath.Join(dirs.Done, "010_a.yaml"))
}

func TestDrainStopsAfterCurrentOperation(t *testing.T) {
	drain := &run.DrainFlag{}
	runner := &closeOnFirstRunner{drain: drain}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithDrainSignal(drain.Draining))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("runner ran %d ops, want 1 (the current op finishes, the next does not start)", runner.calls)
	}
	if sum.Interrupted != 1 || sum.Done != 0 {
		t.Errorf("Summary = %+v, want Interrupted:1 Done:0", sum)
	}
	// A drained manifest stays in processing for the next run to resume; it is not
	// moved to done or failed.
	mustExist(t, filepath.Join(dirs.Processing, "010_a.yaml"))
	mustNotExist(t, filepath.Join(dirs.Done, "010_a.yaml"))
	mustNotExist(t, filepath.Join(dirs.Failed, "010_a.yaml"))
}

func TestDrainBeforeAnyManifestRunsNothing(t *testing.T) {
	drain := &run.DrainFlag{}
	drain.Request() // already requested before the run starts
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithDrainSignal(drain.Draining))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 0 {
		t.Errorf("runner ran %d ops, want 0 (drained before starting)", runner.calls)
	}
}

func TestDrainCancelledLetsRunComplete(t *testing.T) {
	drain := &run.DrainFlag{}
	runner := &drainThenCancelRunner{drain: drain}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithDrainSignal(drain.Draining))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	// The drain was canceled before the next operation, so both ops run and the manifest
	// completes normally.
	if runner.calls != 2 {
		t.Errorf("runner ran %d ops, want 2 (drain canceled — run completes)", runner.calls)
	}
	if sum.Done != 1 || sum.Interrupted != 0 {
		t.Errorf("Summary = %+v, want Done:1 Interrupted:0", sum)
	}
	mustExist(t, filepath.Join(dirs.Done, "010_a.yaml"))
}
