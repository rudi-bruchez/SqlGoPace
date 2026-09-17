package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	firstSQL  = "UPDATE dbo.MEASUREMENT SET Archived = 1 WHERE Archived = 0 AND Id > @from AND Id <= @to"
	editedSQL = "UPDATE dbo.MEASUREMENT SET Archived = 1 WHERE Archived = 0 AND Region = 'EU' AND Id > @from AND Id <= @to"
)

func tempWatermark(t *testing.T, sql string) watermarkFile {
	t.Helper()
	return watermarkFile{path: filepath.Join(t.TempDir(), "m.yaml.op0.wm"), fingerprint: contentFingerprint(sql)}
}

// A watermark is a position in a walk, and a position only means anything against the walk
// that produced it. planFingerprint hashes command and target only, so editing set, where
// or the batch's options leaves it unchanged; and it is compared only when the resume
// cursor is past zero, which a run interrupted during its first operation never is. Both
// gaps land on the same place: a key_range walk resuming behind a watermark recorded
// against different SQL, silently skipping every row below it.
func TestWatermarkIsIgnoredWhenTheStatementChanged(t *testing.T) {
	w := tempWatermark(t, firstSQL)
	if err := w.Save(context.Background(), 5000); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Same manifest, same operation index, edited predicate.
	edited := watermarkFile{path: w.path, fingerprint: contentFingerprint(editedSQL)}

	got, ok, err := edited.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if ok {
		t.Errorf("Load() = (%d, true): the walk resumed behind a watermark left by different SQL", got)
	}
}

// The ordinary resume must still work, or the sidecar buys nothing.
func TestWatermarkSurvivesAnUnchangedStatement(t *testing.T) {
	w := tempWatermark(t, firstSQL)
	if err := w.Save(context.Background(), 5000); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	same := watermarkFile{path: w.path, fingerprint: contentFingerprint(firstSQL)}

	got, ok, err := same.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !ok || got != 5000 {
		t.Errorf("Load() = (%d, %t), want (5000, true)", got, ok)
	}
}

// A watermark written before 0.42.0 carries no fingerprint, so there is nothing to compare
// it against. Unverifiable is not the same as matching: the walk restarts rather than
// resume behind a position it cannot vouch for. key_range requires an idempotent literal
// SET, so restarting redoes work rather than corrupting it.
func TestWatermarkWithoutAFingerprintIsNotTrusted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.yaml.op0.wm")
	if err := os.WriteFile(path, []byte("5000"), 0o600); err != nil {
		t.Fatalf("write legacy watermark: %v", err)
	}

	w := watermarkFile{path: path, fingerprint: contentFingerprint(firstSQL)}

	got, ok, err := w.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if ok {
		t.Errorf("Load() = (%d, true), want the unverifiable watermark ignored", got)
	}
}

// The fingerprint is written beside the value rather than in it, so a file this version
// wrote is still readable as a number by anything that looks.
func TestWatermarkFileKeepsTheValueOnItsOwnLine(t *testing.T) {
	w := tempWatermark(t, firstSQL)
	if err := w.Save(context.Background(), 4242); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	b, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if lines[0] != "4242" {
		t.Errorf("first line = %q, want the watermark value", lines[0])
	}
	if len(lines) != 2 || lines[1] == "" {
		t.Errorf("file = %q, want the value then the fingerprint", string(b))
	}
}
