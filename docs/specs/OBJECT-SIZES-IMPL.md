# Object sizes implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Report the used size of every structure a `rebuild_index`, `rebuild_heap` or
`reorganize_index` rewrites, before and after, and make the heap rebuild's real scope — all
nonclustered indexes, including a disabled one it re-enables — visible to the preflight, the dry
run, the console and the planner.

**Architecture:** One new SQL read (`TableStructureSizes`) returns every heap/index of a table with
its used KB; one pure selector (`preflight.Rewritten`) says which of those rows an operation
rewrites. Preflight sizes its space check from that sum and refuses a heap rebuild that would
re-enable a disabled index unless the operation opts in; the engine reads the same rows before and
after each operation and reports per structure, per manifest and (through two new `runs` columns)
per campaign; the planner computes heap + nonclustered totals from the inventory it already has.

**Tech Stack:** Go (stdlib + `github.com/microsoft/go-mssqldb`, `gopkg.in/yaml.v3`, Bubble Tea for
the console, SQLite for history). Tests are `go test -race`, no database except the
`integration`-tagged files.

**Spec:** [OBJECT-SIZES.md](OBJECT-SIZES.md) — read it before starting; the probe results it rests on
are in [OBJECT-SIZES-ANALYSIS.md](OBJECT-SIZES-ANALYSIS.md).

## Global Constraints

- **English only**, US spelling, everywhere including comments and these docs.
- **Never commit client identifiers.** Use `PRODDB`, `dbo.MEASUREMENT`, `PK_MEASUREMENT`,
  `SQLPROD01`, `CORP\svc_sqlagent` in tests and docs.
- **No `context.WithTimeout` around executing DDL.** The size reads are not DDL; they take the
  caller's context as-is.
- **TDD**: write the failing test, run it, implement, run it, commit. One commit per task.
- **Idiomatic Go, KISS.** No new abstraction the task does not need.
- **The checkout is CRLF** (`core.autocrlf=true`), so plain `gofmt -l` flags every file. Check a file
  with `tr -d '\r' < FILE | gofmt -l -`; `golangci-lint run ./...` currently reports ~118 `gofmt`
  issues that are this noise and nothing else.
- **A size read never changes an outcome.** Every failure degrades to "unknown"; the only new
  failure in the whole plan is the preflight disabled-index guard (Task 6).
- **Sizes are kilobytes** end to end (`used_page_count * 8`). Only the renderer humanizes.
- **Do not touch** `internal/config/audit_test.go`, `internal/tui/harm_audit_test.go`, or any
  `docs/specs/REVIEW-*.md`.
- Commit messages end with:
  `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01VJMk27TibZLG7nRsnGY6Qm`.

## File Structure

| File | Responsibility | Task |
| --- | --- | --- |
| `internal/mssql/indexes.go` | `StructureSize`, `TableStructureSizes`, `DisabledIndexes`; `IndexSizeMB` deleted | 1, 11 |
| `internal/mssql/indexes_integration_test.go` | server-backed coverage of both reads | 1, 11 |
| `internal/preflight/sizes.go` (new) | `Rewritten`, `SumKB`, `UnsizedDisabled` — pure selection over `[]mssql.StructureSize` | 2 |
| `internal/preflight/preflight.go` | `Prober` swap, space check sum, scope `Warn`, disabled-index guard | 3, 6 |
| `internal/ddl/manifest.go` | `RebuildHeap.AllowReenableDisabledIndexes` | 5 |
| `internal/report/report.go` | `SizeLine`, `OperationReport.Sizes`, run totals, `HeapScopeNotices`, `HumanizeKB`, rendering | 7 |
| `internal/report/history.go` | `runs.size_before_kb` / `size_after_kb` | 8 |
| `internal/run/sizes.go` (new) | `SizeReader`, `WithSizeReader`, before/after reads, per-structure totals | 9 |
| `internal/run/engine.go` | call the reads, fill the report, manifest-start scope, `OpInfo.Detail` | 9, 10 |
| `internal/run/step.go` | `OpInfo.Detail` | 10 |
| `internal/tui/model.go`, `view.go` | `OperationRow.Detail`, `StepDoneMsg.Detail`, row rendering | 12 |
| `cmd/sqlgopace/main.go` | `WithSizeReader` wiring, forwarder details, dry-run heap lines | 13 |
| `internal/plan/plan.go`, `internal/maint/decide.go` | heap totals, both bounds, disabled-index skip | 14 |
| docs, `CHANGELOG.md`, `internal/version/VERSION` | 0.35.0 | 15 |

Task order matters: 1 → 2 → 3 (preflight space), 4 → 5 → 6 (the guard), 7 → 8 (report shape),
9 → 10 (engine), 11 → 14 (planner), 12 → 13 (console), 15 last.

---

### Task 1: `TableStructureSizes` — one read for every structure of a table

**Files:**
- Modify: `internal/mssql/indexes.go:95-125` (replace `indexSizeMBSQL` and `IndexSizeMB`)
- Modify: `internal/mssql/indexes_integration_test.go:9-90`

**Interfaces:**
- Consumes: nothing.
- Produces: `mssql.StructureSize{IndexID int; Name, TypeDesc string; Disabled bool; UsedKB int64}` and
  `func (c *Conn) TableStructureSizes(ctx context.Context, schema, table string, partition *int) ([]StructureSize, error)`.

- [ ] **Step 1: Write the failing integration test**

Replace `TestIndexSizeMBIntegration` in `internal/mssql/indexes_integration_test.go` with the test
below. Keep the file's existing build tag and connection helper (read the top of the file first and
reuse `openTestConn`/`exec` exactly as the neighbouring tests do).

```go
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
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go vet -tags integration ./internal/mssql`
Expected: FAIL — `conn.TableStructureSizes undefined`.

- [ ] **Step 3: Implement the read**

In `internal/mssql/indexes.go`, delete `indexSizeMBSQL` and `IndexSizeMB` and add:

```go
// StructureSize is one heap or index of a table, its used pages summed over partitions.
// A disabled nonclustered index has no pages at all ("Disabling ... a nonclustered index
// physically deletes the index data"), so it is listed with UsedKB 0 rather than dropped:
// ALTER TABLE ... REBUILD on a heap rebuilds it from the table, so it is part of the
// rewrite even though nothing can size it in advance.
type StructureSize struct {
	IndexID  int    // 0 = heap
	Name     string // empty for the heap
	TypeDesc string // sys.indexes.type_desc
	Disabled bool   // sys.indexes.is_disabled
	UsedKB   int64
}

// tableStructureSizesSQL lists every structure of one table with its used size.
// LEFT JOIN, with the partition filter inside the join, so a structure with no
// allocated pages still gets a row. used_page_count is in 8-KB pages.
const tableStructureSizesSQL = `
SELECT i.index_id, i.name, i.type_desc, i.is_disabled,
       COALESCE(SUM(ps.used_page_count), 0) * 8 AS used_kb
FROM sys.indexes i
LEFT JOIN sys.dm_db_partition_stats ps
  ON ps.object_id = i.object_id AND ps.index_id = i.index_id
 AND (@partition = 0 OR ps.partition_number = @partition)
WHERE i.object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table))
  AND i.is_hypothetical = 0
GROUP BY i.index_id, i.name, i.type_desc, i.is_disabled
ORDER BY i.index_id;`

// TableStructureSizes returns every heap/index of [schema].[table] with its used size in
// KB, summed across partitions (or for one partition when partition is set). A missing
// object yields no rows, which callers read as "size unknown" and must never fail a run on.
func (c *Conn) TableStructureSizes(ctx context.Context, schema, table string, partition *int) ([]StructureSize, error) {
	// 0 means "every partition": partition numbers start at 1, so it cannot collide.
	part := 0
	if partition != nil {
		part = *partition
	}
	rows, err := c.pool.QueryContext(ctx, tableStructureSizesSQL,
		sql.Named("schema", schema), sql.Named("table", table), sql.Named("partition", part))
	if err != nil {
		return nil, fmt.Errorf("structure sizes %s.%s: %w", schema, table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []StructureSize
	for rows.Next() {
		var (
			s    StructureSize
			name sql.NullString
		)
		if err := rows.Scan(&s.IndexID, &name, &s.TypeDesc, &s.Disabled, &s.UsedKB); err != nil {
			return nil, fmt.Errorf("scan structure size row: %w", err)
		}
		s.Name = name.String
		out = append(out, s)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Verify it compiles and the package still builds**

Run: `go build ./... && go vet -tags integration ./internal/mssql`
Expected: `internal/preflight` now fails to build (`IndexSizeMB` gone) — that is Task 3's job. If
you want a green tree before then, do Tasks 1-3 in one sitting and commit at the end of Task 3.
Run the integration test if a server is available:
`SQLGOPACE_TEST_DSN=... go test -tags integration ./internal/mssql -run TableStructureSizes -v`

- [ ] **Step 5: Commit**

```bash
git add internal/mssql/indexes.go internal/mssql/indexes_integration_test.go
git commit -m "feat(mssql): read every structure of a table with its used size"
```

---

### Task 2: `Rewritten` — which structures an operation rewrites

**Files:**
- Create: `internal/preflight/sizes.go`
- Create: `internal/preflight/sizes_test.go`

**Interfaces:**
- Consumes: `mssql.StructureSize` (Task 1).
- Produces: `preflight.Rewritten(op ddl.Operation, sizes []mssql.StructureSize) []mssql.StructureSize`,
  `preflight.SumKB(sizes []mssql.StructureSize) int64`,
  `preflight.DisabledNames(sizes []mssql.StructureSize) []string`,
  `preflight.SizedOperation(op ddl.Operation) (schema, table string, partition *int, ok bool)`.

- [ ] **Step 1: Write the failing test**

```go
package preflight_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
)

var probeSizes = []mssql.StructureSize{
	{IndexID: 0, Name: "", TypeDesc: "HEAP", UsedKB: 5_000_000},
	{IndexID: 2, Name: "IX_MEASUREMENT_TS", TypeDesc: "NONCLUSTERED", UsedKB: 2_000_000},
	{IndexID: 3, Name: "IX_MEASUREMENT_OLD", TypeDesc: "NONCLUSTERED", Disabled: true},
	{IndexID: 4, Name: "NCCI_MEASUREMENT", TypeDesc: "NONCLUSTERED COLUMNSTORE", UsedKB: 900_000},
}

// TestRewritten pins the rule the whole feature rests on: ALTER TABLE ... REBUILD on a
// heap rewrites the heap AND every other structure of the table (verified on a server,
// OBJECT-SIZES-ANALYSIS.md), while an index operation rewrites only the index it names.
func TestRewritten(t *testing.T) {
	tests := []struct {
		name string
		op   ddl.Operation
		want []string // Name of each returned structure, in order
	}{
		{"rebuild_heap takes everything", ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT"},
			[]string{"", "IX_MEASUREMENT_TS", "IX_MEASUREMENT_OLD", "NCCI_MEASUREMENT"}},
		{"rebuild_index takes its index", ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "IX_MEASUREMENT_TS"},
			[]string{"IX_MEASUREMENT_TS"}},
		{"reorganize_index takes its index", ddl.ReorganizeIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "NCCI_MEASUREMENT"},
			[]string{"NCCI_MEASUREMENT"}},
		{"unknown index name takes nothing", ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "IX_absent"}, nil},
		{"another operation takes nothing", ddl.UpdateStatistics{Schema: "dbo", Table: "MEASUREMENT"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preflight.Rewritten(tt.op, probeSizes)
			if len(got) != len(tt.want) {
				t.Fatalf("Rewritten() = %d structures, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, w := range tt.want {
				if got[i].Name != w {
					t.Errorf("structure %d = %q, want %q", i, got[i].Name, w)
				}
			}
		})
	}
}

