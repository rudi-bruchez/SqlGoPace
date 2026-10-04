//go:build integration

package mssql_test

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// A key_range UPDATE commits before its watermark is saved, so a crash replays the
// boundary batch. Against a real server, the replayed statement must present no row to an
// AFTER UPDATE trigger a second time: before 0.48.0 a 5-row batch replayed once showed the
// trigger 10 rows.
func TestIntegrationKeyRangeReplayTouchesNoRowTwice(t *testing.T) {
	conn, ctx := openTestConn(t)
	exec := func(sql string) {
		t.Helper()
		if err := conn.ExecDDL(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`IF OBJECT_ID('dbo.sqlgopace_kr') IS NOT NULL DROP TABLE dbo.sqlgopace_kr;
		IF OBJECT_ID('dbo.sqlgopace_kr_audit') IS NOT NULL DROP TABLE dbo.sqlgopace_kr_audit;
		CREATE TABLE dbo.sqlgopace_kr (Id int NOT NULL PRIMARY KEY CLUSTERED, Archived int NOT NULL DEFAULT 0);
		CREATE TABLE dbo.sqlgopace_kr_audit (rows_seen int NOT NULL);`)
	t.Cleanup(func() {
		_ = conn.ExecDDL(ctx, "DROP TABLE IF EXISTS dbo.sqlgopace_kr; DROP TABLE IF EXISTS dbo.sqlgopace_kr_audit;")
	})
	exec(`CREATE TRIGGER dbo.sqlgopace_kr_au ON dbo.sqlgopace_kr AFTER UPDATE AS
		INSERT dbo.sqlgopace_kr_audit SELECT COUNT(*) FROM inserted;`)
	exec(`INSERT dbo.sqlgopace_kr (Id) SELECT TOP (10) ROW_NUMBER() OVER (ORDER BY (SELECT 1)) FROM sys.objects;`)

	m, err := ddl.ParseManifest(strings.NewReader(`operations:
  - operation: batch_update
    schema: dbo
    table: sqlgopace_kr
    set: { Archived: 1 }
    confirm_full_table: true
    batch: { strategy: key_range, key: Id }
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op := m.Operations[0].(ddl.BatchDML)
	stmt := ddl.BatchKeyRangeUpdateSQL(op, "Id", 0, 5, false, ddl.ResolvedOptions{})
	exec(stmt) // the batch commits; the watermark save is lost to a crash
	exec(stmt) // the resume replays the same range

	seen, _, err := conn.QueryInt(ctx, "SELECT ISNULL(SUM(rows_seen), 0) FROM dbo.sqlgopace_kr_audit;")
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if seen != 5 {
		t.Errorf("trigger saw %d rows across the batch and its replay, want 5 (each row once)", seen)
	}
}
