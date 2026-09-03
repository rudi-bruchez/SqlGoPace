package run

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// QueueLockName is the lock file kept in the processing directory for the lifetime of
// a run. Its contents are advisory — who holds it, for a human reading the refusal —
// while the exclusion itself is an OS-level lock on the open file (see lock_unix.go /
// lock_windows.go), so a process that dies without releasing it leaves nothing to
// clean up. That matters more here than anywhere else: a crash is exactly when the
// next run has recovery work to do, and a lock needing manual removal would turn the
// recovery feature off at the moment it is needed.
const QueueLockName = ".sqlgopace.lock"

// ErrQueueLocked is returned when another SqlGoPace process holds the queue.
var ErrQueueLocked = errors.New("another SqlGoPace run holds this queue")

// QueueLock is a held queue lock. Release it once, at process exit.
type QueueLock struct {
	f *os.File
}

// LockQueue takes the exclusive lock on a processing directory, which is what makes
// crash recovery safe to run.
//
// Recovery sweeps the processing directory before anything is claimed, and decides an
// orphan is dead by looking for a row in sys.dm_exec_requests. A live instance has no
// such row whenever it is between statements — awaiting relief after a pressure pause,
// between shrink chunks, between DML batches, between operations — so a second run
// starting in any of those windows requeues a peer's in-flight manifest and then runs
// it. Two offline rebuilds of one index is the mild outcome; with
// abort_blocking_resumable the second run ABORTs the first's paused build, which
// SQL Server documents as unresumable.
//
// The code carried that assumption in a comment ("the tool does not support concurrent
// instances on one queue") and did not enforce it, which left the operator no way to
// know they had violated it. Cron overlap is not exotic.
//
// The lock is per processing directory, not per database: the race being closed is the
// recovery sweep, and two runs on separate queues cannot sweep each other. It relies on
// the filesystem honoring locks, so a queue on an NFSv3 share is not protected.
func LockQueue(dir string) (*QueueLock, error) {
	// The lock is taken before the engine builds the queue, so on a first run the
	// directory is not there yet. Same mode as Queue.EnsureDirs, which creates the
	// other three.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create processing directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, QueueLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open queue lock %s: %w", path, err)
	}
	held, err := tryLockFile(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock queue %s: %w", path, err)
	}
	if !held {
		holder := readHolder(f)
		_ = f.Close()
		return nil, fmt.Errorf("%s (%s): %s: %w", dir, path, holder, ErrQueueLocked)
	}
	// Only now is the file ours to write: the previous holder's line stays readable
	// right up to the moment we win the lock, which is what makes the refusal above
	// name a process rather than a path.
	if err := writeHolder(f); err != nil {
		_ = unlockFile(f)
		_ = f.Close()
		return nil, err
	}
	return &QueueLock{f: f}, nil
}

// Release drops the lock and removes the file. Removing it is cosmetic — the OS lock
// is what excludes — so a failure to remove is not reported.
func (l *QueueLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	name := l.f.Name()
	err := unlockFile(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	_ = os.Remove(name)
	return err
}

// writeHolder records who holds the lock, for the message the next process prints.
func writeHolder(f *os.File) error {
	host, _ := os.Hostname()
	line := fmt.Sprintf("pid %d on %s since %s\n", os.Getpid(), host, time.Now().Format(time.RFC3339))
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("write queue lock: %w", err)
	}
	if _, err := f.WriteAt([]byte(line), 0); err != nil {
		return fmt.Errorf("write queue lock: %w", err)
	}
	return nil
}

// readHolder returns the holder line, or a placeholder when it cannot be read — an
// unreadable line must never turn a clean refusal into a different error.
func readHolder(f *os.File) string {
	buf := make([]byte, 256)
	n, _ := f.ReadAt(buf, 0)
	if n == 0 {
		return "holder unknown"
	}
	return string(trimLine(buf[:n]))
}

func trimLine(b []byte) []byte {
	for i, c := range b {
		if c == '\n' || c == '\r' || c == 0 {
			return b[:i]
		}
	}
	return b
}
