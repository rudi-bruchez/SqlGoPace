//go:build integration

// Shrink read integration tests (design §5, §13). They run only with
// `-tags=integration` against a real SQL Server (see integration_test.go for the
// SQLGOPACE_TEST_DSN setup). They only read DMVs/catalog views, so they neither
// create objects nor mutate the database.
package mssql_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestIntegrationFileSpaceAndSize(t *testing.T) {
	conn, ctx := openTestConn(t)

	data, err := conn.FileSpace(ctx, mssql.FileTypeRows)
	if err != nil {
		t.Fatalf("FileSpace(ROWS) error = %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("FileSpace(ROWS) returned no data files, want at least one")
	}
	for _, f := range data {
		if f.SizeMB <= 0 {
			t.Errorf("file %q SizeMB = %d, want > 0", f.Name, f.SizeMB)
		}
		if f.UsedMB < 0 || f.UsedMB > f.SizeMB {
			t.Errorf("file %q UsedMB = %d, want in [0, %d]", f.Name, f.UsedMB, f.SizeMB)
		}
		if f.FreeMB != f.SizeMB-f.UsedMB {
			t.Errorf("file %q FreeMB = %d, want %d", f.Name, f.FreeMB, f.SizeMB-f.UsedMB)
		}
	}

	// FileSizeMB on the first data file must agree with FileSpace.SizeMB.
	first := data[0]
	got, err := conn.FileSizeMB(ctx, first.Name)
	if err != nil {
		t.Fatalf("FileSizeMB(%q) error = %v", first.Name, err)
	}
	if got != first.SizeMB {
		t.Errorf("FileSizeMB(%q) = %d, want %d (matching FileSpace)", first.Name, got, first.SizeMB)
	}

	logs, err := conn.FileSpace(ctx, mssql.FileTypeLog)
	if err != nil {
		t.Fatalf("FileSpace(LOG) error = %v", err)
	}
	if len(logs) == 0 {
		t.Errorf("FileSpace(LOG) returned no log files, want at least one")
	}
}

func TestIntegrationLogReuseAndFloor(t *testing.T) {
	conn, ctx := openTestConn(t)

	model, reuse, err := conn.LogReuse(ctx)
	if err != nil {
		t.Fatalf("LogReuse() error = %v", err)
	}
	if model == "" || reuse == "" {
		t.Errorf("LogReuse() = (%q, %q), want both non-empty", model, reuse)
	}
	t.Logf("recovery_model=%s log_reuse_wait=%s", model, reuse)

	floor, err := conn.ActiveLogFloorMB(ctx)
	if err != nil {
		t.Fatalf("ActiveLogFloorMB() error = %v", err)
	}
	if floor < 0 {
		t.Errorf("ActiveLogFloorMB() = %d, want >= 0", floor)
	}
}
