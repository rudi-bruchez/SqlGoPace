//go:build windows

package run

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockRegion is the byte range LockFileEx claims: one byte at offset 2^32, far past
// the holder line at the start of the file. Windows locks are mandatory, not advisory
// — a range locked by one process cannot even be READ by another — so locking the
// whole file would make the holder line unreadable and turn the refusal into "holder
// unknown". Locking past the data keeps it readable while still excluding.
func lockRegion() *windows.Overlapped {
	return &windows.Overlapped{Offset: 0, OffsetHigh: 1}
}

// tryLockFile takes a non-blocking exclusive lock on that range. Windows releases it
// when the handle closes, including on an abnormal process exit, so a crashed run
// leaves a stale file and no stale lock.
func tryLockFile(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, lockRegion())
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
		return false, nil
	default:
		return false, err
	}
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockRegion())
}
