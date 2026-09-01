package run_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// Three operations, so "between" is distinguishable from "after each".
const threeOpManifest = `
description: checkpoint test
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX1
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX2
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX3
`

func setupThreeOpEngine(t *testing.T, opts ...run.EngineOption) (*run.Engine, run.Dirs) {
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
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(threeOpManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	matrix, err := ddl.LoadFile(filepath.FromSlash("../../ddl_compatibility.yaml"))
	if err != nil {
		t.Fatalf("load matrix: %v", err)
	}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}
	eng := run.NewEngine(dirs, target, matrix, ddl.Policy{}, fakePreflighter{}, &fakeOpRunner{}, opts...)
	return eng, dirs
}

// checkpoint_between_operations was parsed, documented in four places, and read by
// nothing: the field's only appearance in the tree was its own declaration. An operator
// under SIMPLE recovery who set it believed the log was being released between the
// operations of a long manifest. It was not.
func TestCheckpointBetweenOperations(t *testing.T) {
	var calls int
	eng, _ := setupThreeOpEngine(t, run.WithCheckpointBetweenOperations(func(context.Context) error {
		calls++
		return nil
	}))

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	// Between three operations there are two gaps. A checkpoint after the last one has
	// no following operation to protect and would only cost a round trip.
	if calls != 2 {
		t.Errorf("checkpoints = %d, want 2 (between three operations)", calls)
	}
}

// Not wired means not called: the option is the whole switch, so an operator who leaves
// the key false pays nothing.
func TestNoCheckpointWhenNotWired(t *testing.T) {
	eng, dirs := setupThreeOpEngine(t)
	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs.Done, "010_a.yaml.log")); err != nil {
		t.Errorf("manifest should still complete without the option: %v", err)
	}
}

// A CHECKPOINT is an optimization, not part of the operation. Failing one must not fail
// the manifest that was otherwise fine.
func TestCheckpointFailureDoesNotFailTheManifest(t *testing.T) {
	eng, dirs := setupThreeOpEngine(t, run.WithCheckpointBetweenOperations(func(context.Context) error {
		return errors.New("checkpoint refused")
	}))

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Done != 1 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want Done:1 Failed:0 (a failed checkpoint is not a failed manifest)", sum)
	}
	if _, err := os.Stat(filepath.Join(dirs.Done, "010_a.yaml.log")); err != nil {
		t.Errorf("manifest should land in done/: %v", err)
	}
}
