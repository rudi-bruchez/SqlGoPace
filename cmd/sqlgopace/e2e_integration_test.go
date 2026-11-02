//go:build integration

// End-to-end test: drives the real CLI run path against a live SQL Server.
// Requires SQLGOPACE_TEST_DSN (see docker-compose.yml and the Makefile e2e targets).
package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mssqldb "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"

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

// e2eDB opens a plain database/sql pool on database, reusing the DSN's server and
// credentials. The test needs connections the tool does not own: one to run the backup
// loop, one to set up and drop the database.
func e2eDB(t *testing.T, dsn, database string) *sql.DB {
	t.Helper()
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.Database = database
	return sql.OpenDB(mssqldb.NewConnectorConfig(cfg))
}

const (
	backupDB   = "sqlgopace_e2e_backup"
	backupFile = "/var/opt/mssql/data/sqlgopace_e2e_backup.bak"
)

// dropBackupDB removes the test database, its backup history and its backup file. It is
// both the cleanup and the guard against a database a previous run left behind.
func dropBackupDB(ctx context.Context, master *sql.DB) error {
	for _, s := range []string{
		"IF DB_ID(N'" + backupDB + "') IS NOT NULL ALTER DATABASE [" + backupDB + "] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;",
		"DROP DATABASE IF EXISTS [" + backupDB + "];",
		"EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'" + backupDB + "';",
		"EXEC master.sys.xp_delete_files N'" + backupFile + "';",
	} {
		if _, err := master.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

const e2eBackupShrinkManifest = `
description: e2e shrink data against a running backup
database: sqlgopace_e2e_backup
operations:
  - operation: shrink
    type: data
    files: all
    targetfreespace: 10%
`

// TestE2EShrinkWaitsForBackup runs a data shrink while BACKUP DATABASE loops on the same
// database. A shrink chunk cannot move pages while a data backup holds the file, so
// without awaitBackup the chunks gain nothing and the shrink stalls; with it, the run log
// names the backup it waited for.
//
// The backup goes to a file, not to NUL: measured by the design review, a backup to NUL
// of this database takes 0.6 s, every chunk waits it out and advances, and no backup wait
// is ever seen. The loop is bounded in time (90 s) rather than by the shrink, because
// awaitBackup waits as long as a data backup runs and an unbounded loop would make the
// shrink wait for the test.
func TestE2EShrinkWaitsForBackup(t *testing.T) {
	dsn := e2eDSN(t)
	ctx := context.Background()

	master := e2eDB(t, dsn, "master")
	t.Cleanup(func() { _ = master.Close() })
	if err := dropBackupDB(ctx, master); err != nil {
		t.Fatalf("drop leftover database: %v", err)
	}
	if _, err := master.ExecContext(ctx, "CREATE DATABASE ["+backupDB+"];"); err != nil {
		t.Skipf("CREATE DATABASE refused: %v", err)
	}
	t.Cleanup(func() {
		if err := dropBackupDB(context.Background(), master); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	db := e2eDB(t, dsn, backupDB)
	t.Cleanup(func() { _ = db.Close() })
	for _, s := range []string{
		"ALTER DATABASE [" + backupDB + "] SET RECOVERY SIMPLE;",
		"CREATE TABLE dbo.dropped (id int IDENTITY PRIMARY KEY, payload char(7000) NOT NULL);",
		"CREATE TABLE dbo.kept (id int IDENTITY PRIMARY KEY, payload char(7000) NOT NULL);",
		"INSERT INTO dbo.dropped (payload) SELECT TOP (60000) 'x' FROM sys.all_objects a CROSS JOIN sys.all_objects b;",
		"INSERT INTO dbo.kept (payload) SELECT TOP (60000) 'x' FROM sys.all_objects a CROSS JOIN sys.all_objects b;",
		"DROP TABLE dbo.dropped;",
		"CHECKPOINT;",
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed statement %q: %v", s, err)
		}
	}
	// Dropping a table this size is a deferred drop: its pages are deallocated by a
	// background task a few seconds later, and that task waits for any running BACKUP
	// DATABASE to finish (measured on 2022 CU26). Started now, the backup loop would keep
	// the space allocated and the shrink would find nothing to reclaim.
	freed := false
	for wait := time.Now().Add(60 * time.Second); time.Now().Before(wait); time.Sleep(500 * time.Millisecond) {
		var sizeMB, usedMB int
		if err := db.QueryRowContext(ctx, "SELECT size/128, FILEPROPERTY(name, 'SpaceUsed')/128 FROM sys.database_files WHERE type = 0;").Scan(&sizeMB, &usedMB); err != nil {
			t.Fatalf("read data file space: %v", err)
		}
		if usedMB < sizeMB*2/3 {
			freed = true
			break
		}
	}
	if !freed {
		t.Fatal("dropped table's pages not deallocated within 60 s")
	}

	backupConn := e2eDB(t, dsn, "master")
	done := make(chan struct{})
	finished := make(chan struct{})
	backupErr := make(chan error, 1)
	// Registered after the database cleanup, so it runs first: the loop is stopped
	// before the database is dropped under it.
	t.Cleanup(func() {
		close(done)
		<-finished
		_ = backupConn.Close()
	})
	go func() {
		defer close(finished)
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-done:
				return
			default:
			}
			_, err := backupConn.ExecContext(context.Background(), "BACKUP DATABASE ["+backupDB+"] TO DISK = '"+backupFile+
				"' WITH INIT, COPY_ONLY, NO_COMPRESSION, BUFFERCOUNT = 1, MAXTRANSFERSIZE = 65536;")
			if err != nil {
				backupErr <- err
				return
			}
		}
	}()

	// Start the shrink against a live backup, seen the way the tool sees it.
	seen := false
	for wait := time.Now().Add(30 * time.Second); time.Now().Before(wait); time.Sleep(100 * time.Millisecond) {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sys.dm_exec_requests WHERE command = 'BACKUP DATABASE' AND database_id = DB_ID();").Scan(&n); err != nil {
			t.Fatalf("poll for backup: %v", err)
		}
		if n > 0 {
			seen = true
			break
		}
	}
	if !seen {
		select {
		case err := <-backupErr:
			t.Fatalf("no backup seen within 30 s; backup loop error: %v", err)
		default:
			t.Fatal("no backup seen within 30 s")
		}
	}

	root := t.TempDir()
	toRun := filepath.Join(root, "01.to_run")
	if err := os.MkdirAll(toRun, 0o755); err != nil {
		t.Fatalf("mkdir to_run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(toRun, "010_shrink.yaml"), []byte(e2eBackupShrinkManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	cfgPath := writeE2EConfig(t, root, dsn)

	var out bytes.Buffer
	if err := cli(&out, &out, []string{"--config", cfgPath}); err != nil {
		t.Fatalf("cli(run) error = %v\n--- output ---\n%s", err, out.String())
	}

	select {
	case err := <-backupErr:
		t.Errorf("backup loop failed: %v", err)
	default:
	}
	donePath := filepath.Join(root, "03.done", "010_shrink.yaml")
	if _, err := os.Stat(donePath); err != nil {
		t.Errorf("manifest not in done: %v\n--- output ---\n%s", err, out.String())
	}
	logBytes, err := os.ReadFile(donePath + ".log")
	if err != nil {
		logBytes, err = os.ReadFile(filepath.Join(root, "04.failed", "010_shrink.yaml.log"))
		if err != nil {
			t.Fatalf("run log not written: %v\n--- output ---\n%s", err, out.String())
		}
	}
	t.Logf("run log:\n%s", logBytes)
	// A chunk that ran into the backup must be retried, not counted against the
	// no-progress budget: without the wait, the same run backs off as a stall.
	if bytes.Contains(logBytes, []byte("made no progress")) {
		t.Errorf("run log has a no-progress reaction: the backup was counted as a stall")
	}
	if t.Failed() || bytes.Contains(logBytes, []byte("BACKUP DATABASE")) {
		return
	}
	// A chunk that queues on the backup's lock and gets it in the window between two
	// backups of the loop moves pages, which is correct, so some runs never meet a
	// running backup. The wait logic itself is pinned by the unit tests in internal/run.
	summary := "no shrink summary in the log"
	for _, line := range strings.Split(string(logBytes), "\n") {
		if strings.Contains(line, "shrink "+backupDB+" (data)") {
			summary = strings.TrimSpace(line)
			break
		}
	}
	t.Skipf("the shrink never met a running backup (every chunk advanced between backups), so the run did not exercise the wait: %s", summary)
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
		// Its own connection: cleanups run after the deferred Close of conn above, so
		// reusing conn here executed on a closed connection and panicked.
		t.Cleanup(func() {
			var cc *mssql.Conn
			var cerr error
			if db != "" {
				cc, cerr = mssql.OpenDatabase(context.Background(), dsn, db, "test")
			} else {
				cc, cerr = mssql.Open(context.Background(), dsn, "test")
			}
			if cerr != nil {
				return
			}
			defer func() { _ = cc.Close() }()
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
