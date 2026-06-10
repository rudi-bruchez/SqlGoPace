package run

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestDecideRecovery(t *testing.T) {
	tests := []struct {
		name string
		f    RecoveryFacts
		want RecoveryAction
	}{
		{"orphan alive -> adopt", RecoveryFacts{OrphanAlive: true}, Adopt},
		{"resumable exists -> resume", RecoveryFacts{ResumableExists: true}, Resume},
		{"orphan wins over resumable", RecoveryFacts{OrphanAlive: true, ResumableExists: true}, Adopt},
		{"nothing -> restart", RecoveryFacts{}, Restart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideRecovery(tt.f); got != tt.want {
				t.Errorf("DecideRecovery(%+v) = %v, want %v", tt.f, got, tt.want)
			}
		})
	}
}

func TestMatchesOrphan(t *testing.T) {
	st := RunState{SPID: 57, LoginTime: "2026-06-10T12:00:00", Marker: "0x0123"}

	tests := []struct {
		name string
		id   mssql.SessionIdentity
		want bool
	}{
		{"match", mssql.SessionIdentity{Exists: true, LoginTime: "2026-06-10T12:00:00", ContextInfo: "0x012300"}, true},
		{"no session", mssql.SessionIdentity{Exists: false}, false},
		{"reused spid different login", mssql.SessionIdentity{Exists: true, LoginTime: "2026-06-10T13:00:00", ContextInfo: "0x012300"}, false},
		{"marker mismatch", mssql.SessionIdentity{Exists: true, LoginTime: "2026-06-10T12:00:00", ContextInfo: "0x9999"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesOrphan(tt.id, st); got != tt.want {
				t.Errorf("matchesOrphan() = %t, want %t", got, tt.want)
			}
		})
	}
}

type fakeRecoveryProbe struct {
	id  mssql.SessionIdentity
	ops []mssql.ResumableOp
}

func (f fakeRecoveryProbe) SessionIdentity(context.Context, int) (mssql.SessionIdentity, error) {
	return f.id, nil
}
func (f fakeRecoveryProbe) ResumableOps(context.Context) ([]mssql.ResumableOp, error) {
	return f.ops, nil
}

func setupRecovery(t *testing.T, st RunState) (Dirs, string) {
	t.Helper()
	root := t.TempDir()
	dirs := Dirs{
		ToRun:      filepath.Join(root, "01.to_run"),
		Processing: filepath.Join(root, "02.processing"),
		Done:       filepath.Join(root, "03.done"),
		Failed:     filepath.Join(root, "04.failed"),
	}
	for _, d := range []string{dirs.ToRun, dirs.Processing, dirs.Done, dirs.Failed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	name := "010_a.yaml"
	if err := os.WriteFile(filepath.Join(dirs.Processing, name), []byte("x"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := WriteState(filepath.Join(dirs.Processing, name+stateSuffix), st); err != nil {
		t.Fatalf("write state: %v", err)
	}
	return dirs, name
}

func TestRecovererRequeuesOnRestart(t *testing.T) {
	st := RunState{SPID: 57, LoginTime: "2026-06-10T12:00:00"}
	dirs, name := setupRecovery(t, st)

	// No live session, no resumable op -> Restart -> requeue.
	probe := fakeRecoveryProbe{id: mssql.SessionIdentity{Exists: false}}
	r := NewRecoverer(dirs, probe, io.Discard)

	sum, err := r.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if sum.Requeued != 1 {
		t.Errorf("Requeued = %d, want 1", sum.Requeued)
	}
	if _, err := os.Stat(filepath.Join(dirs.ToRun, name)); err != nil {
		t.Errorf("manifest not requeued to to_run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs.Processing, name+stateSuffix)); !os.IsNotExist(err) {
		t.Errorf("sidecar still present after requeue")
	}
}

func TestRecovererAdoptsLiveOrphan(t *testing.T) {
	st := RunState{SPID: 57, LoginTime: "2026-06-10T12:00:00"}
	dirs, name := setupRecovery(t, st)

	// Live session matches -> orphan alive -> adopt -> leave in processing.
	probe := fakeRecoveryProbe{id: mssql.SessionIdentity{Exists: true, LoginTime: "2026-06-10T12:00:00"}}
	r := NewRecoverer(dirs, probe, io.Discard)

	sum, err := r.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if sum.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", sum.Adopted)
	}
	if _, err := os.Stat(filepath.Join(dirs.Processing, name)); err != nil {
		t.Errorf("adopted manifest should stay in processing: %v", err)
	}
}
