package run_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// drainAfterFailureRunner fails the first operation and requests a drain while running the
// second, so the engine quarantines op 1 (continue-on-failure) and then stops gracefully at
// the top of the next iteration — the shape of a windowed campaign that loses its window
// after some operations were quarantined.
type drainAfterFailureRunner struct {
	drain *run.DrainFlag
	calls int
}

func (r *drainAfterFailureRunner) Run(context.Context, ddl.Operation, string, run.Capabilities, run.ReactionSink) error {
	r.calls++
	switch r.calls {
	case 1:
		return io.ErrUnexpectedEOF
	case 2:
		r.drain.Request()
	}
	return nil
}

// TestInterruptedRunWritesLogWithQuarantinedOps covers the observability hole on the
// graceful-stop paths: an interrupted run left the manifest in processing but wrote no
// report at all, so operations quarantined before the stop survived only in the optional
// SQLite history.
func TestInterruptedRunWritesLogWithQuarantinedOps(t *testing.T) {
	drain := &run.DrainFlag{}
	runner := &drainAfterFailureRunner{drain: drain}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithDrainSignal(drain.Draining))
	writeOnly(t, dirs, "100_c.yaml", continueManifest)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Interrupted != 1 {
		t.Errorf("Summary = %+v, want Interrupted:1", sum)
	}

	// The manifest stays in processing to resume, and its report is now next to it.
	mustExist(t, filepath.Join(dirs.Processing, "100_c.yaml"))
	logPath := filepath.Join(dirs.Processing, "100_c.yaml.log")
	mustExist(t, logPath)

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "INTERRUPTED") {
		t.Errorf("log missing INTERRUPTED outcome\n%s", log)
	}
	if !strings.Contains(log, "quarantined") {
		t.Errorf("log does not name the quarantined operation — the failure is invisible\n%s", log)
	}

	// No recovery manifest on a graceful stop: the frozen cursor makes the next run retry
	// the quarantined op in place, so a recovery manifest would execute it a second time.
	mustNotExist(t, filepath.Join(dirs.Failed, "100_c.yaml.recovery.yaml"))
}

// TestTerminalRunSweepsInterimLog proves the in-processing report does not outlive the
// manifest: once the run reaches a terminal outcome, the report lives in done/failed only.
func TestTerminalRunSweepsInterimLog(t *testing.T) {
	eng, dirs := setupEngine(t, fakePreflighter{}, &seqOpRunner{})
	writeOnly(t, dirs, "100_c.yaml", continueManifest)

	// Seed a stale interim log as an earlier interrupted run would have left it.
	stale := filepath.Join(dirs.Processing, "100_c.yaml.log")
	if err := os.WriteFile(stale, []byte("-- stale interrupted report\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	mustNotExist(t, stale)
	mustExist(t, filepath.Join(dirs.Done, "100_c.yaml.log"))
}
