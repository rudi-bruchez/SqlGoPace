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

// TestFileGrowthsIntegration verifies the growth read against a real catalog: the unit
// conversions are the risky part, because growth's meaning depends on is_percent_growth
// and max_size carries sentinels (-1 = until the disk fills, 0 = no growth) that must
// survive the page-to-MB conversion unchanged.
func TestFileGrowthsIntegration(t *testing.T) {
	conn, ctx := openTestConn(t)

	got, err := conn.FileGrowths(ctx, mssql.FileTypeRows)
	if err != nil {
		t.Fatalf("FileGrowths(ROWS): %v", err)
	}
	if len(got) == 0 {
		t.Fatal("FileGrowths(ROWS) returned no files; every database has at least one")
	}
	for _, f := range got {
		if f.Name == "" || f.TypeDesc != "ROWS" {
			t.Errorf("FileGrowths returned %+v, want a named ROWS file", f)
		}
		if f.SizeMB <= 0 {
			t.Errorf("%s SizeMB = %d, want > 0", f.Name, f.SizeMB)
		}
		if f.MaxSizeMB < -1 {
			t.Errorf("%s MaxSizeMB = %d; only -1 and 0 are sentinels, anything lower is a bad conversion", f.Name, f.MaxSizeMB)
		}
		if f.IsPercent && (f.Growth < 0 || f.Growth > 100) {
			t.Errorf("%s percentage growth = %d, want a whole percentage", f.Name, f.Growth)
		}
		// A capped file must not report negative headroom.
		if mb, ok := f.HeadroomMB(); ok && mb < 0 {
			t.Errorf("%s HeadroomMB = %d, want >= 0", f.Name, mb)
		}
		t.Logf("%s: size=%d MB percent=%t growth=%d next=%d MB max=%d MB",
			f.Name, f.SizeMB, f.IsPercent, f.Growth, f.NextGrowthMB(), f.MaxSizeMB)
	}
}
