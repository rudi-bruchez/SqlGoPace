package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/fsutil"
)

// watermarkFile is a file-backed WatermarkStore: it persists a key_range walk
// position next to the in-processing manifest, so a crash resumes mid-table. The
// file survives a crash (it lives in the processing directory with the manifest);
// the engine removes it once the walk returns. Per the spec, the watermark is held
// in this sidecar, not in the same transaction as the SQL, so a resume is
// at-least-once on the boundary batch — safe for the idempotent literal UPDATE that
// key_range requires.
type watermarkFile struct {
	path string
}

// Load reads the persisted watermark, reporting ok=false when no file exists yet
// (a fresh run, not a resume).
func (w watermarkFile) Load(context.Context) (int64, bool, error) {
	b, err := os.ReadFile(w.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read watermark %q: %w", w.path, err)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse watermark %q: %w", w.path, err)
	}
	return v, true, nil
}

// Save atomically writes the watermark (temp file + rename), so a crash mid-write
// never leaves a torn value.
func (w watermarkFile) Save(_ context.Context, watermark int64) error {
	return fsutil.AtomicWrite(w.path, []byte(strconv.FormatInt(watermark, 10)))
}

// clear removes the watermark file once a walk has returned (the manifest is being
// finalized to done/failed). A crash skips this, leaving the file for the resume.
func (w watermarkFile) clear() { _ = os.Remove(w.path) }

// watermarkStore returns the sidecar store for the i-th operation of a manifest. The
// operation index is stable across runs (expansion is deterministic), so a resumed
// run reads the same file.
func (e *Engine) watermarkStore(name string, i int) watermarkFile {
	return watermarkFile{path: filepath.Join(e.dirs.Processing, fmt.Sprintf("%s.op%d.wm", name, i))}
}
