package run_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	drain := make(chan struct{})
	runner := &closeOnFirstRunner{drain: drain}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithDrainSignal(drain),
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
