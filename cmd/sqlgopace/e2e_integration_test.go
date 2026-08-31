//go:build integration

// End-to-end test: drives the real CLI run path against a live SQL Server.
// Requires SQLGOPACE_TEST_DSN (see docker-compose.yml and the Makefile e2e targets).
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func e2eDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SQLGOPACE_TEST_DSN")
	if dsn == "" {
		t.Skip("SQLGOPACE_TEST_DSN not set; skipping e2e test")
	}
	return dsn
}

// seedTable creates a small table and a nonclustered index to rebuild, and
// registers cleanup that drops the table afterwards.
func seedTable(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()

	conn, err := mssql.Open(ctx, dsn, "test")
	if err != nil {
		t.Fatalf("open setup connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	stmts := []string{
		"IF OBJECT_ID('dbo.sqlgopace_e2e','U') IS NOT NULL DROP TABLE dbo.sqlgopace_e2e;",
		"CREATE TABLE dbo.sqlgopace_e2e (id INT IDENTITY PRIMARY KEY, payload NVARCHAR(100));",
		"INSERT INTO dbo.sqlgopace_e2e (payload) SELECT TOP (1000) 'x' FROM sys.all_objects a CROSS JOIN sys.all_objects b;",
		"CREATE INDEX IX_sqlgopace_e2e ON dbo.sqlgopace_e2e (payload);",
	}
	for _, s := range stmts {
		if err := conn.ExecDDL(ctx, s); err != nil {
			t.Fatalf("seed statement %q: %v", s, err)
		}
	}

	t.Cleanup(func() {
		c, err := mssql.Open(context.Background(), dsn, "test")
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.ExecDDL(context.Background(), "DROP TABLE IF EXISTS dbo.sqlgopace_e2e;")
	})
}

const e2eManifest = `
description: e2e rebuild
operations:
  - operation: rebuild_index
    schema: dbo
    table: sqlgopace_e2e
    index: IX_sqlgopace_e2e
`

func writeE2EConfig(t *testing.T, root, dsn string) string {
	t.Helper()
	matrixAbs, err := filepath.Abs(filepath.FromSlash("../../ddl_compatibility.yaml"))
	if err != nil {
		t.Fatalf("resolve matrix path: %v", err)
	}

	cfg := fmt.Sprintf(`
database:
  connection_string: '%s'
  login_timeout_seconds: 15
directories:
  to_run: '%s'
  processing: '%s'
  done: '%s'
  failed: '%s'
monitoring:
  blocking_poll_seconds: 2
  log_poll_seconds: 2
  progress_poll_seconds: 2
  log_max_size_bytes: 53687091200
  log_max_percent: 95
  blocking_timeout_minutes: 5
  log_drain_timeout_minutes: 30
  max_retry_attempts: 1
  kill_grace_seconds: 30
  checkpoint_between_operations: false
preflight:
  require_data_free_space: false
  check_tempdb: false
  ag_send_queue_warn: true
options_override:
  online: { force: null }
  resumable: { force: null }
  wait_at_low_priority: { force: null }
  maxdop: { force: null }
  sort_in_tempdb: { force: null }
  allow_abort_blockers: false
  wait_max_duration_minutes: 1
notifications:
  webhook_url: ""
  on_events: [fail]
history:
  enabled: false
  destination: ""
matrix_file: '%s'
`,
		dsn,
		filepath.ToSlash(filepath.Join(root, "01.to_run")),
		filepath.ToSlash(filepath.Join(root, "02.processing")),
		filepath.ToSlash(filepath.Join(root, "03.done")),
		filepath.ToSlash(filepath.Join(root, "04.failed")),
		filepath.ToSlash(matrixAbs),
	)

	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// seedFreeSpace grows the connected database's data file with a wide table, then
// drops it, leaving reclaimable free space for a shrink to act on. It is
// best-effort: if the file does not grow past its minimum the shrink is a valid
// no-op, which the test accepts.
func seedFreeSpace(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()

	conn, err := mssql.Open(ctx, dsn, "test")
	if err != nil {
		t.Fatalf("open setup connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	stmts := []string{
		"IF OBJECT_ID('dbo.sqlgopace_shrink_e2e','U') IS NOT NULL DROP TABLE dbo.sqlgopace_shrink_e2e;",
		"CREATE TABLE dbo.sqlgopace_shrink_e2e (id INT IDENTITY PRIMARY KEY, payload CHAR(2000) NOT NULL);",
		"INSERT INTO dbo.sqlgopace_shrink_e2e (payload) SELECT TOP (40000) 'x' FROM sys.all_objects a CROSS JOIN sys.all_objects b;",
		"DROP TABLE dbo.sqlgopace_shrink_e2e;", // frees the pages so the file can shrink
		"CHECKPOINT;",
	}
	for _, s := range stmts {
		if err := conn.ExecDDL(ctx, s); err != nil {
			t.Fatalf("seed statement %q: %v", s, err)
		}
	}
}

const e2eShrinkManifest = `
description: e2e shrink data
operations:
  - operation: shrink
    type: data
    files: all
    targetfreespace: 10%
`

func TestE2EShrinkData(t *testing.T) {
	dsn := e2eDSN(t)
	seedFreeSpace(t, dsn)

	root := t.TempDir()
	toRun := filepath.Join(root, "01.to_run")
	if err := os.MkdirAll(toRun, 0o755); err != nil {
		t.Fatalf("mkdir to_run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(toRun, "010_shrink.yaml"), []byte(e2eShrinkManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	cfgPath := writeE2EConfig(t, root, dsn)

	var out bytes.Buffer
	if err := cli(&out, &out, []string{"--config", cfgPath}); err != nil {
		t.Fatalf("cli(run) error = %v\n--- output ---\n%s", err, out.String())
	}

	donePath := filepath.Join(root, "03.done", "010_shrink.yaml")
	if _, err := os.Stat(donePath); err != nil {
		t.Errorf("manifest not in done: %v\n--- output ---\n%s", err, out.String())
	}
	logBytes, err := os.ReadFile(donePath + ".log")
	if err != nil {
		t.Fatalf("run log not written: %v", err)
	}
	// The run log must carry a per-file shrink summary (a reduction or a no-op are
	// both valid successful outcomes depending on the seeded free space).
	if !bytes.Contains(logBytes, []byte("shrink ")) {
		t.Errorf("run log missing a shrink summary\n--- log ---\n%s", logBytes)
	}
}

func TestE2ERebuildIndex(t *testing.T) {
	dsn := e2eDSN(t)
	seedTable(t, dsn)

	root := t.TempDir()
	toRun := filepath.Join(root, "01.to_run")
	if err := os.MkdirAll(toRun, 0o755); err != nil {
		t.Fatalf("mkdir to_run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(toRun, "010_rebuild.yaml"), []byte(e2eManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	cfgPath := writeE2EConfig(t, root, dsn)

	var out bytes.Buffer
	if err := cli(&out, &out, []string{"--config", cfgPath}); err != nil {
		t.Fatalf("cli(run) error = %v\n--- output ---\n%s", err, out.String())
	}

	donePath := filepath.Join(root, "03.done", "010_rebuild.yaml")
	if _, err := os.Stat(donePath); err != nil {
		t.Errorf("manifest not in done: %v\n--- output ---\n%s", err, out.String())
	}
	if _, err := os.Stat(donePath + ".log"); err != nil {
		t.Errorf("run log not written: %v", err)
	}
}

// TestE2EDryRunExpandsAllInTheManifestDatabase pins a defect an external reviewer
// found and that only shows when the tool is run: expanding "index: ALL" reads
// sys.indexes, which sees only the connection's own database. A real run opens one
// engine per database the queue targets (spec §17.6), so a dry run over the single
// DSN connection rendered another database's index list — the operator reviewed one
// plan and the run executed a different one.
//
// The test connects to the DSN's database (tempdb under the Makefile default) and
// dry-runs a manifest that names master, where the seeded index exists under a name
// that exists nowhere else.
func TestE2EDryRunExpandsAllInTheManifestDatabase(t *testing.T) {
	dsn := e2eDSN(t)
	ctx := context.Background()

	conn, err := mssql.Open(ctx, dsn, "test")
	if err != nil {
		t.Fatalf("open setup connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Same table name in both databases, different index names: only an expansion
	// that read the right database can produce the right one.
	const (
		inMaster = "IX_sqlgopace_xdb_master_only"
		inDSNDB  = "IX_sqlgopace_xdb_connected_only"
	)
	for _, s := range []struct{ db, index string }{{"master", inMaster}, {"", inDSNDB}} {
		c := conn
		if s.db != "" {
			c, err = mssql.OpenDatabase(ctx, dsn, s.db, "test")
			if err != nil {
				t.Fatalf("open %s: %v", s.db, err)
			}
			defer func() { _ = c.Close() }()
		}
		if err := c.ExecDDL(ctx, `IF OBJECT_ID('dbo.sqlgopace_xdb') IS NOT NULL DROP TABLE dbo.sqlgopace_xdb;
			CREATE TABLE dbo.sqlgopace_xdb (id int NOT NULL PRIMARY KEY, a int NULL);`); err != nil {
			t.Fatalf("seed table in %q: %v", s.db, err)
		}
		if err := c.ExecDDL(ctx, "CREATE INDEX "+s.index+" ON dbo.sqlgopace_xdb(a);"); err != nil {
			t.Fatalf("seed index in %q: %v", s.db, err)
		}
		db := s.db
		t.Cleanup(func() {
			cc := conn
			if db != "" {
				var cerr error
				if cc, cerr = mssql.OpenDatabase(context.Background(), dsn, db, "test"); cerr != nil {
					return
				}
				defer func() { _ = cc.Close() }()
			}
			_ = cc.ExecDDL(context.Background(), "IF OBJECT_ID('dbo.sqlgopace_xdb') IS NOT NULL DROP TABLE dbo.sqlgopace_xdb;")
		})
	}

	root := t.TempDir()
	manifest := filepath.Join(root, "010_all.yaml")
	if err := os.WriteFile(manifest, []byte(`
description: cross-database ALL expansion
database: master
operations:
  - operation: rebuild_index
    schema: dbo
    table: sqlgopace_xdb
    index: ALL
`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	cfgPath := writeE2EConfig(t, root, dsn)

	var out bytes.Buffer
	if err := cli(&out, &out, []string{"--config", cfgPath, "--dry-run", manifest}); err != nil {
		t.Fatalf("cli(dry-run) error = %v\n--- output ---\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, inMaster) {
		t.Errorf("plan does not name %s, so ALL was not expanded in the manifest's database:\n%s", inMaster, got)
	}
	if strings.Contains(got, inDSNDB) {
		t.Errorf("plan names %s: ALL was expanded against the connection's database, not the manifest's:\n%s", inDSNDB, got)
	}
}