// TestSumKBIgnoresNothing: the sum is what the rewrite needs room for; a disabled index
// contributes 0 because it has no pages to copy, not because it is excluded.
func TestSumKB(t *testing.T) {
	if got, want := preflight.SumKB(preflight.Rewritten(ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT"}, probeSizes)), int64(7_900_000); got != want {
		t.Errorf("SumKB() = %d, want %d", got, want)
	}
}

func TestDisabledNames(t *testing.T) {
	got := preflight.DisabledNames(probeSizes)
	if len(got) != 1 || got[0] != "IX_MEASUREMENT_OLD" {
		t.Errorf("DisabledNames() = %v, want [IX_MEASUREMENT_OLD]", got)
	}
}

// TestSizedOperation names the operations worth a size read at all.
func TestSizedOperation(t *testing.T) {
	part := 3
	tests := []struct {
		op        ddl.Operation
		wantTable string
		wantPart  *int
		wantOK    bool
	}{
		{ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "IX", Partition: &part}, "MEASUREMENT", &part, true},
		{ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT"}, "MEASUREMENT", nil, true},
		{ddl.ReorganizeIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "IX"}, "MEASUREMENT", nil, true},
		{ddl.UpdateStatistics{Schema: "dbo", Table: "MEASUREMENT"}, "", nil, false},
	}
	for _, tt := range tests {
		_, table, gotPart, ok := preflight.SizedOperation(tt.op)
		if ok != tt.wantOK || table != tt.wantTable {
			t.Errorf("SizedOperation(%T) = (%q, %t), want (%q, %t)", tt.op, table, ok, tt.wantTable, tt.wantOK)
		}
		if tt.wantPart == nil && gotPart != nil {
			t.Errorf("SizedOperation(%T) partition = %v, want nil", tt.op, *gotPart)
		}
		if tt.wantPart != nil && (gotPart == nil || *gotPart != *tt.wantPart) {
			t.Errorf("SizedOperation(%T) partition = %v, want %d", tt.op, gotPart, *tt.wantPart)
		}
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./internal/preflight -run 'TestRewritten|TestSumKB|TestDisabledNames|TestSizedOperation' -v`
Expected: FAIL — `undefined: preflight.Rewritten`.

- [ ] **Step 3: Implement**

Create `internal/preflight/sizes.go`:

```go
package preflight

import (
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// SizedOperation reports the table an operation's size is read from, and whether it is
// one of the operations this project measures: rebuild_index, rebuild_heap and
// reorganize_index, the three that exist to make an object smaller or denser.
//
// partition is carried through because `REBUILD PARTITION = n` rewrites one partition and
// needs room for that partition alone.
func SizedOperation(op ddl.Operation) (schema, table string, partition *int, ok bool) {
	switch o := op.(type) {
	case ddl.RebuildIndex:
		return o.Schema, o.Table, o.Partition, true
	case ddl.RebuildHeap:
		return o.Schema, o.Table, nil, true
	case ddl.ReorganizeIndex:
		return o.Schema, o.Table, o.Partition, true
	default:
		return "", "", nil, false
	}
}

// Rewritten returns the structures op rewrites, out of one table's sizes.
//
// A heap rebuild takes them all: "If the table is a heap, all nonclustered indexes are
// rebuilt" (ALTER TABLE docs), verified on a server to include a nonclustered columnstore
// and a disabled index, which it re-enables (OBJECT-SIZES-ANALYSIS.md). An index
// operation takes the one index it names; an unknown name returns nothing, which callers
// read as "size unknown".
func Rewritten(op ddl.Operation, sizes []mssql.StructureSize) []mssql.StructureSize {
	switch o := op.(type) {
	case ddl.RebuildHeap:
		return sizes
	case ddl.RebuildIndex:
		return named(sizes, o.Index)
	case ddl.ReorganizeIndex:
		return named(sizes, o.Index)
	default:
		return nil
	}
}

func named(sizes []mssql.StructureSize, index string) []mssql.StructureSize {
	for _, s := range sizes {
		if strings.EqualFold(s.Name, index) && s.Name != "" {
			return []mssql.StructureSize{s}
		}
	}
	return nil
}

// SumKB totals the used size of the given structures.
func SumKB(sizes []mssql.StructureSize) int64 {
	var kb int64
	for _, s := range sizes {
		kb += s.UsedKB
	}
	return kb
}

// DisabledNames lists the disabled indexes among the given structures, in index_id order.
func DisabledNames(sizes []mssql.StructureSize) []string {
	var out []string
	for _, s := range sizes {
		if s.Disabled {
			out = append(out, s.Name)
		}
	}
	return out
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/preflight -run 'TestRewritten|TestSumKB|TestDisabledNames|TestSizedOperation' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/preflight/sizes.go internal/preflight/sizes_test.go
git commit -m "feat(preflight): name the structures each operation rewrites"
```

---

### Task 3: the space check counts the whole rewrite

**Files:**
- Modify: `internal/preflight/preflight.go:139-175` (`CheckDataFreeSpace`), `:186-216`
  (delete `rebuiltObject`, keep `rebuiltObjectLabel`), `:326-348` (`Prober`), `:448-461` (the loop)
- Modify: `internal/preflight/preflight_test.go:281-290` (`fakeProber.IndexSizeMB`) and the
  `CheckDataFreeSpace` tests around `:85-95`

**Interfaces:**
- Consumes: `Rewritten`, `SumKB`, `DisabledNames`, `SizedOperation` (Task 2);
  `Conn.TableStructureSizes` (Task 1).
- Produces: `CheckDataFreeSpace(target string, needMB int, unsizedDisabled int, sp DataSpace) Check`;
  `Prober.TableStructureSizes(ctx, schema, table string, partition *int) ([]mssql.StructureSize, error)`
  replacing `Prober.IndexSizeMB`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/preflight/preflight_test.go`:

```go
// TestCheckDataFreeSpaceCountsUnsizedDisabled: a disabled index cannot be sized (it has no
// pages) but the rebuild recreates it, so the check must say the figure is incomplete
// instead of passing on an understated need.
func TestCheckDataFreeSpaceCountsUnsizedDisabled(t *testing.T) {
	c := preflight.CheckDataFreeSpace("dbo.MEASUREMENT (heap)", 500, 1,
		preflight.DataSpace{FreeMB: 5000, GrowthKnown: true})
	if c.Severity != preflight.Pass {
		t.Fatalf("Severity = %v, want Pass", c.Severity)
	}
	if !strings.Contains(c.Detail, "1 disabled index") {
		t.Errorf("Detail = %q, want it to name the unsized disabled index", c.Detail)
	}
}

// TestHeapRebuildSizedFromWholeTable is the defect this fixes: the heap rebuild needs room
// for the heap AND every nonclustered index, not for the heap alone (MAINTENANCE.md §9).
func TestHeapRebuildSizedFromWholeTable(t *testing.T) {
	p := fakeProber{
		structures: []mssql.StructureSize{
			{IndexID: 0, TypeDesc: "HEAP", UsedKB: 5 * 1024 * 1024},             // 5 GB
			{IndexID: 2, Name: "IX_A", TypeDesc: "NONCLUSTERED", UsedKB: 2 * 1024 * 1024}, // 2 GB
		},
		files: []mssql.FileSpace{{FreeMB: 6000}},
	}
	m := &ddl.Manifest{Operations: []ddl.Operation{ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT"}}}
	rep, err := preflight.Run(context.Background(), p, spaceServerInfo, m,
		preflight.Thresholds{RequireDataFreeSpace: true}, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var detail string
	for _, c := range rep.Checks {
		if c.Name == "data free space" {
			detail = c.Detail
		}
	}
	if !strings.Contains(detail, "7168 MB") {
		t.Errorf("data free space detail = %q, want the 7168 MB rewrite (5 GB heap + 2 GB index)", detail)
	}
}
```

`fakeProber` currently answers `IndexSizeMB`; replace that method with:

```go
func (f fakeProber) TableStructureSizes(_ context.Context, _, _ string, _ *int) ([]mssql.StructureSize, error) {
	if f.structuresErr != nil {
		return nil, f.structuresErr
	}
	return f.structures, nil
}
```

and add the two fields (`structures []mssql.StructureSize`, `structuresErr error`) to the struct.
`spaceServerInfo` is whatever `ServerInfo` value the neighbouring space tests already use — reuse
it rather than inventing one; if none exists, use `batchServerInfo`.

- [ ] **Step 2: Run the tests to make sure they fail**

Run: `go test ./internal/preflight -run 'DataFreeSpace|HeapRebuildSized' -v`
Expected: FAIL to compile — `CheckDataFreeSpace` takes 3 arguments, `fakeProber` has no
`structures`.

- [ ] **Step 3: Implement**

1. In `Prober`, replace the `IndexSizeMB` line with:

```go
	TableStructureSizes(ctx context.Context, schema, table string, partition *int) ([]mssql.StructureSize, error)
```

2. Change `CheckDataFreeSpace`'s signature and its first two branches:

```go
// unsizedDisabled counts disabled indexes the rewrite recreates: they have no pages, so
// needMB cannot include them, and a check that stayed silent would understate the need.
func CheckDataFreeSpace(target string, needMB, unsizedDisabled int, sp DataSpace) Check {
	const name = "data free space"
	freeMB := sp.FreeMB
	extra := ""
	if unsizedDisabled > 0 {
		extra = fmt.Sprintf("; + %d disabled index(es) of unknown size, rebuilt from the table", unsizedDisabled)
	}
	switch {
	case needMB <= 0:
		return Check{name, Pass, fmt.Sprintf("%s: size unknown, not checked (%d MB free in data files)%s", target, freeMB, extra)}
	case freeMB >= needMB:
		return Check{name, Pass, fmt.Sprintf("%s: %d MB free, ~%d MB needed%s", target, freeMB, needMB, extra)}
	...
```

Append `extra` to the remaining branch messages the same way.

3. Delete `rebuiltObject` and rewrite the loop body in `Run` (around `:449`):

```go
		if th.RequireDataFreeSpace {
			if schema, table, partition, ok := SizedOperation(op); ok {
				if _, isReorg := op.(ddl.ReorganizeIndex); !isReorg {
					// A size we cannot read is reported as unknown (0), never as a failed run:
					// sys.dm_db_partition_stats also wants VIEW DEFINITION, which the documented
					// VIEW SERVER STATE does not imply, so a legitimate login can be refused it.
					sizes, err := p.TableStructureSizes(ctx, schema, table, partition)
					if err != nil {
						sizes = nil
					}
					rewritten := Rewritten(op, sizes)
					needMB := int((SumKB(rewritten) + 1023) / 1024)
					rep.add(CheckDataFreeSpace(rewrittenLabel(op), needMB, len(DisabledNames(rewritten)),
						DataSpace{FreeMB: dataFreeMB, Growth: dataGrowth, GrowthKnown: growthKnown}))
				}
			}
		}
```

`reorganize_index` is excluded from the space check: `ALTER INDEX ... REORGANIZE` is listed under
"Index operations that require no additional disk space"
(https://learn.microsoft.com/sql/relational-databases/indexes/disk-space-requirements-for-index-ddl-operations).
It is still measured by the engine.

4. Replace `rebuiltObjectLabel`'s callers with a small wrapper that keeps today's wording:

```go
// rewrittenLabel names the object for a check detail, distinguishing a heap (which has no
// index name) from a named index, and naming the partition when only one is rewritten.
func rewrittenLabel(op ddl.Operation) string {
	switch o := op.(type) {
	case ddl.RebuildHeap:
		return fmt.Sprintf("%s.%s (heap)", o.Schema, o.Table)
	case ddl.RebuildIndex:
		name := fmt.Sprintf("%s.%s.%s", o.Schema, o.Table, o.Index)
		if o.Partition != nil {
			name += fmt.Sprintf(" partition %d", *o.Partition)
		}
		return name
	case ddl.ReorganizeIndex:
		return fmt.Sprintf("%s.%s.%s", o.Schema, o.Table, o.Index)
	default:
		return op.Target().String()
	}
}
```

Delete `rebuiltObjectLabel` and its tests, or rename them onto `rewrittenLabel` — do not leave two
label builders.

- [ ] **Step 4: Run the package tests**

Run: `go test -race ./internal/preflight ./internal/mssql`
Expected: PASS. Fix any other `fakeProber`/`IndexSizeMB` references the compiler names.

- [ ] **Step 5: Commit**

```bash
git add internal/preflight internal/mssql
git commit -m "fix(preflight): size a heap rebuild from the whole table, not the heap alone"
```

---

### Task 4: `report.HumanizeKB`

**Files:**
- Modify: `internal/report/report.go` (add near the bottom, before `Write`'s helpers)
- Modify: `internal/report/report_test.go`

**Interfaces:**
- Produces: `report.HumanizeKB(kb int64) string`.

- [ ] **Step 1: Write the failing test**

```go
// TestHumanizeKB pins the boundaries, including the TB step: a 1.4 TB object exists in the
// field (TODO.md), and rendering it as "1433.6 GB" while the console header says TB for the
// same database reads as a bug.
func TestHumanizeKB(t *testing.T) {
	tests := []struct {
		kb   int64
		want string
	}{
		{0, "0 KB"},
		{812, "812 KB"},
		{1024, "1.0 MB"},
		{12_700, "12.4 MB"},
		{1024 * 1024, "1.0 GB"},
		{2_684_354, "2.6 GB"},
		{1024 * 1024 * 1024, "1.00 TB"},
	}
	for _, tt := range tests {
		if got := report.HumanizeKB(tt.kb); got != tt.want {
			t.Errorf("HumanizeKB(%d) = %q, want %q", tt.kb, got, tt.want)
		}
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./internal/report -run TestHumanizeKB -v`
Expected: FAIL — `undefined: report.HumanizeKB`.

- [ ] **Step 3: Implement**

```go
// HumanizeKB renders a size in kilobytes, escalating the unit so large values stay
// readable. It mirrors tui.HumanizeMB's steps (and its two decimals at TB) without
// importing it: internal/report has no internal dependency, and one formatter is not
// worth making the report package depend on the console's.
func HumanizeKB(kb int64) string {
	switch {
	case kb < 1024:
		return fmt.Sprintf("%d KB", kb)
	case kb < 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(kb)/1024)
	case kb < 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(kb)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f TB", float64(kb)/(1024*1024*1024))
	}
}
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/report -run TestHumanizeKB -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): humanize kilobyte sizes up to TB"
```

---

### Task 5: the manifest opt-in `allow_reenable_disabled_indexes`

**Files:**
- Modify: `internal/ddl/manifest.go:902-915` (`RebuildHeap`)
- Modify: `internal/ddl/manifest_test.go`, `internal/ddl/render_test.go`

**Interfaces:**
- Produces: `ddl.RebuildHeap.AllowReenableDisabledIndexes bool`
  (`yaml:"allow_reenable_disabled_indexes,omitempty"`).

- [ ] **Step 1: Write the failing tests**

In `internal/ddl/manifest_test.go`:

```go
// TestRebuildHeapAllowReenableDecodes: the key exists on rebuild_heap, and strict decoding
// still rejects it on an operation that has no such field.
func TestRebuildHeapAllowReenableDecodes(t *testing.T) {
	m, err := ddl.ParseManifest([]byte(`
operations:
  - operation: rebuild_heap
    schema: dbo
    table: MEASUREMENT
    allow_reenable_disabled_indexes: true
`))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}
	op, ok := m.Operations[0].(ddl.RebuildHeap)
	if !ok {
		t.Fatalf("operation = %T, want ddl.RebuildHeap", m.Operations[0])
	}
	if !op.AllowReenableDisabledIndexes {
		t.Error("AllowReenableDisabledIndexes = false, want true")
	}

	if _, err := ddl.ParseManifest([]byte(`
operations:
  - operation: reorganize_index
    schema: dbo
    table: MEASUREMENT
    index: IX_MEASUREMENT_TS
    allow_reenable_disabled_indexes: true
`)); err == nil {
		t.Error("ParseManifest() accepted the key on reorganize_index, want a strict-decoding error")
	}
}
```

In `internal/ddl/render_test.go`, extend the existing rebuild_heap rendering test so the key
round-trips, and assert it is absent when false:

```go
// TestRenderRebuildHeapOptIn: the key round-trips when set, and a heap that does not set it
// renders no key at all (omitempty) — a generated plan must not be noisy with false flags.
func TestRenderRebuildHeapOptIn(t *testing.T) {
	with, err := ddl.RenderManifest(&ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT", AllowReenableDisabledIndexes: true},
	}})
	if err != nil {
		t.Fatalf("RenderManifest() error = %v", err)
	}
	if !strings.Contains(string(with), "allow_reenable_disabled_indexes: true") {
		t.Errorf("rendered manifest missing the opt-in:\n%s", with)
	}
	without, err := ddl.RenderManifest(&ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT"},
	}})
	if err != nil {
		t.Fatalf("RenderManifest() error = %v", err)
	}
	if strings.Contains(string(without), "allow_reenable_disabled_indexes") {
		t.Errorf("rendered manifest carries the opt-in when unset:\n%s", without)
	}
}
```

Check the real name of the render entry point first (`render.go` top) and use it; the test above
assumes `RenderManifest`.

- [ ] **Step 2: Run the tests to make sure they fail**

Run: `go test ./internal/ddl -run 'AllowReenable|RebuildHeapOptIn' -v`
Expected: FAIL — unknown field `allow_reenable_disabled_indexes`.

- [ ] **Step 3: Implement**

```go
type RebuildHeap struct {
	Schema          string          `yaml:"schema"`
	Table           string          `yaml:"table"`
	DataCompression string          `yaml:"data_compression"`
	Options         OptionOverrides `yaml:"options"`
	// AllowReenableDisabledIndexes accepts the one irreversible side effect of a heap
	// rebuild: ALTER TABLE ... REBUILD rebuilds every nonclustered index of the table,
	// which re-enables a disabled one and rebuilds it uncompressed ("compression settings
	// metadata is lost when nonclustered indexes are disabled"). Verified on a server, see
	// docs/specs/OBJECT-SIZES-ANALYSIS.md. Preflight refuses such a rebuild unless this is
	// set; the maintenance planner never sets it.
	AllowReenableDisabledIndexes bool `yaml:"allow_reenable_disabled_indexes,omitempty"`
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/ddl`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ddl
git commit -m "feat(ddl): opt in to a heap rebuild that re-enables a disabled index"
```

---

### Task 6: the preflight guard and the scope record

**Files:**
- Modify: `internal/preflight/preflight.go` (two new checks + the loop from Task 3)
- Modify: `internal/preflight/preflight_test.go`

**Interfaces:**
- Consumes: Tasks 2, 3, 5.
- Produces: `CheckHeapRebuildScope(target string, rewritten []mssql.StructureSize) Check`,
  `CheckReenabledIndexes(target string, disabled []string, allowed bool, readErr error) Check`.

- [ ] **Step 1: Write the failing tests**

```go
// TestCheckReenabledIndexes pins the guard: a heap rebuild that would re-enable a disabled
// index fails until the operation opts in, and says what the operator loses.
func TestCheckReenabledIndexes(t *testing.T) {
	c := preflight.CheckReenabledIndexes("dbo.MEASUREMENT (heap)", []string{"IX_MEASUREMENT_OLD"}, false, nil)
	if c.Severity != preflight.Fail {
		t.Fatalf("Severity = %v, want Fail", c.Severity)
	}
	for _, want := range []string{"IX_MEASUREMENT_OLD", "without its compression", "allow_reenable_disabled_indexes"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("Detail = %q, want it to contain %q", c.Detail, want)
		}
	}
	if got := preflight.CheckReenabledIndexes("dbo.MEASUREMENT (heap)", []string{"IX_MEASUREMENT_OLD"}, true, nil); got.Severity != preflight.Warn {
		t.Errorf("with the opt-in: Severity = %v, want Warn", got.Severity)
	}
	if got := preflight.CheckReenabledIndexes("dbo.MEASUREMENT (heap)", nil, false, nil); got.Severity != preflight.Pass {
		t.Errorf("no disabled index: Severity = %v, want Pass", got.Severity)
	}
	if got := preflight.CheckReenabledIndexes("dbo.MEASUREMENT (heap)", nil, false, errors.New("permission denied")); got.Severity != preflight.Warn {
		t.Errorf("unreadable index state: Severity = %v, want Warn (never fail a run on a permission)", got.Severity)
	}
}

// TestCheckHeapRebuildScope records what else the statement rewrites, for the .log.
func TestCheckHeapRebuildScope(t *testing.T) {
	c := preflight.CheckHeapRebuildScope("dbo.MEASUREMENT (heap)", probeSizes)
	if c.Severity != preflight.Warn {
		t.Fatalf("Severity = %v, want Warn", c.Severity)
	}
	for _, want := range []string{"3 nonclustered index(es)", "IX_MEASUREMENT_TS", "one transaction"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("Detail = %q, want it to contain %q", c.Detail, want)
		}
	}
	if got := preflight.CheckHeapRebuildScope("dbo.T (heap)", []mssql.StructureSize{{IndexID: 0, UsedKB: 10}}); got.Severity != preflight.Pass {
		t.Errorf("heap with no index: Severity = %v, want Pass", got.Severity)
	}
}

// TestRunRefusesHeapRebuildWithDisabledIndex is the end-to-end gate.
func TestRunRefusesHeapRebuildWithDisabledIndex(t *testing.T) {
	p := fakeProber{structures: probeSizes, files: []mssql.FileSpace{{FreeMB: 100000}}}
	refused := &ddl.Manifest{Operations: []ddl.Operation{ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT"}}}
	rep, err := preflight.Run(context.Background(), p, spaceServerInfo, refused, preflight.Thresholds{}, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !rep.HasFailure() {
		t.Fatalf("preflight passed a heap rebuild that re-enables a disabled index:\n%+v", rep.Checks)
	}

	allowed := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT", AllowReenableDisabledIndexes: true},
	}}
	rep, err = preflight.Run(context.Background(), p, spaceServerInfo, allowed, preflight.Thresholds{}, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.HasFailure() {
		t.Errorf("preflight refused an opted-in heap rebuild:\n%+v", rep.Checks)
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./internal/preflight -run 'Reenabled|HeapRebuildScope|RefusesHeap' -v`
Expected: FAIL — `undefined: preflight.CheckReenabledIndexes`.

- [ ] **Step 3: Implement**

In `internal/preflight/sizes.go` (the checks belong with the selection they read):

```go
// CheckReenabledIndexes guards the one irreversible side effect of a heap rebuild. The
// rebuild recreates every nonclustered index of the table, so a disabled one comes back
// live and uncompressed (verified: docs/specs/OBJECT-SIZES-ANALYSIS.md). That undoes a
// deliberate operator decision, so it fails unless the operation opted in. A read error
// warns rather than fails: a login missing VIEW DEFINITION must not be blocked by a guard
// that cannot run.
func CheckReenabledIndexes(target string, disabled []string, allowed bool, readErr error) Check {
	const name = "heap rebuild re-enables indexes"
	switch {
	case readErr != nil:
		return Check{name, Warn, fmt.Sprintf("%s: index state could not be read (%v); a disabled index would be re-enabled by the rebuild", target, readErr)}
	case len(disabled) == 0:
		return Check{name, Pass, target + ": no disabled index on the table"}
	case allowed:
		return Check{name, Warn, fmt.Sprintf(
			"%s: rebuild re-enables %s and rebuilds it without its compression — allowed by allow_reenable_disabled_indexes",
			target, strings.Join(disabled, ", "))}
	default:
		return Check{name, Fail, fmt.Sprintf(
			"%s: ALTER TABLE REBUILD re-enables disabled index(es) %s and rebuilds them without their compression (compression metadata is dropped when an index is disabled); drop the index, or set allow_reenable_disabled_indexes: true on this operation",
			target, strings.Join(disabled, ", "))}
	}
}

// CheckHeapRebuildScope records, in the .log, what else a heap rebuild rewrites. It is a
// record, not the warning: only FAIL lines reach the console, so the operator is told
// before the fact by the engine's manifest-start line and the dry run.
func CheckHeapRebuildScope(target string, rewritten []mssql.StructureSize) Check {
	const name = "heap rebuild scope"
	var (
		parts []string
		count int
	)
	for _, s := range rewritten {
		if s.IndexID == 0 {
			continue
		}
		count++
		parts = append(parts, fmt.Sprintf("%s %s", s.Name, report.HumanizeKB(s.UsedKB)))
	}
	if count == 0 {
		return Check{name, Pass, target + ": no nonclustered index; the rebuild rewrites the heap alone"}
	}
	return Check{name, Warn, fmt.Sprintf("%s: ALTER TABLE REBUILD also rebuilds %d nonclustered index(es): %s; %s rewritten in one transaction",
		target, count, strings.Join(parts, ", "), report.HumanizeKB(SumKB(rewritten)))}
}
```

`internal/preflight` importing `internal/report` for `HumanizeKB` is a new edge. Check first
whether `report` imports `preflight` (it does not today); if that ever reverses, move `HumanizeKB`
to its own tiny package rather than duplicating it.

In `Run`'s operation loop, next to the space check from Task 3 and **outside**
`if th.RequireDataFreeSpace` (the guard is not a space check and must run whatever that setting
says):

```go
		if heap, ok := op.(ddl.RebuildHeap); ok {
			sizes, err := p.TableStructureSizes(ctx, heap.Schema, heap.Table, nil)
			label := rewrittenLabel(op)
			rep.add(CheckReenabledIndexes(label, DisabledNames(sizes), heap.AllowReenableDisabledIndexes, err))
			if err == nil {
				rep.add(CheckHeapRebuildScope(label, sizes))
			}
		}
```

This reads the sizes a second time for a heap when the space check already read them. Keep one read
per operation: hoist the read above both checks and pass `sizes` into each.

- [ ] **Step 4: Run the package tests**

Run: `go test -race ./internal/preflight`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/preflight
git commit -m "feat(preflight): refuse a heap rebuild that silently re-enables a disabled index"
```

---

### Task 7: the report carries sizes

**Files:**
- Modify: `internal/report/report.go:68-113` (types), `:115-183` (`Write`)
- Modify: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `HumanizeKB` (Task 4).
- Produces: `report.SizeLine{Name, Type string; WasDisabled bool; BeforeKB, AfterKB int64}`,
  `OperationReport.Sizes []SizeLine`, `OperationReport.SizesPartial bool`,
  `RunReport.HeapScopeNotices []string`, `RunReport.SizeBeforeKB/SizeAfterKB int64`,
  `RunReport.SizeStructures int`, `RunReport.SizesUnread string`.
  `SizeUnknown = int64(-1)` marks a side that was not measured.

- [ ] **Step 1: Write the failing test**

```go
// TestWriteRendersSizes covers the three shapes: one structure, a heap block with a total,
// and the states that have no percentage (unknown, a re-enabled index that had no pages).
func TestWriteRendersSizes(t *testing.T) {
	r := report.RunReport{
		Manifest: "100_h.yaml", Outcome: "SUCCESS",
		HeapScopeNotices: []string{"operation 1 rebuild_heap dbo.MEASUREMENT also rebuilds 2 nonclustered index(es)"},
		Operations: []report.OperationReport{
			{Index: 1, CommandType: "rebuild_index", Target: "dbo.MEASUREMENT.IX_TS", Outcome: "success",
				Sizes: []report.SizeLine{{Name: "IX_TS", Type: "NONCLUSTERED", BeforeKB: 2_097_152, AfterKB: 1_468_006}}},
			{Index: 2, CommandType: "rebuild_heap", Target: "dbo.MEASUREMENT", Outcome: "success",
				Sizes: []report.SizeLine{
					{Name: "heap", Type: "HEAP", BeforeKB: 5_242_880, AfterKB: 3_250_586},
					{Name: "IX_OLD", Type: "NONCLUSTERED", WasDisabled: true, BeforeKB: 0, AfterKB: 462_848},
					{Name: "IX_TS", Type: "NONCLUSTERED", BeforeKB: 2_097_152, AfterKB: report.SizeUnknown},
				}},
		},
		SizeBeforeKB: 7_340_032, SizeAfterKB: 4_718_592, SizeStructures: 2,
	}
	var b strings.Builder
	if err := report.Write(&b, r); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"also rebuilds 2 nonclustered index(es)",
		"size: IX_TS 2.0 GB -> 1.4 GB (-30.0%)",
		"size (heap and 2 nonclustered index(es)):",
		"IX_OLD (was disabled)",
		"-> unknown",
		"total",
		"size: 7.0 GB -> 4.5 GB (-35.7%) over 2 structure(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "IX_OLD (was disabled)   0 KB -> 452.0 MB (") {
		t.Error("a structure with no pages before must not get a percentage")
	}
}

// TestWriteSizesUnread: when nothing could be measured, the manifest says so once instead
// of printing "unknown -> unknown" under every operation.
func TestWriteSizesUnread(t *testing.T) {
	var b strings.Builder
	err := report.Write(&b, report.RunReport{
		Manifest: "100_h.yaml", Outcome: "SUCCESS",
		SizesUnread: "structure sizes dbo.MEASUREMENT: permission denied",
		Operations:  []report.OperationReport{{Index: 1, CommandType: "rebuild_index", Outcome: "success"}},
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !strings.Contains(b.String(), "sizes not measured: structure sizes dbo.MEASUREMENT: permission denied") {
		t.Errorf("report missing the manifest-level unread line:\n%s", b.String())
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./internal/report -run 'RendersSizes|SizesUnread' -v`
Expected: FAIL — `undefined: report.SizeLine`.

- [ ] **Step 3: Implement**

Types:

```go
// SizeUnknown marks a side of a size line that was not measured: the read failed, or the
// operation did not reach the point where that side is meaningful.
const SizeUnknown int64 = -1

// SizeLine is one structure's used size before and after an operation. Name is "heap" for
// the heap itself. WasDisabled marks an index the operation re-enabled: it had no pages
// before, so its growth is real and its "before" is not a shrinkable figure.
type SizeLine struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	WasDisabled bool   `json:"was_disabled,omitempty"`
	BeforeKB    int64  `json:"before_kb"`
	AfterKB     int64  `json:"after_kb"`
}
```

`OperationReport` gains `Sizes []SizeLine json:"sizes,omitempty"` and
`SizesPartial bool json:"sizes_partial,omitempty"`; `RunReport` gains
`HeapScopeNotices []string json:"heap_scope_notices,omitempty"`,
`SizeBeforeKB`/`SizeAfterKB int64 json:"size_before_kb,omitempty"`,
`SizeStructures int json:"size_structures,omitempty"` and
`SizesUnread string json:"sizes_unread,omitempty"`.

Rendering, in `Write`: after the `CancelOnlyNotice` block, print each `HeapScopeNotices` entry on
its own line. Inside the operations loop, after the `Shrink` block:

```go
			renderSizes(w, op)
```

and after the loop, before `CancelOnlySummary`:

```go
	if r.SizesUnread != "" {
		fmt.Fprintf(w, "\nsizes not measured: %s\n", r.SizesUnread)
	}
	if r.SizeStructures > 0 {
		fmt.Fprintf(w, "\nsize: %s\n", sizeChange(r.SizeBeforeKB, r.SizeAfterKB)+
			fmt.Sprintf(" over %d structure(s)", r.SizeStructures))
	}
```

with:

```go
// sizeChange renders "before -> after (-p%)", dropping the percentage when either side is
// unknown or the before size is zero (a re-enabled index grew from nothing; there is no
// percentage to state).
func sizeChange(beforeKB, afterKB int64) string {
	before, after := "unknown", "unknown"
	if beforeKB != SizeUnknown {
		before = HumanizeKB(beforeKB)
	}
	if afterKB != SizeUnknown {
		after = HumanizeKB(afterKB)
	}
	out := before + " -> " + after
	if beforeKB > 0 && afterKB != SizeUnknown {
		out += fmt.Sprintf(" (%+.1f%%)", (float64(afterKB)-float64(beforeKB))/float64(beforeKB)*100)
	}
	return out
}

// renderSizes prints an operation's size lines: one line for a single structure, a block
// with a total when the operation rewrote several (a heap rebuild).
func renderSizes(w io.Writer, op OperationReport) {
	partial := ""
	if op.SizesPartial {
		partial = ", partial"
	}
	switch len(op.Sizes) {
	case 0:
		return
	case 1:
		s := op.Sizes[0]
		fmt.Fprintf(w, "      size: %s %s%s\n", sizeName(s), sizeChange(s.BeforeKB, s.AfterKB), partial)
		return
	}
	fmt.Fprintf(w, "      size (heap and %d nonclustered index(es))%s:\n", len(op.Sizes)-1, partial)
	var totalBefore, totalAfter int64
	for _, s := range op.Sizes {
		fmt.Fprintf(w, "        %-34s %s\n", sizeName(s), sizeChange(s.BeforeKB, s.AfterKB))
		if s.BeforeKB != SizeUnknown {
			totalBefore += s.BeforeKB
		}
		if s.AfterKB != SizeUnknown {
			totalAfter += s.AfterKB
		}
	}
	fmt.Fprintf(w, "        %-34s %s\n", "total", sizeChange(totalBefore, totalAfter))
}

func sizeName(s SizeLine) string {
	if s.WasDisabled {
		return s.Name + " (was disabled)"
	}
	return s.Name
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/report`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/report
git commit -m "feat(report): record and render object sizes before and after"
```

---

### Task 8: the campaign figure in the SQLite history

**Files:**
- Modify: `internal/report/history.go:13-23` (`RunRecord`), `:68-75` (`columnMigrations`),
  `:117-125` (`Record`)
- Modify: `internal/report/history_test.go`

**Interfaces:**
- Produces: `RunRecord.SizeBeforeKB`, `RunRecord.SizeAfterKB` (`int64`), columns
  `runs.size_before_kb`, `runs.size_after_kb`.

- [ ] **Step 1: Write the failing test**

```go
// TestRecordStoresSizes: the two columns are added to an existing database by the additive
// migration and carry the manifest totals, so a campaign is one SUM over its runs.
func TestRecordStoresSizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	h, err := report.OpenHistory(path)
	if err != nil {
		t.Fatalf("OpenHistory() error = %v", err)
	}
	if err := h.Record(context.Background(), report.RunRecord{
		Manifest: "100_h.yaml", Outcome: "SUCCESS", SizeBeforeKB: 7_340_032, SizeAfterKB: 4_718_592,
	}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var before, after int64
	if err := db.QueryRow(`SELECT size_before_kb, size_after_kb FROM runs`).Scan(&before, &after); err != nil {
		t.Fatalf("query: %v", err)
	}
	if before != 7_340_032 || after != 4_718_592 {
		t.Errorf("stored (%d, %d), want (7340032, 4718592)", before, after)
	}
}
```

Use the same SQLite driver import the other history tests use (check the top of
`internal/report/history_test.go`), not a new one.

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./internal/report -run TestRecordStoresSizes -v`
Expected: FAIL — `unknown field SizeBeforeKB` / `no such column`.

- [ ] **Step 3: Implement**

```go
	SizeBeforeKB int64 // used size of every structure this run measured on both sides, before
	SizeAfterKB  int64 // ... and after; 0 when nothing was fully measured
```

```go
	{"size_before_kb", `ALTER TABLE runs ADD COLUMN size_before_kb INTEGER;`},
	{"size_after_kb", `ALTER TABLE runs ADD COLUMN size_after_kb INTEGER;`},
```

and extend `Record`'s `INSERT` with the two columns and values.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/report`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/report
git commit -m "feat(report): store per-run size totals in the history"
```

---

### Task 9: the engine measures each operation

**Files:**
- Create: `internal/run/sizes.go`
- Create: `internal/run/sizes_test.go`
- Modify: `internal/run/engine.go:200-215` (fields), `:300-330` (options), `:874-1000` (`runStep`),
  the `manifestRun` struct near `:731-750`, and `finalize*` where `rep` totals are set

**Interfaces:**
- Consumes: `mssql.StructureSize` (1), `preflight.Rewritten/SumKB/SizedOperation` (2),
  `report.SizeLine`/`SizeUnknown` (7).
- Produces: `run.SizeReader` interface, `run.WithSizeReader(SizeReader) EngineOption`,
  `(*sizeTotals).add(schema, table string, lines []report.SizeLine)`,
  `(*sizeTotals).totals() (beforeKB, afterKB int64, structures int)`.

- [ ] **Step 1: Write the failing tests**

`internal/run/sizes_test.go`:

```go
package run

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// TestSizeTotalsCountsEachStructureOnce: a hand-written manifest can rebuild a heap and
// then one of its indexes. The index must count once — its first "before" and its last
// "after" — or the manifest total double-counts it.
func TestSizeTotalsCountsEachStructureOnce(t *testing.T) {
	var tot sizeTotals
	tot.add("dbo", "MEASUREMENT", []report.SizeLine{
		{Name: "heap", BeforeKB: 5000, AfterKB: 3000},
		{Name: "IX_TS", BeforeKB: 2000, AfterKB: 1500},
	})
	tot.add("dbo", "MEASUREMENT", []report.SizeLine{
		{Name: "IX_TS", BeforeKB: 1500, AfterKB: 1000},
	})

	before, after, structures := tot.totals()
	if structures != 2 {
		t.Fatalf("structures = %d, want 2 (heap + IX_TS)", structures)
	}
	if before != 7000 || after != 4000 {
		t.Errorf("totals = (%d, %d), want (7000, 4000): first before, last after", before, after)
	}
}

// TestSizeTotalsSkipsUnmeasuredSides: a structure missing either side contributes nothing,
// so the manifest figure is never a sum of halves.
func TestSizeTotalsSkipsUnmeasuredSides(t *testing.T) {
	var st sizeTotals
	st.add("dbo", "T", []report.SizeLine{{Name: "IX", BeforeKB: 100, AfterKB: report.SizeUnknown}})
	before, after, structures := st.totals()
	if structures != 0 || before != 0 || after != 0 {
		t.Errorf("totals = (%d, %d, %d), want zeros", before, after, structures)
	}
}
```

Add to `internal/run/engine_test.go` (it already has `setupEngine`, `writeOnly`, `heapManifest`,
`seqOpRunner`):

```go
// fakeSizeReader answers with a fixed table shape, and can fail on demand.
type fakeSizeReader struct {
	sizes []mssql.StructureSize
	err   error
	calls int
}

func (f *fakeSizeReader) TableStructureSizes(_ context.Context, _, _ string, _ *int) ([]mssql.StructureSize, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	// Shrink on every call after the first, so before != after.
	out := make([]mssql.StructureSize, len(f.sizes))
	copy(out, f.sizes)
	if f.calls > 1 {
		for i := range out {
			out[i].UsedKB = out[i].UsedKB / 2
		}
	}
	return out, nil
}

// TestSizesRecordedForSuccessfulOperation: both sides measured, one line per structure.
func TestSizesRecordedForSuccessfulOperation(t *testing.T) {
	sizes := &fakeSizeReader{sizes: []mssql.StructureSize{
		{IndexID: 0, TypeDesc: "HEAP", UsedKB: 4000},
		{IndexID: 2, Name: "IX_A", TypeDesc: "NONCLUSTERED", UsedKB: 2000},
	}}
	eng, dirs := setupEngine(t, fakePreflighter{}, &seqOpRunner{}, run.WithSizeReader(sizes))
	writeOnly(t, dirs, "100_h.yaml", heapManifest)

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	log := readLog(t, filepath.Join(dirs.Done, "100_h.yaml.log"))
	if !strings.Contains(log, "size (heap and 1 nonclustered index(es))") {
		t.Errorf("log missing the heap size block:\n%s", log)
	}
	if !strings.Contains(log, "over 2 structure(s)") {
		t.Errorf("log missing the manifest size total:\n%s", log)
	}
}

// TestNoAfterSizeOnFailedRebuild: a rebuild that failed rolled back, so there is no
// meaningful "after" and none is recorded.
func TestNoAfterSizeOnFailedRebuild(t *testing.T) {
	sizes := &fakeSizeReader{sizes: []mssql.StructureSize{{IndexID: 0, TypeDesc: "HEAP", UsedKB: 4000}}}
	runner := &seqOpRunner{errs: []error{os.ErrDeadlineExceeded, nil}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithSizeReader(sizes))
	writeOnly(t, dirs, "100_h.yaml", heapManifest)

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	log := readLog(t, filepath.Join(dirs.Failed, "100_h.yaml.log"))
	if !strings.Contains(log, "-> unknown") {
		t.Errorf("failed operation should record an unknown after size:\n%s", log)
	}
}

// TestSizeReaderErrorDoesNotFailTheRun, and the manifest says once why nothing was measured.
func TestSizeReaderErrorDoesNotFailTheRun(t *testing.T) {
	sizes := &fakeSizeReader{err: errors.New("permission denied")}
	eng, dirs := setupEngine(t, fakePreflighter{}, &seqOpRunner{}, run.WithSizeReader(sizes))
	writeOnly(t, dirs, "100_h.yaml", heapManifest)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 0 {
		t.Fatalf("Summary = %+v, want no failure from a size read", sum)
	}
	log := readLog(t, filepath.Join(dirs.Done, "100_h.yaml.log"))
	if !strings.Contains(log, "sizes not measured: permission denied") {
		t.Errorf("log missing the manifest-level unread line:\n%s", log)
	}
	if strings.Contains(log, "size: ") {
		t.Errorf("log printed a per-operation size line with nothing measured:\n%s", log)
	}
}

// TestNoSizeReaderMeansNoSizeLines guards the nil case (every existing test builds an
// engine without one).
func TestNoSizeReaderMeansNoSizeLines(t *testing.T) {
	eng, dirs := setupEngine(t, fakePreflighter{}, &seqOpRunner{})
	writeOnly(t, dirs, "100_h.yaml", heapManifest)
	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if log := readLog(t, filepath.Join(dirs.Done, "100_h.yaml.log")); strings.Contains(log, "size") {
		t.Errorf("engine with no size reader wrote size output:\n%s", log)
	}
}
```

`readLog` is a two-line helper (`os.ReadFile` + `t.Fatalf`); add it next to the existing helpers if
the file has none.

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./internal/run -run 'SizeTotals|Sizes|SizeReader' -v`
Expected: FAIL — `undefined: run.WithSizeReader`.

- [ ] **Step 3: Implement**

`internal/run/sizes.go`:

```go
package run

import (
	"context"
	"fmt"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// SizeReader reads the used size of every structure of a table. *mssql.Conn satisfies it.
type SizeReader interface {
	TableStructureSizes(ctx context.Context, schema, table string, partition *int) ([]mssql.StructureSize, error)
}

// readSizes returns the structures op rewrites, or nil with the error when the read
// failed. A nil reader (tests, and any engine built without one) reads as "not measured".
func readSizes(ctx context.Context, r SizeReader, op ddl.Operation) ([]mssql.StructureSize, error) {
	if r == nil {
		return nil, nil
	}
	schema, table, partition, ok := preflight.SizedOperation(op)
	if !ok {
		return nil, nil
	}
	sizes, err := r.TableStructureSizes(ctx, schema, table, partition)
	if err != nil {
		return nil, err
	}
	return preflight.Rewritten(op, sizes), nil
}

// sizeLines pairs a before and an after read into the report's lines. A structure present
// on one side only still gets a line, with SizeUnknown on the missing side; a structure
// that had no pages before (a disabled index the rebuild re-enabled) is marked.
func sizeLines(before, after []mssql.StructureSize) []report.SizeLine {
	if len(before) == 0 && len(after) == 0 {
		return nil
	}
	byID := map[int]report.SizeLine{}
	order := []int{}
	for _, s := range before {
		byID[s.IndexID] = report.SizeLine{
			Name: structureName(s), Type: s.TypeDesc, WasDisabled: s.Disabled,
			BeforeKB: s.UsedKB, AfterKB: report.SizeUnknown,
		}
		order = append(order, s.IndexID)
	}
	for _, s := range after {
		line, seen := byID[s.IndexID]
		if !seen {
			line = report.SizeLine{Name: structureName(s), Type: s.TypeDesc, BeforeKB: report.SizeUnknown}
			order = append(order, s.IndexID)
		}
		line.AfterKB = s.UsedKB
		byID[s.IndexID] = line
	}
	out := make([]report.SizeLine, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

func structureName(s mssql.StructureSize) string {
	if s.IndexID == 0 {
		return "heap"
	}
	return s.Name
}

// sizeTotals accumulates the manifest figure per distinct structure: the first measured
// "before" and the last measured "after". A manifest that rebuilds a heap and then one of
// its indexes touches that index twice, and counting it twice would inflate the total.
type sizeTotals struct {
	first map[string]int64
	last  map[string]int64
	order []string
}

func (t *sizeTotals) add(schema, table string, lines []report.SizeLine) {
	for _, l := range lines {
		if l.BeforeKB == report.SizeUnknown || l.AfterKB == report.SizeUnknown {
			continue
		}
		key := fmt.Sprintf("%s.%s.%s", schema, table, l.Name)
		if t.first == nil {
			t.first, t.last = map[string]int64{}, map[string]int64{}
		}
		if _, seen := t.first[key]; !seen {
			t.first[key] = l.BeforeKB
			t.order = append(t.order, key)
		}
		t.last[key] = l.AfterKB
	}
}

func (t *sizeTotals) totals() (beforeKB, afterKB int64, structures int) {
	for _, key := range t.order {
		beforeKB += t.first[key]
		afterKB += t.last[key]
		structures++
	}
	return beforeKB, afterKB, len(t.order)
}
```

In `engine.go`:

- add the field `sizes SizeReader` and the option
  `func WithSizeReader(r SizeReader) EngineOption { return func(e *Engine) { e.sizes = r } }`
  (document it the way `WithLogWatch` is documented);
- add `sizeTotals sizeTotals` and `sizesUnread string` to `manifestRun`;
- in `runStep`, **after** the resumable `switch` that sets `stmt` and before the operation runs:

```go
	// Size before: for a RESUME too. A paused resumable's partial target lives under
	// internal index ids that neither sys.indexes nor sys.dm_db_partition_stats shows, so
	// this read returns the source index — the true old size (docs/specs/OBJECT-SIZES-ANALYSIS.md).
	sizesBefore, sizeErr := readSizes(ctx, e.sizes, step.Operation)
```

- after the operation, next to `waitLines, waitTotal := e.operationWaits(...)`:

```go
	// Size after: only when the operation succeeded, except for a reorganize, which
	// commits incrementally and keeps its work when canceled ("committed work preserved,
	// no rollback") — its partial compaction is real and worth reporting.
	_, isReorg := step.Operation.(ddl.ReorganizeIndex)
	var sizesAfter []mssql.StructureSize
	if runErr == nil || isReorg {
		var err error
		sizesAfter, err = readSizes(ctx, e.sizes, step.Operation)
		if err != nil && sizeErr == nil {
			sizeErr = err
		}
	}
	if sizeErr != nil && r.sizesUnread == "" {
		r.sizesUnread = sizeErr.Error()
	}
```

- fill `opRep.Sizes = sizeLines(sizesBefore, sizesAfter)` and
  `opRep.SizesPartial = isReorg && runErr != nil` in the `report.OperationReport` literal, and feed
  the totals right after it:

```go
	if ref := step.Operation.Target(); len(opRep.Sizes) > 0 {
		r.sizeTotals.add(ref.Schema, ref.Table, opRep.Sizes)
	}
```

- where the run report is finalized (every path that writes `r.rep`), set:

```go
	r.rep.SizeBeforeKB, r.rep.SizeAfterKB, r.rep.SizeStructures = r.sizeTotals.totals()
	r.rep.SizesUnread = r.sizesUnread
```

Put that in one helper called from `finalizeAll`/`finalizePartial`/`finalizeDrained` rather than
three copies, and carry the same two totals into the `report.RunRecord` the history writes.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/run ./internal/report`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/run internal/report
git commit -m "feat(run): measure each rebuild and reorganize before and after"
```

---

### Task 10: manifest-start scope, and the per-operation detail

**Files:**
- Modify: `internal/run/engine.go:614-641` (op list + notices)
- Modify: `internal/run/step.go:45-52` (`OpInfo`)
- Modify: `internal/run/engine_test.go`

**Interfaces:**
- Consumes: Task 9's `SizeReader`, Task 7's `RunReport.HeapScopeNotices`.
- Produces: `run.OpInfo.Detail string`; `heapScopeNotice(index int, op ddl.RebuildHeap, sizes []mssql.StructureSize) string`;
  `opDetail(step ddl.PlannedOperation, maxRetries int, scope string) string`.

- [ ] **Step 1: Write the failing test**

```go
// TestManifestStartHeapScope: before anything runs, the log, the report and the operation
// list say the heap rebuild rewrites more than the manifest names.
func TestManifestStartHeapScope(t *testing.T) {
	sizes := &fakeSizeReader{sizes: []mssql.StructureSize{
		{IndexID: 0, TypeDesc: "HEAP", UsedKB: 5 * 1024 * 1024},
		{IndexID: 2, Name: "IX_A", TypeDesc: "NONCLUSTERED", UsedKB: 2 * 1024 * 1024},
	}}
	var ops [][]run.OpInfo
	eng, dirs := setupEngine(t, fakePreflighter{}, &seqOpRunner{},
		run.WithSizeReader(sizes), run.WithOpListSink(func(l []run.OpInfo) { ops = append(ops, l) }))
	writeOnly(t, dirs, "100_h.yaml", heapManifest)

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	log := readLog(t, filepath.Join(dirs.Done, "100_h.yaml.log"))
	if !strings.Contains(log, "also rebuilds 1 nonclustered index(es) (IX_A): 7.0 GB rewritten in one transaction") {
		t.Errorf("log missing the manifest-start heap scope line:\n%s", log)
	}
	if len(ops) != 1 {
		t.Fatalf("op list emitted %d times, want 1", len(ops))
	}
	if !strings.Contains(ops[0][0].Detail, "+1 nonclustered, 7.0 GB rewritten") {
		t.Errorf("OpInfo.Detail = %q, want the heap scope", ops[0][0].Detail)
	}
	if !strings.Contains(ops[0][0].Detail, "cancel only") {
		t.Errorf("OpInfo.Detail = %q, want the rollback-on-cancel marker too (rebuild_heap has no RESUMABLE form)", ops[0][0].Detail)
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./internal/run -run TestManifestStartHeapScope -v`
Expected: FAIL — `ops[0][0].Detail undefined`.

- [ ] **Step 3: Implement**

`OpInfo` gains:

```go
	Detail  string // manifest-start note for the console row: "cancel only", the heap rewrite scope, or both
```

In `processOne`, replace the op-list block with one that reads the heap scope first (the read is
skipped entirely when no operation is a `rebuild_heap`, or when no size reader is wired):

```go
	// A heap rebuild rewrites every nonclustered index of its table, in one transaction,
	// and the manifest names only the table. Say so before anything runs — the .log is
	// read after the damage (H2 class, REVIEW-2026-09-15-harm.md).
	scopes := map[int]string{}
	for i := resumeFrom; i < len(planned); i++ {
		heap, ok := planned[i].Operation.(ddl.RebuildHeap)
		if !ok {
			continue
		}
		sizes, err := readSizes(ctx, e.sizes, heap)
		if err != nil || len(sizes) < 2 {
			continue
		}
		notice := heapScopeNotice(i+1, heap, sizes)
		fmt.Fprintln(e.out, notice)
		rep.HeapScopeNotices = append(rep.HeapScopeNotices, notice)
		scopes[i] = heapScopeDetail(sizes)
	}

	if e.opListSink != nil {
		ops := make([]OpInfo, len(planned))
		for i, step := range planned {
			ops[i] = OpInfo{
				Index: i + 1, Command: step.Operation.CommandType(), Target: opTarget(step.Operation),
				Detail: opDetail(step, scopes[i]),
			}
		}
		e.emitOpList(ops)
	}
```

and in `sizes.go`:

```go
// heapScopeNotice is the manifest-start line for one heap rebuild: what else it rewrites,
// and how much, in one transaction.
func heapScopeNotice(index int, op ddl.RebuildHeap, sizes []mssql.StructureSize) string {
	names, count := nonclusteredNames(sizes)
	notice := fmt.Sprintf("operation %d rebuild_heap %s.%s also rebuilds %d nonclustered index(es) (%s): %s rewritten in one transaction",
		index, op.Schema, op.Table, count, strings.Join(names, ", "), report.HumanizeKB(preflight.SumKB(sizes)))
	if disabled := preflight.DisabledNames(sizes); len(disabled) > 0 {
		notice += fmt.Sprintf("; it re-enables disabled index(es) %s, rebuilt without their compression", strings.Join(disabled, ", "))
	}
	return notice
}

// heapScopeDetail is the same fact, short enough for a console row.
func heapScopeDetail(sizes []mssql.StructureSize) string {
	_, count := nonclusteredNames(sizes)
	detail := fmt.Sprintf("+%d nonclustered, %s rewritten", count, report.HumanizeKB(preflight.SumKB(sizes)))
	if n := len(preflight.DisabledNames(sizes)); n > 0 {
		detail += fmt.Sprintf(", re-enables %d disabled", n)
	}
	return detail
}

// opDetail joins the manifest-start notes for one operation's console row.
func opDetail(step ddl.PlannedOperation, scope string) string {
	var parts []string
	if RollbackOnCancel(step) {
		parts = append(parts, "cancel only")
	}
	if scope != "" {
		parts = append(parts, scope)
	}
	return strings.Join(parts, " · ")
}
```

`nonclusteredNames` returns the names and count of every structure with `IndexID != 0`.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/run`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/run
git commit -m "feat(run): name a heap rebuild's real scope before it runs"
```

---

### Task 11: `DisabledIndexes` for the planner

**Files:**
- Modify: `internal/mssql/indexes.go` (add after `TableStructureSizes`)
- Modify: `internal/mssql/indexes_integration_test.go`

**Interfaces:**
- Produces: `func (c *Conn) DisabledIndexes(ctx context.Context, objectID int64) ([]string, error)`.

- [ ] **Step 1: Write the failing integration test**

```go
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

	var objectID int64
	if err := conn.QueryRowContext(ctx, "SELECT OBJECT_ID(N'dbo."+table+"')").Scan(&objectID); err != nil {
		t.Fatalf("object id: %v", err)
	}
	got, err := conn.DisabledIndexes(ctx, objectID)
	if err != nil {
		t.Fatalf("DisabledIndexes: %v", err)
	}
	if len(got) != 1 || got[0] != "IX_"+table+"_off" {
		t.Errorf("DisabledIndexes() = %v, want one disabled index", got)
	}
}
```

Use whatever raw-query helper the file already has instead of `conn.QueryRowContext` if `Conn` does
not expose it.

- [ ] **Step 2: Run it to make sure it fails**

Run: `go vet -tags integration ./internal/mssql`
Expected: FAIL — `conn.DisabledIndexes undefined`.

- [ ] **Step 3: Implement**

```go
const disabledIndexesSQL = `
SELECT i.name
FROM sys.indexes i
WHERE i.object_id = @object_id AND i.is_disabled = 1 AND i.name IS NOT NULL
ORDER BY i.index_id;`

// DisabledIndexes lists the disabled indexes of one object. The maintenance planner needs
// it because its inventory reads sys.dm_db_partition_stats, where a disabled index has no
// row: disabling a nonclustered index physically deletes its data.
func (c *Conn) DisabledIndexes(ctx context.Context, objectID int64) ([]string, error) {
	rows, err := c.pool.QueryContext(ctx, disabledIndexesSQL, sql.Named("object_id", objectID))
	if err != nil {
		return nil, fmt.Errorf("disabled indexes for object %d: %w", objectID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan disabled index row: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Verify**

Run: `go build ./... && go vet -tags integration ./internal/mssql`
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add internal/mssql
git commit -m "feat(mssql): list the disabled indexes of an object"
```

---

### Task 12: the console keeps the detail on the operation's row

**Files:**
- Modify: `internal/tui/model.go:86-90` (`OperationRow`), the `StepDoneMsg` type and its `Update`
  case at `:550-551`
- Modify: `internal/tui/view.go:218-236` (`opRow`)
- Modify: `internal/tui/model_test.go`

**Interfaces:**
- Produces: `tui.OperationRow.Detail`, `tui.StepDoneMsg.Detail`, and `setOpDetail`.

- [ ] **Step 1: Write the failing test**

```go
// TestOperationRowKeepsItsDetail: the row is the only per-operation line the console keeps
// for the whole run. A LogMsg — a kill, an ignore answer — must not disturb it, which is
// exactly how the 0.34.0 rollback-on-cancel notice got erased (single notice slot).
func TestOperationRowKeepsItsDetail(t *testing.T) {
	m := tui.New(nil)
	m, _ = update(m, tui.OperationsMsg{Ops: []tui.OperationRow{
		{Index: 1, Label: "rebuild_heap dbo.MEASUREMENT", Status: "TO RUN", Detail: "cancel only · +2 nonclustered, 8.1 GB rewritten"},
	}})
	m, _ = update(m, tui.LogMsg{Line: "killed blocker SPID 53"})
	if v := m.View(); !strings.Contains(v, "cancel only · +2 nonclustered, 8.1 GB rewritten") {
		t.Errorf("row detail lost after a LogMsg:\n%s", v)
	}

	m, _ = update(m, tui.StepDoneMsg{Index: 1, Outcome: "success", Detail: "8.1 GB -> 5.4 GB (-33.3%)"})
	v := m.View()
	if !strings.Contains(v, "8.1 GB -> 5.4 GB (-33.3%)") {
		t.Errorf("finished row missing the size result:\n%s", v)
	}
	if strings.Contains(v, "cancel only") {
		t.Errorf("finished row still shows the start detail:\n%s", v)
	}
}
```

`update` is the existing test helper that calls `Model.Update` and re-asserts the concrete type; use
the file's own helper name.

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./internal/tui -run TestOperationRowKeepsItsDetail -v`
Expected: FAIL — unknown field `Detail`.

- [ ] **Step 3: Implement**

- `OperationRow` gains `Detail string // manifest-start note, replaced by the size result when the operation finishes`.
- `StepDoneMsg` gains `Detail string`.
- The `StepDoneMsg` case sets the status as today and, when `msg.Detail != ""`, replaces that row's
  `Detail`.
- `opRow` appends `"   " + o.Detail` when non-empty, after the status and before the SPID/elapsed
  suffix, styled with `helpStyle` so it reads as secondary.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/tui`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat(tui): keep each operation's note on its own row"
```

---

### Task 13: wiring — engine option, forwarder, dry run

**Files:**
- Modify: `cmd/sqlgopace/main.go:637-666` (`opts`), `:1187-1204` (`step`, `ops`), `:1489-1505`
  (`dryRunManifest`), `:1532-1546` (`renderPlan`)
- Modify: `cmd/sqlgopace/main_test.go`

**Interfaces:**
- Consumes: Tasks 9, 10, 12.
- Produces: `heapScopeLines(sizes []mssql.StructureSize, allowed bool) []string` in `main.go`, used by
  `renderPlan`; `renderPlan` gains a `scopes map[int][]string` parameter.

- [ ] **Step 1: Write the failing tests**

```go
// TestDryRunHeapScopeLines: connected, the dry run lists what else the heap rebuild
// rewrites and warns about a disabled index; offline it says it cannot list them.
func TestDryRunHeapScopeLines(t *testing.T) {
	sizes := []mssql.StructureSize{
		{IndexID: 0, TypeDesc: "HEAP", UsedKB: 5 * 1024 * 1024},
		{IndexID: 2, Name: "IX_A", TypeDesc: "NONCLUSTERED", UsedKB: 2 * 1024 * 1024},
		{IndexID: 3, Name: "IX_OLD", TypeDesc: "NONCLUSTERED", Disabled: true},
	}
	got := strings.Join(heapScopeLines(sizes, false), "\n")
	for _, want := range []string{
		"also rebuilds 2 nonclustered index(es): IX_A 2.0 GB, IX_OLD 0 KB",
		"7.0 GB rewritten",
		"re-enables disabled index IX_OLD without its compression",
		"allow_reenable_disabled_indexes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run heap lines missing %q:\n%s", want, got)
		}
	}
	if allowed := strings.Join(heapScopeLines(sizes, true), "\n"); !strings.Contains(allowed, "allowed by allow_reenable_disabled_indexes") {
		t.Errorf("opted-in wording missing:\n%s", allowed)
	}
	if offline := strings.Join(heapScopeLines(nil, false), "\n"); !strings.Contains(offline, "not listed offline") {
		t.Errorf("offline wording missing:\n%s", offline)
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./cmd/sqlgopace -run TestDryRunHeapScopeLines -v`
Expected: FAIL — `undefined: heapScopeLines`.

- [ ] **Step 3: Implement**

1. In `buildEngine`'s `opts`, after `run.WithLogWatch(...)`:

```go
		run.WithSizeReader(conn),
```

2. In `tuiForwarder.ops`, carry the detail:

```go
		rows[i] = tui.OperationRow{Index: o.Index, Label: opLabel(o.Command, o.Target), Status: "TO RUN", Detail: o.Detail}
```

and in `tuiForwarder.step`, on `StepFinished`:

```go
		f.send(tui.StepDoneMsg{Index: ev.Index, Outcome: ev.Outcome, Detail: ev.Detail})
```

3. Add `heapScopeLines` to `main.go`:

```go
// heapScopeLines renders the dry run's note under a rebuild_heap: what else the statement
// rewrites, and the disabled indexes it would re-enable. sizes is nil for an offline dry
// run or a failed read, where the lines say so instead of guessing.
func heapScopeLines(sizes []mssql.StructureSize, allowed bool) []string {
	if len(sizes) == 0 {
		return []string{"--     also rebuilds every nonclustered index on the table, and re-enables any disabled one (not listed offline)"}
	}
	var parts []string
	var heapKB int64
	for _, s := range sizes {
		if s.IndexID == 0 {
			heapKB = s.UsedKB
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", s.Name, report.HumanizeKB(s.UsedKB)))
	}
	if len(parts) == 0 {
		return nil
	}
	lines := []string{fmt.Sprintf("--     also rebuilds %d nonclustered index(es): %s (heap %s; %s rewritten)",
		len(parts), strings.Join(parts, ", "), report.HumanizeKB(heapKB), report.HumanizeKB(preflight.SumKB(sizes)))}
	if disabled := preflight.DisabledNames(sizes); len(disabled) > 0 {
		tail := "preflight refuses this unless allow_reenable_disabled_indexes: true"
		if allowed {
			tail = "allowed by allow_reenable_disabled_indexes"
		}
		lines = append(lines, fmt.Sprintf("--     re-enables disabled index %s without its compression — %s",
			strings.Join(disabled, ", "), tail))
	}
	return lines
}
```

4. `dryRunManifest` reads the sizes for each `rebuild_heap` when it has a connection (the expander
is the connection: give it a second parameter `sizes run.SizeReader`, nil offline), builds
`map[int][]string`, and passes it to `renderPlan`, which prints the lines after the cancel-only line.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./cmd/sqlgopace`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/sqlgopace
git commit -m "feat(cli): show a heap rebuild's scope in the dry run and on the console"
```

---

### Task 14: the planner counts the whole rewrite

**Files:**
- Modify: `internal/plan/plan.go:21-27` (`Reader`), `:70-100` (`buildInput`), `:182-220`
  (`heapMeasurement`)
- Modify: `internal/maint/decide.go:43-53` (`HeapMeasurement`), `:270-310` (`decideHeap`)
- Modify: `internal/plan/plan_test.go`, `internal/maint/decide_test.go`

**Interfaces:**
- Consumes: `Conn.DisabledIndexes` (Task 11).
- Produces: `Reader.DisabledIndexes(ctx context.Context, objectID int64) ([]string, error)`;
  `maint.HeapMeasurement.NonclusteredMB`, `.NonclusteredCount`, `.RewriteMB`, `.DisabledIndexes []string`.

- [ ] **Step 1: Write the failing tests**

In `internal/maint/decide_test.go`:

```go
// TestDecideHeapBoundsUseDifferentFigures: min_size_mb is the "worth it" gate and reads the
// heap alone; max_size_mb is the cost gate and reads the whole rewrite, because
// ALTER TABLE REBUILD rewrites every nonclustered index in the same statement.
func TestDecideHeapBoundsUseDifferentFigures(t *testing.T) {
	p := baseProfile(t) // heap bounds 10 MB .. 10000 MB in the fixture
	big := maint.HeapMeasurement{
		Schema: "dbo", Table: "MEASUREMENT", SizeMB: 5000,
		NonclusteredMB: 20000, NonclusteredCount: 2, RewriteMB: 25000,
		RecordCount: 1000, ForwardedRecordCount: 500, PageSpaceUsedPercent: 90,
	}
	if d := maint.DecideHeap(big, p); d.Kind != "skip" {
		t.Errorf("Kind = %q, want skip: the rewrite is 25000 MB, above max_size_mb", d.Kind)
	}
	small := big
	small.SizeMB, small.NonclusteredMB, small.RewriteMB = 5, 0, 5
	if d := maint.DecideHeap(small, p); d.Kind != "skip" {
		t.Errorf("Kind = %q, want skip: the heap is below min_size_mb", d.Kind)
	}
	ok := big
	ok.NonclusteredMB, ok.RewriteMB = 2000, 7000
	d := maint.DecideHeap(ok, p)
	if d.Kind != "rebuild_heap" {
		t.Fatalf("Kind = %q, want rebuild_heap", d.Kind)
	}
	if !strings.Contains(d.Reason, "also rebuilds 2 nonclustered index(es)") {
		t.Errorf("Reason = %q, want it to name the indexes it rewrites", d.Reason)
	}
	if d.Metrics.SizeMB != 5000 {
		t.Errorf("Metrics.SizeMB = %d, want the heap alone (5000)", d.Metrics.SizeMB)
	}
}

// TestDecideHeapSkipsDisabledIndex: the planner never emits a manifest preflight would
// refuse, and never sets the opt-in itself.
func TestDecideHeapSkipsDisabledIndex(t *testing.T) {
	p := baseProfile(t)
	m := maint.HeapMeasurement{
		Schema: "dbo", Table: "MEASUREMENT", SizeMB: 500, RewriteMB: 700,
		RecordCount: 1000, ForwardedRecordCount: 500, PageSpaceUsedPercent: 90,
		DisabledIndexes: []string{"IX_MEASUREMENT_OLD"},
	}
	d := maint.DecideHeap(m, p)
	if d.Kind != "skip" {
		t.Fatalf("Kind = %q, want skip", d.Kind)
	}
	if !strings.Contains(d.Reason, "IX_MEASUREMENT_OLD") {
		t.Errorf("Reason = %q, want it to name the disabled index", d.Reason)
	}
}
```

In `internal/plan/plan_test.go`, using the package's existing fake `Reader`:

```go
// TestHeapMeasurementSumsPartitionsAndIndexes: the fixed defect. The heap's own size is the
// sum over its partitions (plan.go read partition 1 only), and the rewrite adds every
// nonclustered index of the table.
func TestHeapMeasurementSumsPartitionsAndIndexes(t *testing.T) {
	r := &fakeReader{inventory: []mssql.InventoryObject{
		{Schema: "dbo", Table: "MEASUREMENT", ObjectID: 1, IndexID: 0, Type: 0, PartitionNumber: 1, SizeMB: 3000},
		{Schema: "dbo", Table: "MEASUREMENT", ObjectID: 1, IndexID: 0, Type: 0, PartitionNumber: 2, SizeMB: 2000},
		{Schema: "dbo", Table: "MEASUREMENT", ObjectID: 1, IndexID: 2, IndexName: "IX_A", Type: 2, PartitionNumber: 1, SizeMB: 1500},
	}}
	in, err := plan.BuildInput(context.Background(), r, profileWithHeapBounds(10, 100000), plan.Categories{}, "PRODDB", io.Discard)
	if err != nil {
		t.Fatalf("BuildInput() error = %v", err)
	}
	if len(in.Heaps) != 1 {
		t.Fatalf("heaps = %d, want 1", len(in.Heaps))
	}
	h := in.Heaps[0]
	if h.SizeMB != 5000 || h.NonclusteredMB != 1500 || h.RewriteMB != 6500 || h.NonclusteredCount != 1 {
		t.Errorf("heap measurement = %+v, want heap 5000, nonclustered 1500 (1 index), rewrite 6500", h)
	}
}
```

Use whatever the package's exported entry point is (`BuildInput` may be unexported — call it through
the exported analysis function the other tests use).

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./internal/maint ./internal/plan -run 'Heap' -v`
Expected: FAIL — unknown fields `NonclusteredMB`, `RewriteMB`.

- [ ] **Step 3: Implement**

1. `HeapMeasurement` gains:

```go
	NonclusteredMB    int64    // every nonclustered index of the table, summed over partitions
	NonclusteredCount int      // how many, for the reason line
	RewriteMB         int64    // SizeMB + NonclusteredMB: what ALTER TABLE REBUILD rewrites
	DisabledIndexes   []string // disabled indexes the rebuild would re-enable; any means skip
```

2. `decideHeap`: replace the two size checks with

```go
	if m.SizeMB < p.Heap.MinSizeMB {
		return skipDecision("heap", target, fmt.Sprintf("heap %d MB below min %d MB", m.SizeMB, p.Heap.MinSizeMB))
	}
	if m.RewriteMB > p.Heap.MaxSizeMB {
		return skipDecision("heap", target, fmt.Sprintf(
			"rebuild rewrites %d MB (heap %d MB + %d nonclustered %d MB), above max %d MB",
			m.RewriteMB, m.SizeMB, m.NonclusteredCount, m.NonclusteredMB, p.Heap.MaxSizeMB))
	}
	if len(m.DisabledIndexes) > 0 {
		return skipDecision("heap", target, fmt.Sprintf(
			"rebuild would re-enable disabled index(es) %s without their compression; drop them, or rebuild by hand with allow_reenable_disabled_indexes",
			strings.Join(m.DisabledIndexes, ", ")))
	}
```

and append to the emitted decision's reason, when `m.NonclusteredCount > 0`:

```go
	reason += fmt.Sprintf("; also rebuilds %d nonclustered index(es) (%d MB; %d MB rewritten)",
		m.NonclusteredCount, m.NonclusteredMB, m.RewriteMB)
```

`Metrics.SizeMB` stays `m.SizeMB` — the heap alone.

3. `plan.go`: add `DisabledIndexes` to `Reader`; in `buildInput`, build
`byObject := map[int64][]mssql.InventoryObject` in one pass before the group loop, and pass it to
`heapMeasurement`, which now sums the heap's own partitions and the table's other structures, reads
`DisabledIndexes` (on a read error, skip the heap and log why), and writes the log line for every
skip instead of returning a bare `false`:

```go
	if sizeMB < p.Heap.MinSizeMB {
		fmt.Fprintf(logw, "-- skip heap %s.%s: heap %d MB below heap.min_size_mb %d\n", head.Schema, head.Table, sizeMB, p.Heap.MinSizeMB)
		return maint.HeapMeasurement{}, false
	}
	if rewriteMB > p.Heap.MaxSizeMB {
		fmt.Fprintf(logw, "-- skip heap %s.%s: rebuild rewrites %d MB (heap %d MB + %d nonclustered %d MB), above heap.max_size_mb %d\n",
			head.Schema, head.Table, rewriteMB, sizeMB, ncCount, ncMB, p.Heap.MaxSizeMB)
		return maint.HeapMeasurement{}, false
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/plan ./internal/maint ./cmd/sqlgopace`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/plan internal/maint
git commit -m "fix(plan): weigh a heap rebuild by everything it rewrites"
```

---

### Task 15: docs, CHANGELOG, version

**Files:**
- Modify: `internal/version/VERSION`, `CHANGELOG.md`, `README.md`, `docs/permissions.md`,
  `docs/running.md`, `docs/configuration.md`, `docs/specs/MAINTENANCE.md`,
  `docs/specs/COMPRESSION-SCOPE.md`, `docs/specs/CANCEL-ONLY.md`, `docs/specs/TODO.md`,
  `maintenance_profile.yaml`, `internal/scaffold/assets/maintenance_profile.yaml`

- [ ] **Step 1: Update the operator-facing docs**

- `README.md`: `allow_reenable_disabled_indexes` in the `rebuild_heap` reference, one line saying
  what the rebuild does to a disabled index.
- `docs/permissions.md`: `VIEW DEFINITION` now also drives the engine's size lines, the connected
  dry run and the disabled-index guard — remove "Nothing else in the tool needs it".
- `docs/running.md`: the row detail, the `.log` size lines and totals, that the figure is a net
  change under a live workload, and that history totals double-count a structure rebuilt by two
  manifests.
- `docs/configuration.md`: mention the new preflight checks if its list enumerates them.

- [ ] **Step 2: Update the specs and the profile twins**

- `MAINTENANCE.md` §5.3 (two bounds, disabled-index skip) and §9 (implemented; point at
  OBJECT-SIZES.md).
- `COMPRESSION-SCOPE.md` §4, §4.5, §7: the heap skip line is owned by OBJECT-SIZES.md.
- `CANCEL-ONLY.md` §2: the console carries `cancel only` on each row now.
- `maintenance_profile.yaml` **and** `internal/scaffold/assets/maintenance_profile.yaml`: the
  comments on `heap.min_size_mb` / `heap.max_size_mb`. These two are byte-pinned twins — change both
  identically or `internal/scaffold`'s test fails.
- `TODO.md`: move nothing; the paused-resumable follow-up stays open.

- [ ] **Step 3: Bump the version and write the CHANGELOG**

`internal/version/VERSION` becomes `0.35.0`. Add a `## [0.35.0]` section in the sober house style,
one bullet per change, with these facts and no essay:

- size before/after per structure, per manifest, and the two `runs` columns;
- the preflight now sizes a heap rebuild from the whole table (it sized the heap alone);
- the planner weighs `heap.max_size_mb` against the whole rewrite and sums the heap's partitions
  (it read partition 1) — **migration note**: heaps may now fall outside the bound, revisit
  `heap.max_size_mb`;
- a heap rebuild on a table with a disabled index now fails preflight — **migration note**: name
  `allow_reenable_disabled_indexes`;
- `maintenance_analysis.size_mb` for a partitioned heap now means the sum, not partition 1;
- the console keeps each operation's note on its own row (the 0.34.0 rollback-on-cancel notice was
  erased by the next kill).

- [ ] **Step 4: Verify the whole tree**

```bash
go build ./... && go vet ./...
go test -race ./...
for f in $(git diff --name-only HEAD | grep '\.go$'); do tr -d '\r' < "$f" | gofmt -l - ; done   # expect no output
golangci-lint run ./...   # expect only the pre-existing gofmt/CRLF noise
```

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "docs: object sizes, heap rebuild scope, and the 0.35.0 release notes"
```

---

## Self-review

**Spec coverage.** §1 → Task 1; §2 → Task 5; §3 → Tasks 3 and 6; §4 → Task 13; §5.1 → Task 10;
§5.2 → Task 9; §5.3 → Tasks 10, 12, 13; §5.4 → Tasks 4, 7; §5.5 → Task 9 (`sizeTotals`); §5.6 →
Task 8; §5.7 → Task 15 (`docs/running.md`); §6 → Tasks 11, 14; error table → Tasks 3, 6, 9, 14;
docs list → Task 15.

**Known gaps, deliberate.** The spec's `Rewritten` lives in `internal/preflight` (Claude review
finding 13) rather than `internal/mssql`; `internal/preflight` gains an import of `internal/report`
for `HumanizeKB` (Task 6) — if that ever becomes a cycle, move the formatter, don't duplicate it.
The `reorganize_index` space check is skipped on Microsoft's documented grounds (Task 3, step 3),
which the spec did not state explicitly.

**Types used across tasks:** `mssql.StructureSize` (1) → `preflight.Rewritten/SumKB/DisabledNames/SizedOperation`
(2) → `run.SizeReader`/`readSizes`/`sizeLines`/`sizeTotals` (9) → `report.SizeLine`/`SizeUnknown`/
`HumanizeKB` (4, 7) → `run.OpInfo.Detail`/`tui.OperationRow.Detail`/`tui.StepDoneMsg.Detail` (10, 12,
13) → `maint.HeapMeasurement.RewriteMB` and `plan.Reader.DisabledIndexes` (11, 14). Names match
across tasks.
