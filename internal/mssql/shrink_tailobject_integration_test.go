//go:build integration

// Tail-object integration test (design: tail-object identification). It runs only
// with `-tags=integration` against a real SQL Server (see integration_test.go for
// the SQLGOPACE_TEST_DSN setup). Unlike shrink_integration_test.go, this test
// mutates the database: FindTailObject needs data on disk to guarantee a *found*
// tail object, so it creates a small filler table via the connection's exported
// ExecDDL, inserts enough rows to occupy pages, and drops the table on cleanup.
package mssql_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// TestFindTailObjectIntegration creates a table with enough rows to own pages near
// the primary data file's tail, and asserts FindTailObject names it. Requires
// SQLGOPACE_TEST_DSN (2019+, for sys.dm_db_page_info).
func TestFindTailObjectIntegration(t *testing.T) {
	conn, ctx := openTestConn(t)

	// A dedicated filler table with enough rows to occupy pages near the file tail.
	if err := conn.ExecDDL(ctx, `IF OBJECT_ID('dbo.sqlgopace_tail_test') IS NOT NULL DROP TABLE dbo.sqlgopace_tail_test;`); err != nil {
		t.Fatalf("drop pre-existing test table: %v", err)
	}
	if err := conn.ExecDDL(ctx, `CREATE TABLE dbo.sqlgopace_tail_test (id int identity primary key, filler char(4000) not null);`); err != nil {
		t.Fatalf("create test table: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.ExecDDL(ctx, `DROP TABLE dbo.sqlgopace_tail_test;`); err != nil {
			t.Errorf("drop test table: %v", err)
		}
	})
	if _, err := conn.ExecRows(ctx, `INSERT INTO dbo.sqlgopace_tail_test (filler) SELECT TOP (2000) 'x' FROM sys.all_objects a CROSS JOIN sys.all_objects b;`); err != nil {
		t.Fatalf("insert filler rows: %v", err)
	}

	files, err := conn.FileSpace(ctx, mssql.FileTypeRows)
	if err != nil || len(files) == 0 {
		t.Fatalf("FileSpace: %v (files=%d)", err, len(files))
	}
	f := files[0]

	got, found, err := conn.FindTailObject(ctx, f.FileID, 262144)
	if err != nil {
		t.Fatalf("FindTailObject: %v", err)
	}
	if !found {
		t.Fatal("expected a tail object, got found=false")
	}
	if got.ObjectID == 0 {
		t.Errorf("tail object has zero object_id: %+v", got)
	}
	t.Logf("tail object: %s.%s index_id=%d page_from_end=%d", got.Schema, got.Table, got.IndexID, got.PageFromEnd)
}
