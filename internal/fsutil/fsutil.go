// Package fsutil holds small filesystem helpers shared across packages.
package fsutil

import (
	"os"
	"path/filepath"
)

// AtomicWrite writes data to a temp file in path's directory and renames it over path, so a
// reader — or a crash — sees either the old or the new complete file, never a partial write.
// os.Rename replaces the destination on both POSIX and Windows. A crash between CreateTemp and
// Rename can leave a ".sqlgopace-*.tmp" file behind; callers that own a directory sweep those.
func AtomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".sqlgopace-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
