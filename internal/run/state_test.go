package run_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

func TestRunStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "010_a.yaml.state.json")
	state := run.State{
		Manifest:  "010_a.yaml",
		SPID:      57,
		Marker:    "0x0123456789abcdeffedcba9876543210",
		Command:   "ALTER INDEX [IX] ON [dbo].[T] REBUILD;",
		StartedAt: "2026-06-10T12:00:00Z",
	}

	if err := run.WriteState(path, state); err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}
	got, err := run.ReadState(path)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if diff := cmp.Diff(state, got); diff != "" {
		t.Errorf("state round-trip mismatch (-want +got):\n%s", diff)
	}

	if err := run.RemoveState(path); err != nil {
		t.Fatalf("RemoveState() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state file still exists after RemoveState")
	}
}
