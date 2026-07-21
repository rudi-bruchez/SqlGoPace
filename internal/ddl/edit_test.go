package ddl_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestIgnoredSessionFor(t *testing.T) {
	t.Run("session_id", func(t *testing.T) {
		s, ok := ddl.IgnoredSessionFor("session_id", "", 53)
		if !ok || s.SessionID == nil || *s.SessionID != 53 {
			t.Fatalf("IgnoredSessionFor(session_id) = (%+v, %v), want SessionID=53", s, ok)
		}
	})
	t.Run("app_name becomes anchored literal regexp", func(t *testing.T) {
		s, ok := ddl.IgnoredSessionFor("app_name", "SQL Agent (job)", 0)
		if !ok || s.AppName != `^SQL Agent \(job\)$` {
			t.Fatalf("IgnoredSessionFor(app_name) = (%+v, %v), want anchored-escaped AppName", s, ok)
		}
	})
	t.Run("empty value on a string criterion is rejected", func(t *testing.T) {
		if _, ok := ddl.IgnoredSessionFor("login_name", "", 0); ok {
			t.Error("IgnoredSessionFor with empty value should return ok=false")
		}
	})
	t.Run("unknown criterion is rejected", func(t *testing.T) {
		if _, ok := ddl.IgnoredSessionFor("nope", "x", 1); ok {
			t.Error("IgnoredSessionFor with unknown criterion should return ok=false")
		}
	})
	t.Run("non-positive session_id is rejected", func(t *testing.T) {
		if _, ok := ddl.IgnoredSessionFor("session_id", "", 0); ok {
			t.Error("IgnoredSessionFor(session_id, spid=0) should return ok=false")
		}
	})
}

func TestAppendIgnoredSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	body := "description: edit me\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rule, _ := ddl.IgnoredSessionFor("app_name", "Reporting", 0)
	if err := ddl.AppendIgnoredSession(path, rule); err != nil {
		t.Fatalf("AppendIgnoredSession() error = %v", err)
	}

	m, err := ddl.LoadManifestFile(path)
	if err != nil {
		t.Fatalf("reload after append: %v", err)
	}
	if len(m.IgnoreBlockedSessions) != 1 || m.IgnoreBlockedSessions[0].AppName != "^Reporting$" {
		t.Fatalf("after append, rules = %+v, want one ^Reporting$ app_name rule", m.IgnoreBlockedSessions)
	}
	// The rest of the manifest must survive the rewrite.
	if m.Description != "edit me" || len(m.Operations) != 1 {
		t.Errorf("rewrite lost manifest content: desc=%q ops=%d", m.Description, len(m.Operations))
	}

	// Re-appending the same rule is a no-op (deduped).
	if err := ddl.AppendIgnoredSession(path, rule); err != nil {
		t.Fatalf("second AppendIgnoredSession() error = %v", err)
	}
	m, _ = ddl.LoadManifestFile(path)
	if len(m.IgnoreBlockedSessions) != 1 {
		t.Errorf("duplicate rule not deduped: %d rules", len(m.IgnoreBlockedSessions))
	}
}

func TestKilledSessionFor(t *testing.T) {
	t.Run("login becomes anchored literal regexp with after=0", func(t *testing.T) {
		s, ok := ddl.KilledSessionFor("login_name", "SVC_RPT", 0)
		if !ok || s.LoginName != `^SVC_RPT$` || s.AfterSeconds != 0 {
			t.Fatalf("KilledSessionFor(login_name) = (%+v, %v), want anchored login, after=0", s, ok)
		}
	})
	t.Run("unknown criterion is rejected", func(t *testing.T) {
		if _, ok := ddl.KilledSessionFor("nope", "x", 1); ok {
			t.Error("KilledSessionFor with unknown criterion should return ok=false")
		}
	})
}

func TestAppendKilledSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	body := "description: edit me\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rule, _ := ddl.KilledSessionFor("login_name", "SVC_RPT", 0)
	if err := ddl.AppendKilledSession(path, rule); err != nil {
		t.Fatalf("AppendKilledSession() error = %v", err)
	}
	m, err := ddl.LoadManifestFile(path)
	if err != nil {
		t.Fatalf("reload after append: %v", err)
	}
	if len(m.KillBlockedSessions) != 1 || m.KillBlockedSessions[0].LoginName != "^SVC_RPT$" {
		t.Fatalf("after append, kill rules = %+v, want one ^SVC_RPT$ login rule", m.KillBlockedSessions)
	}
	if m.Description != "edit me" || len(m.Operations) != 1 {
		t.Errorf("rewrite lost manifest content: desc=%q ops=%d", m.Description, len(m.Operations))
	}
	// Re-appending the same rule is a no-op (deduped).
	if err := ddl.AppendKilledSession(path, rule); err != nil {
		t.Fatalf("second AppendKilledSession() error = %v", err)
	}
	m, _ = ddl.LoadManifestFile(path)
	if len(m.KillBlockedSessions) != 1 {
		t.Errorf("duplicate kill rule not deduped: %d rules", len(m.KillBlockedSessions))
	}
}

// TestAppendIgnoredSessionPreservesShrink is the regression for the TUI ignore-write bug:
// the rewrite serialized a shrink as "operation: shrink_data" (its CommandType), which no
// longer parses. The manifest must round-trip through the rewrite intact.
func TestAppendIgnoredSessionPreservesShrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shrink.yaml")
	body := "description: shrink me\ndatabase: DB\noperations:\n" +
		"  - operation: shrink\n    type: data\n    files: all\n    targetfreespace: 10%\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rule, _ := ddl.IgnoredSessionFor("login_name", "svc", 0)
	if err := ddl.AppendIgnoredSession(path, rule); err != nil {
		t.Fatalf("AppendIgnoredSession() error = %v", err)
	}

	m, err := ddl.LoadManifestFile(path) // an "operation: shrink_data" would fail here
	if err != nil {
		t.Fatalf("reload after append: %v", err)
	}
	if len(m.Operations) != 1 || m.Operations[0].CommandType() != "shrink_data" {
		t.Fatalf("shrink operation lost or altered after rewrite: %+v", m.Operations)
	}
	sh, ok := m.Operations[0].(ddl.Shrink)
	if !ok || sh.Type != "data" || sh.FilesOrAll() != "all" || sh.TargetFreeSpace != "10%" {
		t.Errorf("shrink fields not preserved: %+v", m.Operations[0])
	}
}
