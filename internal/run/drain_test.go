package run_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// closeOnFirstRunner runs the operation normally, then closes drain on its first call,
// so the engine sees the drain request at the top of the next iteration — modeling a
// drain requested while operation 1 was running.
type closeOnFirstRunner struct {
	drain chan struct{}
	calls int
}

func (r *closeOnFirstRunner) Run(context.Context, ddl.Operation, string, run.Capabilities, run.ReactionSink) error {
	r.calls++
	if r.calls == 1 {
		close(r.drain)
	}
	return nil
}

func TestDrainStopsAfterCurrentOperation(t *testing.T) {
	drain := make(chan struct{})
	runner := &closeOnFirstRunner{drain: drain}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithDrainSignal(drain))
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
	drain := make(chan struct{})
	close(drain) // already requested before the run starts
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithDrainSignal(drain))
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
