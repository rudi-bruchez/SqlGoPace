//go:build integration

package mssql_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// TestTableStructureSizesIntegration covers what the preflight and the engine need: the
// heap and each index in one read, a disabled index listed with no pages (it is rebuilt
// from the table, so it must not vanish from the count), one partition, and a missing
// object reading as "no rows" rather than an error.
func TestTableStructureSizesIntegration(t *testing.T) {
	conn, ctx := openTestConn(t)
	table := "sqlgopace_sizes_probe"
	exec(t, conn, ctx, "DROP TABLE IF EXISTS dbo."+table)
	exec(t, conn, ctx, "CREATE TABLE dbo."+table+" (id INT NOT NULL, v CHAR(200) NOT NULL)")
	exec(t, conn, ctx, "INSERT INTO dbo."+table+" (id, v) SELECT TOP (5000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)), 'x' FROM sys.all_objects a CROSS JOIN sys.all_objects b")
	exec(t, conn, ctx, "CREATE INDEX IX_"+table+"_live ON dbo."+table+" (id)")
	exec(t, conn, ctx, "CREATE INDEX IX_"+table+"_off ON dbo."+table+" (v)")
	exec(t, conn, ctx, "ALTER INDEX IX_"+table+"_off ON dbo."+table+" DISABLE")
	t.Cleanup(func() { exec(t, conn, ctx, "DROP TABLE IF EXISTS dbo."+table) })

	got, err := conn.TableStructureSizes(ctx, "dbo", table, nil)
	if err != nil {
		t.Fatalf("TableStructureSizes: %v", err)
	}
	byName := map[string]mssql.StructureSize{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if len(got) != 3 {
		t.Fatalf("got %d structures, want 3 (heap + 2 indexes): %+v", len(got), got)
	}
	if heap := byName[""]; heap.IndexID != 0 || heap.UsedKB <= 0 {
		t.Errorf("heap row = %+v, want index_id 0 with pages", heap)
	}
	if live := byName["IX_"+table+"_live"]; live.Disabled || live.UsedKB <= 0 {
		t.Errorf("live index row = %+v, want enabled with pages", live)
	}
	off := byName["IX_"+table+"_off"]
	if !off.Disabled || off.UsedKB != 0 {
		t.Errorf("disabled index row = %+v, want Disabled with 0 KB", off)
	}

	one := 1
	part, err := conn.TableStructureSizes(ctx, "dbo", table, &one)
	if err != nil {
		t.Fatalf("TableStructureSizes(partition 1): %v", err)
	}
	if len(part) != 3 {
		t.Errorf("partition read returned %d rows, want 3 (an unpartitioned table has partition 1)", len(part))
	}

	none, err := conn.TableStructureSizes(ctx, "dbo", "sqlgopace_no_such_table", nil)
	if err != nil {
		t.Fatalf("TableStructureSizes(missing) error = %v, want nil", err)
	}
	if len(none) != 0 {
		t.Errorf("missing object returned %d rows, want 0", len(none))
	}
}

// TestDisabledIndexesIntegration: the planner needs this because its inventory joins
// sys.dm_db_partition_stats, where a disabled index has no row at all.
func TestDisabledIndexesIntegration(t *testing.T) {
	conn, ctx := openTestConn(t)
	table := "sqlgopace_disabled_probe"
	exec(t, conn, ctx, "DROP TABLE IF EXISTS dbo."+table)
	exec(t, conn, ctx, "CREATE TABLE dbo."+table+" (id INT NOT NULL, v CHAR(50) NOT NULL)")
	exec(t, conn, ctx, "CREATE INDEX IX_"+table+"_off ON dbo."+table+" (v)")
	exec(t, conn, ctx, "ALTER INDEX IX_"+table+"_off ON dbo."+table+" DISABLE")
	t.Cleanup(func() { exec(t, conn, ctx, "DROP TABLE IF EXISTS dbo."+table) })

	inv, err := conn.ObjectInventory(ctx)
	if err != nil {
		t.Fatalf("ObjectInventory() error = %v", err)
	}
	var objectID int64
	for _, o := range inv {
		if o.Table == table && o.Schema == "dbo" {
			objectID = o.ObjectID
			break
		}
	}
	if objectID == 0 {
		t.Fatalf("table %s not found in inventory", table)
	}
	got, err := conn.DisabledIndexes(ctx, objectID)
	if err != nil {
		t.Fatalf("DisabledIndexes: %v", err)
	}
	if len(got) != 1 || got[0] != "IX_"+table+"_off" {
		t.Errorf("DisabledIndexes() = %v, want one disabled index", got)
	}
}
