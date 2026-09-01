package ddl_test

import (
	"errors"
	"fmt"
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
			name: "valid key_range literal update",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: T
    set: { A: 1 }
    confirm_full_table: true
    batch: { strategy: key_range, key: Id }
`,
		},
		{
			name: "key_range rejects delete",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: T
    where:
      - { column: A, op: '=', value: 1 }
    batch: { strategy: key_range }
`,
			wantErr: true,
		},
		{
			name: "key_range rejects set_raw",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: T
    set_raw: "A = A + 1"
    confirm_full_table: true
    batch: { strategy: key_range, key: Id }
`,
			wantErr: true,
		},
		{
			name: "unknown strategy",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: T
    set: { A: 1 }
    confirm_full_table: true
    batch: { strategy: bogus }
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

func TestBatchKeyRangeSQL(t *testing.T) {
	op := parseOneOp(t, `operations:
  - operation: batch_update
    schema: dbo
    table: Orders
    set: { Status: 'Archived' }
    confirm_full_table: true
    batch: { strategy: key_range, key: OrderID }
`).(ddl.BatchDML)

	// First batch: no lower bound; next-key query orders and tops by the key.
	if got, want := ddl.BatchKeyRangeNextSQL(op, "OrderID", 5000, 0, false),
		"SELECT MAX(k) FROM (SELECT TOP (5000) [OrderID] AS k FROM [dbo].[Orders] ORDER BY [OrderID]) x;"; got != want {
		t.Errorf("first next-key SQL\n got: %s\nwant: %s", got, want)
	}
	// Subsequent batch: lower bound on the watermark.
	if got, want := ddl.BatchKeyRangeNextSQL(op, "OrderID", 5000, 1000, true),
		"SELECT MAX(k) FROM (SELECT TOP (5000) [OrderID] AS k FROM [dbo].[Orders] WHERE [OrderID] > 1000 ORDER BY [OrderID]) x;"; got != want {
		t.Errorf("next-key SQL with watermark\n got: %s\nwant: %s", got, want)
	}
	// Update of the (watermark, next] range — no self-limiting clause needed.
	if got, want := ddl.BatchKeyRangeUpdateSQL(op, "OrderID", 1000, 2000, true, ddl.ResolvedOptions{}),
		"UPDATE [dbo].[Orders] SET [Status] = N'Archived' WHERE [OrderID] > 1000 AND [OrderID] <= 2000;"; got != want {
		t.Errorf("range update SQL\n got: %s\nwant: %s", got, want)
	}
	// First-batch update has no lower bound.
	if got, want := ddl.BatchKeyRangeUpdateSQL(op, "OrderID", 0, 2000, false, ddl.ResolvedOptions{}),
		"UPDATE [dbo].[Orders] SET [Status] = N'Archived' WHERE [OrderID] <= 2000;"; got != want {
		t.Errorf("first range update SQL\n got: %s\nwant: %s", got, want)
	}
}

func TestMarshalManifestRoundTripsBatchDML(t *testing.T) {
	src := `operations:
  - operation: batch_update
    schema: dbo
    table: Orders
    set:
      Status: Archived
      Retries: 0
    where:
      - {column: Status, op: '=', value: Pending}
    batch:
      strategy: key_range
      key: OrderID
`
	m, err := ddl.ParseManifest(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	out, err := ddl.MarshalManifest(m)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	// The rendered manifest must re-parse (in particular the literal set values must
	// round-trip as scalars, not the {raw,string} struct).
	m2, err := ddl.ParseManifest(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse marshaled manifest: %v\n%s", err, out)
	}
	b1 := m.Operations[0].(ddl.BatchDML)
	b2 := m2.Operations[0].(ddl.BatchDML)
	if b2.Verb != b1.Verb || b2.Set["Status"] != b1.Set["Status"] || b2.Set["Retries"] != b1.Set["Retries"] || b2.Batch.Strategy != b1.Batch.Strategy || b2.Batch.Key != b1.Batch.Key {
		t.Errorf("round-trip mismatch:\n before: %+v\n  after: %+v", b1, b2)
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

// TestBatchDMLNullTargetSelfLimits pins the termination guarantee for a literal
// UPDATE whose target value is NULL. `col <> NULL` is UNKNOWN for every row, so the
// generic "IS NULL OR <> target" clause matched every row the batch had just set and
// the predicate loop could never exhaust: the clause written to guarantee
// termination guaranteed non-termination. The self-limit for a NULL target is
// `col IS NOT NULL`.
// YAML resolves an unquoted ISO date to !!timestamp, not !!str, so it took
// renderLiteral's unquoted branch and reached T-SQL as three integers and two minus
// signs: `[CreatedAt] > 2020-01-01` is arithmetic (2018), not a date. Against a
// datetime column that is valid and silently compares to the wrong value, which on `>`
// matches every row of a table the author believed was filtered. A date literal in
// T-SQL is a quoted string, so a !!timestamp must take the quoted branch.
func TestBatchDMLQuotesUnquotedDates(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name: "plain date in a where condition",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: MEASUREMENT
    where:
      - { column: CreatedAt, op: '<', value: 2020-01-01 }
`,
			want: "DELETE TOP (5000) FROM [dbo].[MEASUREMENT] WHERE [CreatedAt] < N'2020-01-01';",
		},
		{
			name: "single-digit month and day",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: MEASUREMENT
    where:
      - { column: CreatedAt, op: '<', value: 2020-1-1 }
`,
			want: "DELETE TOP (5000) FROM [dbo].[MEASUREMENT] WHERE [CreatedAt] < N'2020-1-1';",
		},
		{
			name: "full timestamp",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: MEASUREMENT
    where:
      - { column: CreatedAt, op: '>=', value: 2020-01-01T10:00:00Z }
`,
			want: "DELETE TOP (5000) FROM [dbo].[MEASUREMENT] WHERE [CreatedAt] >= N'2020-01-01T10:00:00Z';",
		},
		{
			name: "date as an update target",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: MEASUREMENT
    set: { ArchivedOn: 2020-01-01 }
    where:
      - { column: Archived, op: '=', value: 1 }
`,
			want: "UPDATE TOP (5000) [dbo].[MEASUREMENT] SET [ArchivedOn] = N'2020-01-01' WHERE ([ArchivedOn] IS NULL OR [ArchivedOn] <> N'2020-01-01') AND ([Archived] = 1);",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := parseOneOp(t, tt.manifest).(ddl.BatchDML)
			got := ddl.BatchDMLChunkSQL(op, 5000, ddl.ResolvedOptions{})
			if got != tt.want {
				t.Errorf("BatchDMLChunkSQL()\n got = %s\nwant = %s", got, tt.want)
			}
		})
	}
}

// A quoted date and an unquoted one must generate the same SQL: the YAML author should
// not have to know how the parser resolves a bare scalar to get the query they wrote.
func TestBatchDMLQuotedAndUnquotedDatesAgree(t *testing.T) {
	tmpl := `operations:
  - operation: batch_delete
    schema: dbo
    table: MEASUREMENT
    where:
      - { column: CreatedAt, op: '<', value: %s }
`
	bare := ddl.BatchDMLChunkSQL(parseOneOp(t, fmt.Sprintf(tmpl, "2020-01-01")).(ddl.BatchDML), 100, ddl.ResolvedOptions{})
	quoted := ddl.BatchDMLChunkSQL(parseOneOp(t, fmt.Sprintf(tmpl, "'2020-01-01'")).(ddl.BatchDML), 100, ddl.ResolvedOptions{})
	if bare != quoted {
		t.Errorf("unquoted and quoted dates disagree:\n bare   = %s\n quoted = %s", bare, quoted)
	}
}

func TestBatchDMLNullTargetSelfLimits(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name: "null target",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: Orders
    set: { Status: null }
    where:
      - { column: Archived, op: '=', value: 1 }
`,
			want: "UPDATE TOP (5000) [dbo].[Orders] SET [Status] = NULL WHERE ([Status] IS NOT NULL) AND ([Archived] = 1);",
		},
		{
			name: "tilde is the same null",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: Orders
    set: { Status: ~ }
    confirm_full_table: true
`,
			want: "UPDATE TOP (5000) [dbo].[Orders] SET [Status] = NULL WHERE ([Status] IS NOT NULL);",
		},
		{
			name: "empty scalar is the same null",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: Orders
    set:
      Status:
    confirm_full_table: true
`,
			want: "UPDATE TOP (5000) [dbo].[Orders] SET [Status] = NULL WHERE ([Status] IS NOT NULL);",
		},
		{
			name: "null mixed with a value target",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: Orders
    set: { Note: null, Status: 'Archived' }
    confirm_full_table: true
`,
			want: "UPDATE TOP (5000) [dbo].[Orders] SET [Note] = NULL, [Status] = N'Archived' WHERE ([Note] IS NOT NULL OR [Status] IS NULL OR [Status] <> N'Archived');",
		},
		{
			name: "the string \"null\" is not a null",
			manifest: `operations:
  - operation: batch_update
    schema: dbo
    table: Orders
    set: { Status: 'null' }
    confirm_full_table: true
`,
			want: "UPDATE TOP (5000) [dbo].[Orders] SET [Status] = N'null' WHERE ([Status] IS NULL OR [Status] <> N'null');",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, ok := parseOneOp(t, tt.manifest).(ddl.BatchDML)
			if !ok {
				t.Fatalf("not a BatchDML")
			}
			if got := ddl.BatchDMLChunkSQL(op, 5000, ddl.ResolvedOptions{}); got != tt.want {
				t.Errorf("BatchDMLChunkSQL:\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

// TestMarshalManifestRoundTripsNullSetValue guards the rewrite path for a NULL set
// target. MarshalManifest is the inverse of ParseManifest and is used whenever a
// manifest is rewritten in place — a recovery manifest, the blocked-session capture,
// the TUI's kill-rule append. A null literal renders as an empty scalar, which
// compact() drops as carrying no information; that emptied the `set:` map and then
// dropped `set:` itself, so the rewritten manifest no longer parsed at all.
func TestMarshalManifestRoundTripsNullSetValue(t *testing.T) {
	src := `operations:
  - operation: batch_update
    schema: dbo
    table: Orders
    set: { Note: null, Status: 'Archived' }
    confirm_full_table: true
`
	m, err := ddl.ParseManifest(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	out, err := ddl.MarshalManifest(m)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	m2, err := ddl.ParseManifest(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("reparse of rendered manifest: %v\n--- rendered ---\n%s", err, out)
	}
	op, ok := m2.Operations[0].(ddl.BatchDML)
	if !ok {
		t.Fatalf("not a BatchDML after round trip")
	}
	if len(op.Set) != 2 {
		t.Fatalf("set has %d entries after round trip, want 2\n--- rendered ---\n%s", len(op.Set), out)
	}
	if !op.Set["Note"].IsNull() {
		t.Errorf("Note = %#v after round trip, want a null literal", op.Set["Note"])
	}
	// The generated SQL must be identical on both sides, or a rewrite silently
	// changes what the operation does.
	want := ddl.BatchDMLChunkSQL(m.Operations[0].(ddl.BatchDML), 1000, ddl.ResolvedOptions{})
	if got := ddl.BatchDMLChunkSQL(op, 1000, ddl.ResolvedOptions{}); got != want {
		t.Errorf("SQL changed across a rewrite:\n got %s\nwant %s", got, want)
	}
}

// TestBatchUnmatchedRowsSQL pins the selectivity probe. It asks "does this predicate
// spare any row at all", capped, so preflight can tell a genuinely selective filter
// from `where_raw: "1=1"` — which walks past confirm_full_table today. The CASE wrapper
// is what makes it correct under three-valued logic: a plain NOT (pred) drops the rows
// where the predicate is UNKNOWN, which the DML does not act on either, so they must
// count as spared.
func TestBatchUnmatchedRowsSQL(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name: "declarative where",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: AuditLog
    where:
      - { column: CreatedAt, op: '<', value: '2024-01-01' }
`,
			want: "SELECT COUNT(*) FROM (SELECT TOP (1000) 1 AS c FROM [dbo].[AuditLog] WHERE CASE WHEN ([CreatedAt] < N'2024-01-01') THEN 1 ELSE 0 END = 0) x;",
		},
		{
			name: "raw where",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: Orders
    where_raw: "1=1"
`,
			want: "SELECT COUNT(*) FROM (SELECT TOP (1000) 1 AS c FROM [dbo].[Orders] WHERE CASE WHEN (1=1) THEN 1 ELSE 0 END = 0) x;",
		},
		{
			name: "no predicate has nothing to probe",
			manifest: `operations:
  - operation: batch_delete
    schema: dbo
    table: Scratch
    confirm_full_table: true
`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := parseOneOp(t, tt.manifest).(ddl.BatchDML)
			if got := ddl.BatchUnmatchedRowsSQL(op, 1000); got != tt.want {
				t.Errorf("BatchUnmatchedRowsSQL:\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}
