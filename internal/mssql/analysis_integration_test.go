//go:build integration

// Maintenance-analysis integration tests (spec §4). They run only with
// `-tags=integration` against a real SQL Server (see integration_test.go for the
// SQLGOPACE_TEST_DSN setup). Each test seeds throwaway objects in the connected
// database and drops them on cleanup.
package mssql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// exec runs a setup/teardown statement on the connection.
func exec(t *testing.T, conn *mssql.Conn, ctx context.Context, stmt string) {
	t.Helper()
	if err := conn.ExecDDL(ctx, stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

// findObject returns the inventory row for the given table and index_id.
func findObject(t *testing.T, conn *mssql.Conn, ctx context.Context, table string, indexID int) mssql.InventoryObject {
	t.Helper()
	inv, err := conn.ObjectInventory(ctx)
	if err != nil {
		t.Fatalf("ObjectInventory() error = %v", err)
	}
	for _, o := range inv {
		if o.Table == table && o.IndexID == indexID {
			return o
		}
	}
	t.Fatalf("object %s (index_id %d) not found in inventory", table, indexID)
	return mssql.InventoryObject{}
}

func TestIntegrationObjectInventoryAndCompression(t *testing.T) {
	conn, ctx := openTestConn(t)
	const table = "sgp_m3_idx"

	exec(t, conn, ctx, "IF OBJECT_ID('dbo."+table+"') IS NOT NULL DROP TABLE dbo."+table)
	exec(t, conn, ctx, "CREATE TABLE dbo."+table+" (id int IDENTITY PRIMARY KEY, pad varchar(400) NOT NULL)")
	t.Cleanup(func() {
		_ = conn.ExecDDL(context.Background(), "IF OBJECT_ID('dbo."+table+"') IS NOT NULL DROP TABLE dbo."+table)
	})
	exec(t, conn, ctx, "INSERT INTO dbo."+table+" (pad) SELECT TOP (5000) REPLICATE('x', 350) FROM sys.all_objects a CROSS JOIN sys.all_objects b")

	obj := findObject(t, conn, ctx, table, 1) // clustered index
	if obj.Compression != "NONE" {
		t.Errorf("Compression = %q, want NONE", obj.Compression)
	}
	if obj.SizeMB <= 0 {
		t.Errorf("SizeMB = %v, want > 0", obj.SizeMB)
	}
	if obj.IsHeap() {
		t.Errorf("IsHeap() = true, want false for a clustered table")
	}

	// Fragmentation via the cheap LIMITED scan.
	ps, err := conn.PhysicalStats(ctx, obj.ObjectID, obj.IndexID, nil, mssql.PhysicalLimited)
	if err != nil {
		t.Fatalf("PhysicalStats(LIMITED) error = %v", err)
	}
	if len(ps) == 0 {
		t.Fatalf("PhysicalStats returned no rows")
	}

	// Compression estimate (the expensive read) for ROW and PAGE.
	for _, setting := range []string{"ROW", "PAGE"} {
		est, err := conn.EstimateCompression(ctx, "dbo", table, obj.IndexID, nil, setting)
		if err != nil {
			t.Fatalf("EstimateCompression(%s) error = %v", setting, err)
		}
		if len(est) == 0 || est[0].CurrentKB <= 0 {
			t.Errorf("EstimateCompression(%s) = %+v, want a positive current size", setting, est)
		}
	}

	// Operational stats and statistics properties should be readable.
	if _, err := conn.IndexOperationalStats(ctx, obj.ObjectID, obj.IndexID, nil); err != nil {
		t.Fatalf("IndexOperationalStats() error = %v", err)
	}
	props, err := conn.StatsProperties(ctx, obj.ObjectID)
	if err != nil {
		t.Fatalf("StatsProperties() error = %v", err)
	}
	if len(props) == 0 {
		t.Errorf("StatsProperties returned none; expected at least the PK statistic")
	}
}

func TestIntegrationHeapForwardedRecords(t *testing.T) {
	conn, ctx := openTestConn(t)
	const table = "sgp_m3_heap"

	exec(t, conn, ctx, "IF OBJECT_ID('dbo."+table+"') IS NOT NULL DROP TABLE dbo."+table)
	exec(t, conn, ctx, "CREATE TABLE dbo."+table+" (id int IDENTITY, v varchar(8000) NOT NULL)") // no PK → heap
	t.Cleanup(func() {
		_ = conn.ExecDDL(context.Background(), "IF OBJECT_ID('dbo."+table+"') IS NOT NULL DROP TABLE dbo."+table)
	})
	exec(t, conn, ctx, "INSERT INTO dbo."+table+" (v) SELECT TOP (5000) REPLICATE('a', 100) FROM sys.all_objects a CROSS JOIN sys.all_objects b")
	// Grow every row well past its original size to force forwarding pointers.
	exec(t, conn, ctx, "UPDATE dbo."+table+" SET v = REPLICATE('b', 4000)")

	obj := findObject(t, conn, ctx, table, 0) // heap: index_id = 0
	if !obj.IsHeap() {
		t.Fatalf("IsHeap() = false, want true (index_id %d, type %d)", obj.IndexID, obj.Type)
	}

	// A LIMITED scan does NOT populate forwarded_record_count — verify it is zero,
	// so the SAMPLED result below proves the mode matters (spec §15.4).
	limited, err := conn.PhysicalStats(ctx, obj.ObjectID, 0, nil, mssql.PhysicalLimited)
	if err != nil {
		t.Fatalf("PhysicalStats(LIMITED) error = %v", err)
	}
	if len(limited) > 0 && limited[0].ForwardedRecordCount != 0 {
		t.Errorf("LIMITED ForwardedRecordCount = %d, want 0 (only SAMPLED populates it)", limited[0].ForwardedRecordCount)
	}

	sampled, err := conn.PhysicalStats(ctx, obj.ObjectID, 0, nil, mssql.PhysicalSampled)
	if err != nil {
		t.Fatalf("PhysicalStats(SAMPLED) error = %v", err)
	}
	if len(sampled) == 0 {
		t.Fatalf("PhysicalStats(SAMPLED) returned no rows")
	}
	if sampled[0].ForwardedRecordCount <= 0 {
		t.Errorf("SAMPLED ForwardedRecordCount = %d, want > 0 after growing rows", sampled[0].ForwardedRecordCount)
	}
	if sampled[0].RecordCount <= 0 {
		t.Errorf("SAMPLED RecordCount = %d, want > 0", sampled[0].RecordCount)
	}
}

// TestIntegrationInventoryExcludesSystemObjects guards the is_ms_shipped filter.
func TestIntegrationInventoryExcludesSystemObjects(t *testing.T) {
	conn, ctx := openTestConn(t)
	inv, err := conn.ObjectInventory(ctx)
	if err != nil {
		t.Fatalf("ObjectInventory() error = %v", err)
	}
	for _, o := range inv {
		if strings.HasPrefix(o.Table, "sys") || strings.HasPrefix(o.Schema, "sys") {
			t.Errorf("inventory included a system object: %s.%s", o.Schema, o.Table)
		}
	}
}
