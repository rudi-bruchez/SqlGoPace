package run_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

func newTestQueue(t *testing.T) (*run.Queue, run.Dirs) {
	t.Helper()
	root := t.TempDir()
	dirs := run.Dirs{
		ToRun:      filepath.Join(root, "01.to_run"),
		Processing: filepath.Join(root, "02.processing"),
		Done:       filepath.Join(root, "03.done"),
		Failed:     filepath.Join(root, "04.failed"),
	}
	q := run.NewQueue(dirs)
	if err := q.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error = %v", err)
	}
	return q, dirs
}

func writeFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestQueueDiscoverSorted(t *testing.T) {
	q, dirs := newTestQueue(t)
	writeFile(t, dirs.ToRun, "020_b.yaml")
	writeFile(t, dirs.ToRun, "010_a.yaml")
	writeFile(t, dirs.ToRun, "notes.txt")   // ignored: not a manifest extension
	writeFile(t, dirs.ToRun, ".010_a.yaml") // ignored: hidden / disabled by leading dot
	writeFile(t, dirs.ToRun, ".gitkeep")    // ignored: dotfile

	got, err := q.Discover()
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	want := []string{"010_a.yaml", "020_b.yaml"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Discover() mismatch (-want +got):\n%s", diff)
	}
}

func TestQueueLifecycle(t *testing.T) {
	q, dirs := newTestQueue(t)
	writeFile(t, dirs.ToRun, "010_a.yaml")

	processingPath, err := q.Claim("010_a.yaml")
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if _, err := os.Stat(processingPath); err != nil {
		t.Errorf("claimed file not in processing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs.ToRun, "010_a.yaml")); !os.IsNotExist(err) {
		t.Errorf("file still in to_run after Claim")
	}

	if err := q.Complete("010_a.yaml"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs.Done, "010_a.yaml")); err != nil {
		t.Errorf("file not in done after Complete: %v", err)
	}
}

func TestQueueFail(t *testing.T) {
	q, dirs := newTestQueue(t)
	writeFile(t, dirs.ToRun, "010_a.yaml")
	if _, err := q.Claim("010_a.yaml"); err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if err := q.Fail("010_a.yaml"); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs.Failed, "010_a.yaml")); err != nil {
		t.Errorf("file not in failed after Fail: %v", err)
	}
}
