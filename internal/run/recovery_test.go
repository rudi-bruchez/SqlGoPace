package run

import (
	"context"
	"errors"
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
	st := State{SPID: 57, LoginTime: "2026-06-10T12:00:00", Marker: "0x0123"}

	tests := []struct {
		name string
		id   mssql.SessionIdentity
		want bool
	}{
		{"match", mssql.SessionIdentity{Exists: true, Active: true, LoginTime: "2026-06-10T12:00:00", ContextInfo: "0x012300"}, true},
		{"no session", mssql.SessionIdentity{Exists: false}, false},
		{"idle session no request", mssql.SessionIdentity{Exists: true, Active: false, LoginTime: "2026-06-10T12:00:00", ContextInfo: "0x012300"}, false},
		{"reused spid different login", mssql.SessionIdentity{Exists: true, Active: true, LoginTime: "2026-06-10T13:00:00", ContextInfo: "0x012300"}, false},
		{"marker mismatch", mssql.SessionIdentity{Exists: true, Active: true, LoginTime: "2026-06-10T12:00:00", ContextInfo: "0x9999"}, false},
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

func setupRecovery(t *testing.T, st State) (Dirs, string) {
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

func TestRecoverSweepsStrayTempFiles(t *testing.T) {
	// #13: a crash between CreateTemp and Rename can leave a hidden .sqlgopace-*.tmp in
	// processing; recovery must sweep it.
	st := State{SPID: 57, LoginTime: "2026-06-10T12:00:00"}
	dirs, _ := setupRecovery(t, st)
	tmp := filepath.Join(dirs.Processing, ".sqlgopace-abc123.tmp")
	if err := os.WriteFile(tmp, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRecoverer(dirs, fakeRecoveryProbe{id: mssql.SessionIdentity{Exists: false}}, io.Discard)
	if _, err := r.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("stray temp file was not swept (err=%v)", err)
	}
}

func TestRecovererRequeuesOnRestart(t *testing.T) {
	st := State{SPID: 57, LoginTime: "2026-06-10T12:00:00"}
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

func TestRecovererKeepsCursorOnRequeue(t *testing.T) {
	// A drained run left a resume cursor: requeue must move the manifest back to
	// to_run but KEEP the sidecar, so the re-run resumes at the cursor.
	st := State{SPID: 57, LoginTime: "2026-06-10T12:00:00", ResumeFromOp: 3}
	dirs, name := setupRecovery(t, st)

	probe := fakeRecoveryProbe{id: mssql.SessionIdentity{Exists: false}} // no live session → Restart
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
	got, err := ReadState(filepath.Join(dirs.Processing, name+stateSuffix))
	if err != nil {
		t.Fatalf("sidecar with cursor should be kept after requeue: %v", err)
	}
	if got.ResumeFromOp != 3 {
		t.Errorf("kept sidecar ResumeFromOp = %d, want 3", got.ResumeFromOp)
	}
}

func TestRecovererKeepsSidecarForPausedResumable(t *testing.T) {
	// Interrupted during the first operation (cursor 0) but a resumable is paused on the
	// server: the sidecar must survive the requeue so the re-run recognizes the resume and
	// issues ALTER INDEX … RESUME instead of a fresh REBUILD that SQL Server would reject.
	st := State{SPID: 57, LoginTime: "2026-06-10T12:00:00"} // ResumeFromOp 0
	dirs, name := setupRecovery(t, st)

	// Dead session (not adopted) + a paused resumable → DecideRecovery = Resume.
	probe := fakeRecoveryProbe{
		id:  mssql.SessionIdentity{Exists: false},
		ops: []mssql.ResumableOp{{StateDesc: "PAUSED"}},
	}
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
	if _, err := os.Stat(filepath.Join(dirs.Processing, name+stateSuffix)); err != nil {
		t.Errorf("sidecar should be kept for a paused resumable despite cursor 0: %v", err)
	}
}

func TestRecovererRoutesToOrphanDatabase(t *testing.T) {
	// An orphan from DB2 must be reconciled against the DB2 probe, not the
	// connected one. The connected probe would Adopt (alive); the DB2 probe sees
	// no session → Restart → requeue. The outcome proves which probe was used.
	st := State{SPID: 57, Database: "DB2", LoginTime: "2026-06-10T12:00:00"}
	dirs, _ := setupRecovery(t, st)
	connected := fakeRecoveryProbe{id: mssql.SessionIdentity{Exists: true, Active: true, LoginTime: "2026-06-10T12:00:00"}}
	db2 := fakeRecoveryProbe{id: mssql.SessionIdentity{Exists: false}}

	var resolvedFor string
	r := NewRecoverer(dirs, connected, io.Discard, WithRecoveryProbes("CONN",
		func(_ context.Context, db string) (RecoveryProbe, func(), error) {
			resolvedFor = db
			return db2, func() {}, nil
		}))

	sum, err := r.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if resolvedFor != "DB2" {
		t.Errorf("resolver called for %q, want DB2", resolvedFor)
	}
	if sum.Requeued != 1 || sum.Adopted != 0 {
		t.Errorf("sum = %+v, want Requeued 1 / Adopted 0 (DB2 probe → Restart)", sum)
	}
}

func TestRecovererConnectedDatabaseUsesBaseProbe(t *testing.T) {
	// An orphan from the connected database uses the base probe; the resolver is
	// never called.
	st := State{SPID: 57, Database: "CONN", LoginTime: "2026-06-10T12:00:00"}
	dirs, _ := setupRecovery(t, st)
	connected := fakeRecoveryProbe{id: mssql.SessionIdentity{Exists: true, Active: true, LoginTime: "2026-06-10T12:00:00"}}

	called := false
	r := NewRecoverer(dirs, connected, io.Discard, WithRecoveryProbes("CONN",
		func(context.Context, string) (RecoveryProbe, func(), error) {
			called = true
			return nil, nil, nil
		}))

	sum, err := r.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if called {
		t.Errorf("resolver must not be called for the connected database")
	}
	if sum.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1 (base probe, live orphan)", sum.Adopted)
	}
}

func TestRecovererUnreachableDatabaseLeavesOrphan(t *testing.T) {
	// If the orphan's database can't be reached (e.g. now an AG secondary), the
	// orphan is left in processing for a later run, not requeued or adopted.
	st := State{SPID: 57, Database: "DB2", LoginTime: "x"}
	dirs, name := setupRecovery(t, st)

	r := NewRecoverer(dirs, fakeRecoveryProbe{}, io.Discard, WithRecoveryProbes("CONN",
		func(context.Context, string) (RecoveryProbe, func(), error) {
			return nil, nil, errors.New("availability-group secondary")
		}))

	sum, err := r.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if sum.Requeued != 0 || sum.Adopted != 0 {
		t.Errorf("sum = %+v, want all zero (left in processing)", sum)
	}
	if _, serr := os.Stat(filepath.Join(dirs.Processing, name+stateSuffix)); serr != nil {
		t.Errorf("orphan sidecar should remain in processing: %v", serr)
	}
}

func TestRecovererAdoptsLiveOrphan(t *testing.T) {
	st := State{SPID: 57, LoginTime: "2026-06-10T12:00:00"}
	dirs, name := setupRecovery(t, st)

	// Live session matches and is executing a request -> orphan alive -> adopt -> leave in processing.
	probe := fakeRecoveryProbe{id: mssql.SessionIdentity{Exists: true, Active: true, LoginTime: "2026-06-10T12:00:00"}}
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

func TestRecovererIdleOrphanRestarts(t *testing.T) {
	// Regression: after a crash the session lingers with the recorded signature but runs
	// nothing (sleeping, no request). It must not be mistaken for a running operation:
	// with no resumable, the manifest is requeued for a fresh run, not stranded.
	st := State{SPID: 57, LoginTime: "2026-06-10T12:00:00"}
	dirs, name := setupRecovery(t, st)

	probe := fakeRecoveryProbe{id: mssql.SessionIdentity{Exists: true, Active: false, LoginTime: "2026-06-10T12:00:00"}}
	r := NewRecoverer(dirs, probe, io.Discard)

	sum, err := r.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if sum.Requeued != 1 || sum.Adopted != 0 {
		t.Errorf("sum = %+v, want Requeued 1 / Adopted 0 (idle orphan -> restart)", sum)
	}
	if _, err := os.Stat(filepath.Join(dirs.ToRun, name)); err != nil {
		t.Errorf("idle-orphan manifest should be requeued to to_run: %v", err)
	}
}
