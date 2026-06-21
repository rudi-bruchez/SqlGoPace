package ddl_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// parseOneOp parses a manifest with a single operation and returns it.
func parseOneOp(t *testing.T, manifest string) ddl.Operation {
	t.Helper()
	m, err := ddl.ParseManifest(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.Operations) != 1 {
		t.Fatalf("got %d operations, want 1", len(m.Operations))
	}
	return m.Operations[0]
}

func TestParseBatchDMLValidation(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		wantErr  bool
	}{
		{
			name: "valid literal update",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: Orders
    set: { Status: 'Archived' }
    where:
      - { column: Status, op: '=', value: 'Pending' }
`,
		},
		{
			name: "valid delete",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: AuditLog
    where:
      - { column: CreatedAt, op: '<', value: '2024-01-01' }
`,
		},
		{
			name: "valid whole-table delete with confirm",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: Scratch
    confirm_full_table: true
`,
		},
		{
			name: "valid raw update",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: Invoice
    set_raw: "Status = 'Closed'"
    where_raw: "Status = 'Open'"
`,
		},
		{
			name: "update with neither set nor set_raw",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: T
    where:
      - { column: A, op: '=', value: 1 }
`,
			wantErr: true,
		},
		{
			name: "update with both set and set_raw",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: T
    set: { A: 1 }
    set_raw: "A = 1"
    where:
      - { column: A, op: '<>', value: 1 }
`,
			wantErr: true,
		},
		{
			name: "delete with set",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: T
    set: { A: 1 }
    where:
      - { column: A, op: '=', value: 1 }
`,
			wantErr: true,
		},
		{
			name: "both where and where_raw",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: T
    where:
      - { column: A, op: '=', value: 1 }
    where_raw: "A = 1"
`,
			wantErr: true,
		},
		{
			name: "unknown operator",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: T
    where:
      - { column: A, op: 'LIKE', value: 'x%' }
`,
			wantErr: true,
		},
		{
			name: "is null with a value",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: T
    where:
      - { column: A, op: 'is null', value: 1 }
`,
			wantErr: true,
		},
		{
			name: "comparison without a value",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: T
    where:
      - { column: A, op: '>' }
`,
			wantErr: true,
		},
		{
			name: "whole-table delete without confirm",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: T
`,
			wantErr: true,
		},
		{
			name: "raw whole-table update cannot self-limit",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: T
    set_raw: "A = 1"
    confirm_full_table: true
`,
			wantErr: true,
		},
		{
			name: "key_range strategy not yet supported",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: T
    set: { A: 1 }
    confirm_full_table: true
    batch: { strategy: key_range, key: Id }
`,
			wantErr: true,
		},
		{
			name: "non-positive initial_rows",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: T
    set: { A: 1 }
    confirm_full_table: true
    batch: { initial_rows: 0 }
`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ddl.ParseManifest(strings.NewReader(tt.manifest))
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("ParseManifest error = %v, want error presence = %t", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ddl.ErrInvalidManifest) {
				t.Errorf("error = %v, want errors.Is ErrInvalidManifest", err)
			}
		})
	}
}

func TestBatchDMLChunkSQL(t *testing.T) {
	maxdop1 := 1
	tests := []struct {
		name     string
		manifest string
		size     int
		res      ddl.ResolvedOptions
		want     string
	}{
		{
			name: "literal update self-limits and ANDs the filter",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: Orders
    set: { Status: 'Archived' }
    where:
      - { column: Status, op: '=', value: 'Pending' }
`,
			size: 5000,
			want: "UPDATE TOP (5000) [dbo].[Orders] SET [Status] = N'Archived' WHERE ([Status] IS NULL OR [Status] <> N'Archived') AND ([Status] = N'Pending');",
		},
		{
			name: "delete with filter",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: AuditLog
    where:
      - { column: CreatedAt, op: '<', value: '2024-01-01' }
`,
			size: 10000,
			want: "DELETE TOP (10000) FROM [dbo].[AuditLog] WHERE [CreatedAt] < N'2024-01-01';",
		},
		{
			name: "raw update splices set and where verbatim",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: Invoice
    set_raw: "Status = 'Closed', ClosedAt = SYSUTCDATETIME()"
    where_raw: "Status = 'Open'"
`,
			size: 1000,
			want: "UPDATE TOP (1000) [dbo].[Invoice] SET Status = 'Closed', ClosedAt = SYSUTCDATETIME() WHERE Status = 'Open';",
		},
		{
			name: "whole-table literal update is self-limited only",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: Big
    set: { Flag: 1 }
    confirm_full_table: true
`,
			size: 2000,
			want: "UPDATE TOP (2000) [dbo].[Big] SET [Flag] = 1 WHERE ([Flag] IS NULL OR [Flag] <> 1);",
		},
		{
			name: "whole-table delete has no where",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: Scratch
    confirm_full_table: true
`,
			size: 4000,
			want: "DELETE TOP (4000) FROM [dbo].[Scratch];",
		},
		{
			name: "maxdop hint appended",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: AuditLog
    where:
      - { column: A, op: '=', value: 7 }
`,
			size: 500,
			res:  ddl.ResolvedOptions{MaxDOP: &maxdop1},
			want: "DELETE TOP (500) FROM [dbo].[AuditLog] WHERE [A] = 7 OPTION (MAXDOP 1);",
		},
		{
			name: "multiple set columns are sorted",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: T
    set: { Zeta: 1, Alpha: 2 }
    confirm_full_table: true
`,
			size: 100,
			want: "UPDATE TOP (100) [dbo].[T] SET [Alpha] = 2, [Zeta] = 1 WHERE ([Alpha] IS NULL OR [Alpha] <> 2 OR [Zeta] IS NULL OR [Zeta] <> 1);",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, ok := parseOneOp(t, tt.manifest).(ddl.BatchDML)
			if !ok {
				t.Fatalf("operation is not a BatchDML")
			}
			if got := ddl.BatchDMLChunkSQL(op, tt.size, tt.res); got != tt.want {
				t.Errorf("BatchDMLChunkSQL\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestResolveBatchDML(t *testing.T) {
	allEditions := []ddl.Tier{ddl.TierEnterprise, ddl.TierStandard, ddl.TierExpress, ddl.TierAzure}
	m := &ddl.Matrix{
		AzurePseudoMajor: 9999,
		Commands: map[string]ddl.CommandRules{
			"batch_update": {"maxdop": {MinMajor: 9, Editions: allEditions}},
			"batch_delete": {"maxdop": {MinMajor: 9, Editions: allEditions}},
		},
	}
	op := parseOneOp(t, `operations:
  - operation: batch_update
    schema: dbo
    table: T
    set: { A: 1 }
    confirm_full_table: true
    options: { maxdop: 4, ignore_blocking: true, max_block_minutes: 30 }
`)

	res, decisions := ddl.Resolve(op, ddl.Target{MajorVersion: 16, Tier: ddl.TierStandard}, m, ddl.Policy{})

	if res.MaxDOP == nil || *res.MaxDOP != 4 {
		t.Errorf("MaxDOP = %v, want 4", res.MaxDOP)
	}
	if !res.IgnoreBlocking {
		t.Errorf("IgnoreBlocking = false, want true")
	}
	if res.MaxBlockMinutes != 30 {
		t.Errorf("MaxBlockMinutes = %d, want 30", res.MaxBlockMinutes)
	}
	// No ONLINE/RESUMABLE/WALP family is even considered for DML.
	if res.Online || res.Resumable || res.WaitAtLowPriority {
		t.Errorf("DML resolved an index option family: %+v", res)
	}
	for _, d := range decisions {
		switch d.Option {
		case "online", "resumable", "wait_at_low_priority", "sort_in_tempdb":
			t.Errorf("unexpected decision for DML: %+v", d)
		}
	}
}

func TestExpandPreservesBatchDML(t *testing.T) {
	m, err := ddl.ParseManifest(strings.NewReader(`operations:
  - operation: batch_delete
    schema: dbo
    table: AuditLog
    where:
      - { column: A, op: '=', value: 1 }
`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	out, err := ddl.ExpandRebuildAll(m, func(string, string) ([]ddl.IndexDescriptor, error) {
		t.Fatalf("lookup should not be called for a batch op")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ExpandRebuildAll: %v", err)
	}
	if len(out.Operations) != 1 {
		t.Fatalf("got %d operations, want 1", len(out.Operations))
	}
	if _, ok := out.Operations[0].(ddl.BatchDML); !ok {
		t.Errorf("operation type = %T, want ddl.BatchDML", out.Operations[0])
	}
}
