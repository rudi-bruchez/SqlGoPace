//go:build integration

package mssql_test

import (
	"testing"
)

// TestIndexSizeMBIntegration exercises the three cases the preflight free-space check
// depends on: a real index reports a size, a heap is addressable with an empty index name,
// and an object that does not exist reports 0 rather than an error. The check treats 0 as
// "size unknown" and must never fail a run on it, so the missing-object case is the one
// that matters most.
func TestIndexSizeMBIntegration(t *testing.T) {
	conn, ctx := openTestConn(t)

	const table = "sqlgopace_idxsize"
	exec := func(stmt string) {
		t.Helper()
		if err := conn.ExecDDL(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	exec(`IF OBJECT_ID('dbo.` + table + `') IS NOT NULL DROP TABLE dbo.` + table)
	exec(`CREATE TABLE dbo.` + table + ` (id INT NOT NULL, pad CHAR(200) NOT NULL)`)
	t.Cleanup(func() { _ = conn.ExecDDL(ctx, `DROP TABLE IF EXISTS dbo.`+table) })

	// Enough rows to allocate more than one page, so a heap reports a non-zero size.
	exec(`INSERT INTO dbo.` + table + ` (id, pad)
	      SELECT TOP (2000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)), 'x'
	      FROM sys.all_objects a CROSS JOIN sys.all_objects b`)

	t.Run("heap has a size", func(t *testing.T) {
		got, err := conn.IndexSizeMB(ctx, "dbo", table, "", nil)
		if err != nil {
			t.Fatalf("IndexSizeMB(heap): %v", err)
		}
		if got <= 0 {
			t.Errorf("IndexSizeMB(heap) = %d, want > 0", got)
		}
	})

	t.Run("named index has a size", func(t *testing.T) {
		exec(`CREATE CLUSTERED INDEX IX_` + table + ` ON dbo.` + table + ` (id)`)

		got, err := conn.IndexSizeMB(ctx, "dbo", table, "IX_"+table, nil)
		if err != nil {
			t.Fatalf("IndexSizeMB(index): %v", err)
		}
		if got <= 0 {
			t.Errorf("IndexSizeMB(index) = %d, want > 0", got)
		}
	})

	// The partition filter is the part that cannot be unit tested, and getting it wrong
	// sizes a REBUILD PARTITION = n as the whole index. A non-partitioned index lives
	// entirely in partition 1, so asking for that partition must match the unpartitioned
	// answer, and asking for one that does not exist must report nothing.
	t.Run("partition filter selects the partition", func(t *testing.T) {
		whole, err := conn.IndexSizeMB(ctx, "dbo", table, "IX_"+table, nil)
		if err != nil {
			t.Fatalf("IndexSizeMB(whole): %v", err)
		}
		one := 1
		first, err := conn.IndexSizeMB(ctx, "dbo", table, "IX_"+table, &one)
		if err != nil {
			t.Fatalf("IndexSizeMB(partition 1): %v", err)
		}
		if first != whole {
			t.Errorf("IndexSizeMB(partition 1) = %d, want %d (the whole unpartitioned index)", first, whole)
		}
		absent := 99
		none, err := conn.IndexSizeMB(ctx, "dbo", table, "IX_"+table, &absent)
		if err != nil {
			t.Fatalf("IndexSizeMB(partition 99): %v", err)
		}
		if none != 0 {
			t.Errorf("IndexSizeMB(partition 99) = %d, want 0 (no such partition)", none)
		}
	})

	t.Run("missing object reports zero, not an error", func(t *testing.T) {
		got, err := conn.IndexSizeMB(ctx, "dbo", "sqlgopace_no_such_table", "IX_nope", nil)
		if err != nil {
			t.Fatalf("IndexSizeMB(missing) error = %v, want nil (0 means unknown)", err)
		}
		if got != 0 {
			t.Errorf("IndexSizeMB(missing) = %d, want 0", got)
		}
	})
}
