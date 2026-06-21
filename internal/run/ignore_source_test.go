package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

const opTail = "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"

// bumpMtime forces path's modification time forward so manifestSource detects the
// change deterministically, regardless of filesystem timestamp resolution.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func compiledFrom(t *testing.T, path string) IgnoredSessions {
	t.Helper()
	m, err := ddl.LoadManifestFile(path)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	rules, err := CompileIgnoredSessions(m.IgnoreBlockedSessions)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return rules
}

func TestManifestSourceReloadPicksUpNewRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte("ignore_blocked_sessions:\n  - app_name: \"^Reporting\"\n"+opTail), 0o644); err != nil {
		t.Fatal(err)
	}
	src := newManifestSource(path, compiledFrom(t, path))

	reporting := mssql.Session{SPID: 1, Program: "ReportingService"}
	if !src.Current().ignores(reporting) {
		t.Fatal("initial rules should ignore ReportingService")
	}

	// Add a second rule mid-run; live reload must pick it up.
	if err := os.WriteFile(path, []byte("ignore_blocked_sessions:\n  - app_name: \"^Reporting\"\n  - host_name: \"^BATCH01$\"\n"+opTail), 0o644); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, path)
	if !src.Current().ignores(mssql.Session{SPID: 2, Host: "BATCH01"}) {
		t.Error("reload should honor the newly added host_name rule")
	}
}

func TestManifestSourceKeepsLastGoodOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte("ignore_blocked_sessions:\n  - app_name: \"^Reporting\"\n"+opTail), 0o644); err != nil {
		t.Fatal(err)
	}
	src := newManifestSource(path, compiledFrom(t, path))
	reporting := mssql.Session{SPID: 1, Program: "ReportingService"}

	// A mid-edit, invalid file must not drop the working matcher.
	if err := os.WriteFile(path, []byte("not: [valid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, path)
	if !src.Current().ignores(reporting) {
		t.Error("an unparseable manifest must keep the last good matcher")
	}
}

func TestStaticIgnoreAndNilSource(t *testing.T) {
	rules, err := CompileIgnoredSessions([]ddl.IgnoredSession{{AppName: "x"}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := (staticIgnore{rules: rules}).Current(); len(got) != 1 {
		t.Errorf("staticIgnore.Current() len = %d, want 1", len(got))
	}
	if got := currentRules(nil); got != nil {
		t.Errorf("currentRules(nil) = %v, want nil", got)
	}
}

// TestLiveReloadFlipsBlocking is the end-to-end of the reload mechanism: a rule added
// to the manifest mid-run flips ServerSampler.Blocking from true to false, so the
// operation stops yielding to the now-ignored session — without a restart.
func TestLiveReloadFlipsBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte(opTail), 0o644); err != nil {
		t.Fatal(err)
	}
	src := newManifestSource(path, compiledFrom(t, path))

	probe := fakeProbe{sessions: []mssql.Session{{SPID: 60, BlockingSPID: 57, Program: "ReportingService"}}}
	sampler := NewServerSampler(probe, 57, 1000, 80)

	if b, err := sampler.Blocking(context.Background(), src.Current()); err != nil || !b {
		t.Fatalf("Blocking() = (%v, %v), want (true, nil) before the rule is added", b, err)
	}

	if err := os.WriteFile(path, []byte("ignore_blocked_sessions:\n  - app_name: \"^Reporting\"\n"+opTail), 0o644); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, path)

	if b, err := sampler.Blocking(context.Background(), src.Current()); err != nil || b {
		t.Errorf("Blocking() = (%v, %v), want (false, nil) — live reload suppressed it", b, err)
	}
}
