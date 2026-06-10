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

	conn, err := mssql.Open(ctx, dsn)
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
		c, err := mssql.Open(context.Background(), dsn)
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
