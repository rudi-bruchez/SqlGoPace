package run_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// TestQueueLockExcludesASecondHolder is the whole point: recovery sweeps
// 02.processing/, which is exactly where a live peer's in-flight manifest sits, and
// the liveness test it uses (a row in dm_exec_requests) is absent whenever that peer
// is between statements. A second instance must not get that far.
func TestQueueLockExcludesASecondHolder(t *testing.T) {
	dir := t.TempDir()

	first, err := run.LockQueue(dir)
	if err != nil {
		t.Fatalf("LockQueue: %v", err)
	}
	defer func() { _ = first.Release() }()

	second, err := run.LockQueue(dir)
	if !errors.Is(err, run.ErrQueueLocked) {
		if err == nil {
			_ = second.Release()
		}
		t.Fatalf("second LockQueue error = %v, want ErrQueueLocked", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q does not name the queue directory", err)
	}
	// The holder line must survive being read while the lock is held, or the refusal
	// degrades to "holder unknown" — which is the Windows failure mode, since its
	// locks are mandatory and a locked range cannot be read at all.
	if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("error %q does not name the holding process", err)
	}
}

// TestQueueLockReleases: a clean exit must leave the queue takeable, or the next
// cron tick refuses to run.
func TestQueueLockReleases(t *testing.T) {
	dir := t.TempDir()

	first, err := run.LockQueue(dir)
	if err != nil {
		t.Fatalf("LockQueue: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second, err := run.LockQueue(dir)
	if err != nil {
		t.Fatalf("LockQueue after Release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
}

// TestQueueLockIgnoresAStaleFile is the case an O_EXCL create would get wrong. A
// process killed mid-run leaves the lock file behind, and that is precisely when
// crash recovery has work to do: refusing to start would turn every crash into a
// manual cleanup and defeat the feature the lock is protecting.
func TestQueueLockIgnoresAStaleFile(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, run.QueueLockName)
	if err := os.WriteFile(stale, []byte("pid 4242 on SQLPROD01\n"), 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}

	l, err := run.LockQueue(dir)
	if err != nil {
		t.Fatalf("LockQueue over a stale lock file: %v — a crashed run must not need manual cleanup", err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
}

// TestQueueLockNamesTheHolder: the operator needs to know which process has it, or
// the refusal is just an obstacle.
func TestQueueLockNamesTheHolder(t *testing.T) {
	dir := t.TempDir()
	first, err := run.LockQueue(dir)
	if err != nil {
		t.Fatalf("LockQueue: %v", err)
	}
	defer func() { _ = first.Release() }()

	body, err := os.ReadFile(filepath.Join(dir, run.QueueLockName))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if !strings.Contains(string(body), "pid ") {
		t.Errorf("lock file %q does not record the pid", body)
	}
}

// TestQueueLockCreatesTheDirectory: the lock is taken before the engine builds the
// queue directories, so on a first run the processing directory does not exist yet.
func TestQueueLockCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "02.processing")

	l, err := run.LockQueue(dir)
	if err != nil {
		t.Fatalf("LockQueue on a missing directory: %v", err)
	}
	defer func() { _ = l.Release() }()

	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("processing directory not created: stat err = %v", err)
	}
}
