package run

import (
	"context"
	"crypto/sha256"
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
//
// fingerprint is the walk the position belongs to. A position only means anything
// against the statement that produced it, and the plan fingerprint the resume cursor is
// checked against hashes command and target only: editing set, where or the batch's
// options leaves it identical, and it is compared only when the cursor is past zero,
// which a run interrupted during its first operation never is. Both gaps land here, so
// the check lives here too.
type watermarkFile struct {
	path        string
	fingerprint string
}

// contentFingerprint identifies a walk by the statement it runs, which is what decides
// which rows it touches. Hashing the generated SQL rather than the operation struct keeps
// it honest about the thing that actually differs: two manifests that resolve to the same
// statement are the same walk, whatever their YAML looked like.
func contentFingerprint(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return fmt.Sprintf("%x", sum)
}

// Load reads the persisted watermark, reporting ok=false when there is no usable one:
// no file yet (a fresh run), or one whose fingerprint does not match this walk.
//
// A mismatch is not an error. It is the normal outcome of editing a manifest between an
// interruption and a re-run, and the answer is to start the walk over rather than resume
// behind a position recorded against different SQL — which would silently skip every row
// below it. A file with no fingerprint is one written before 0.42.0: unverifiable, which
// is not the same as matching, so it is treated the same way. key_range requires an
// idempotent literal SET, so restarting redoes work rather than corrupting it.
func (w watermarkFile) Load(context.Context) (int64, bool, error) {
	b, err := os.ReadFile(w.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read watermark %q: %w", w.path, err)
	}
	value, fp, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	v, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse watermark %q: %w", w.path, err)
	}
	if strings.TrimSpace(fp) != w.fingerprint {
		return 0, false, nil
	}
	return v, true, nil
}

// Save atomically writes the watermark (temp file + rename), so a crash mid-write
// never leaves a torn value. The value keeps its own first line, so the file still reads
// as a number to anything that looks at it.
func (w watermarkFile) Save(_ context.Context, watermark int64) error {
	body := strconv.FormatInt(watermark, 10) + "\n" + w.fingerprint + "\n"
	return fsutil.AtomicWrite(w.path, []byte(body))
}

// clear removes the watermark file once a walk has returned (the manifest is being
// finalized to done/failed). A crash skips this, leaving the file for the resume.
func (w watermarkFile) clear() { _ = os.Remove(w.path) }

// watermarkStore returns the sidecar store for the i-th operation of a manifest. The
// operation index is stable across runs (expansion is deterministic), so a resumed
// run reads the same file; sql binds the position to the walk that produced it.
func (e *Engine) watermarkStore(name string, i int, sql string) watermarkFile {
	return watermarkFile{
		path:        filepath.Join(e.dirs.Processing, fmt.Sprintf("%s.op%d.wm", name, i)),
		fingerprint: contentFingerprint(sql),
	}
}
