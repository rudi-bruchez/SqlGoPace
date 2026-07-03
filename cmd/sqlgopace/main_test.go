package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestQueuedDatabases(t *testing.T) {
	dir := t.TempDir()
	write := func(name, database string) {
		body := "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
		if database != "" {
			body = "database: " + database + "\n" + body
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("010_a.yaml", "")     // no database → the connected database
	write("020_b.yaml", "DB2")  // explicit
	write("030_c.yaml", "db2")  // same database, different case
	write("040_d.yaml", "CONN") // equals the connected database
	_ = os.WriteFile(filepath.Join(dir, ".hidden.yaml"), []byte("ignored"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)

	got, err := queuedDatabases(dir, "CONN")
	if err != nil {
		t.Fatalf("queuedDatabases() error = %v", err)
	}
	if diff := cmp.Diff([]string{"CONN", "DB2"}, got); diff != "" {
		t.Errorf("queuedDatabases mismatch (-want +got):\n%s", diff)
	}

	// A missing directory is not an error (nothing queued yet).
	if dbs, err := queuedDatabases(filepath.Join(dir, "nope"), "CONN"); err != nil || dbs != nil {
		t.Errorf("queuedDatabases(missing) = (%v, %v), want (nil, nil)", dbs, err)
	}
}

const (
	exampleManifest = "../../01.to_run/.010_example_rebuild.yaml"
	matrixFlag      = "--matrix=../../ddl_compatibility.yaml"
)

func TestProgressMsgForwardVsRollback(t *testing.T) {
	fwd := progressMsg(mssql.Progress{PercentComplete: 30, EstimatedCompletionMS: 5000, Command: "ALTER INDEX"})
	if fwd.Percent != 30 || fwd.RollbackPercent != 0 || fwd.ETASeconds != 5 {
		t.Errorf("forward progress = %+v, want Percent=30 RollbackPercent=0 ETASeconds=5", fwd)
	}

	rb := progressMsg(mssql.Progress{PercentComplete: 60, Command: "KILLED/ROLLBACK"})
	if rb.RollbackPercent != 60 || rb.Percent != 0 {
		t.Errorf("rollback progress = %+v, want RollbackPercent=60 Percent=0", rb)
	}
}

func TestRunDryRunEnterprise2022(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--dry-run", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, exampleManifest}

	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(dry-run) error = %v, want nil", err)
	}

	got := out.String()
	wants := []string{
		"ALTER INDEX [IX_DISPATCH] ON [dbo].[DISPATCH] REBUILD WITH (ONLINE = ON",
		"MAXDOP = 4",
		"DATA_COMPRESSION = PAGE",
		"IF COL_LENGTH(N'[dbo].[DISPATCH]', N'PROCESSED') IS NULL",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("dry-run output missing %q\n--- output ---\n%s", w, got)
		}
	}
}

func TestRunDryRunStandardOmitsOnline(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--dry-run", "--assume-version=16", "--assume-edition=standard", matrixFlag, exampleManifest}

	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(dry-run standard) error = %v, want nil", err)
	}
	if strings.Contains(out.String(), "ONLINE = ON") {
		t.Errorf("Standard edition output should not inject ONLINE:\n%s", out.String())
	}
}

func TestRunDryRunWithConfigPolicy(t *testing.T) {
	var out bytes.Buffer
	// Offline target (assume flags) but policy comes from --config, which forces
	// ONLINE off; the matrix path is taken from the config file.
	args := []string{
		"--dry-run", "--assume-version=16", "--assume-edition=enterprise",
		"--config=testdata/config_force_online_off.yaml", exampleManifest,
	}

	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(config policy) error = %v, want nil", err)
	}
	if strings.Contains(out.String(), "ONLINE = ON") {
		t.Errorf("config forced ONLINE off, but output still injects it:\n%s", out.String())
	}
}

func TestRunExplain(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--dry-run", "--explain", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, exampleManifest}

	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(explain) error = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "online = ON") {
		t.Errorf("explain output missing option decision trail:\n%s", out.String())
	}
}

func TestRunExplainIgnoreBlockedSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "010_ig.yaml")
	body := "ignore_blocked_sessions:\n  - app_name: \"^SQLAgent\"\n    login_name: \"svc\"\n  - session_id: 142\n" +
		"operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	args := []string{"--dry-run", "--explain", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, path}
	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(explain) error = %v", err)
	}
	s := out.String()
	for _, want := range []string{"ignore_blocked_sessions", "app_name~^SQLAgent AND login_name~svc", "session_id=142"} {
		if !strings.Contains(s, want) {
			t.Errorf("explain output missing %q:\n%s", want, s)
		}
	}
}

func TestDryRunAnnotatesWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "win.yaml")
	const m = `
description: windowed
window:
  start: "01:00"
  end: "05:00"
  days: [Sat, Sun]
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`
	if err := os.WriteFile(path, []byte(m), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out bytes.Buffer
	args := []string{"--dry-run", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, path}
	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(dry-run) error = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "window 01:00–05:00") {
		t.Errorf("dry-run output missing window annotation:\n%s", out.String())
	}
}

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer
	if err := cli(&out, io.Discard, []string{"--version"}); err != nil {
		t.Fatalf("run(--version) error = %v, want nil", err)
	}
	if !strings.HasPrefix(out.String(), "sqlgopace ") {
		t.Errorf("run(--version) = %q, want it to start with 'sqlgopace '", out.String())
	}
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing version on non-azure", []string{"--dry-run", "--assume-edition=enterprise", matrixFlag, exampleManifest}},
		{"no manifests", []string{"--dry-run", "--assume-version=16", matrixFlag}},
		{"bad edition", []string{"--dry-run", "--assume-version=16", "--assume-edition=bogus", matrixFlag, exampleManifest}},
		{"missing matrix file", []string{"--dry-run", "--assume-version=16", "--matrix=does-not-exist.yaml", exampleManifest}},
		{"auto with dry-run is rejected", []string{"--auto", "--dry-run", matrixFlag}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := cli(io.Discard, io.Discard, tt.args); err == nil {
				t.Errorf("run(%v) error = nil, want non-nil", tt.args)
			}
		})
	}
}
