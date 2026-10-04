package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateSidecar returned at once when the sidecar could not be read, so a corrupt or
// unreadable state file turned a precise resume into a restart or a replay with nothing
// said. A missing sidecar stays silent (nothing to update); an unreadable one is narrated,
// on e.out and on the notice sink, since the console discards e.out.
func TestUpdateSidecarSaysWhenTheSidecarIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	var notices []string
	e := &Engine{dirs: Dirs{Processing: dir}, out: &out, noticeSink: func(s string) { notices = append(notices, s) }}

	e.updateSidecar("missing.yaml", func(*State) {})
	if out.Len() != 0 || len(notices) != 0 {
		t.Fatalf("a missing sidecar must stay silent, got %q %v", out.String(), notices)
	}

	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"+stateSuffix), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.updateSidecar("bad.yaml", func(*State) {})
	if !strings.Contains(out.String(), "bad.yaml") || !strings.Contains(out.String(), "unreadable") {
		t.Errorf("e.out = %q, want the unreadable sidecar named", out.String())
	}
	if len(notices) != 1 {
		t.Errorf("notices = %v, want one", notices)
	}
}
