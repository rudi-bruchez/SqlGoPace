# Tail-object identification for shrink — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a shrink stalls (or proactively, on a flag), name the object owning the data file's last allocated page — the tail object `DBCC SHRINKFILE` cannot relocate past — and feed it into the existing `.contended.yaml` sidecar so `plan --confirmed` promotes it.

**Architecture:** A backward per-page walk over `sys.dm_db_page_info` (`internal/mssql`) finds the tail object. The shrink driver (`internal/run`) runs the walk at give-up (always) and at chunk-loop entry (flag `identify_tail_object`), gated on SQL 2019+, and emits it as a `ReactionEvent`; the engine's per-operation sink records it into the same `contendedCapture` accumulator the lock capture uses and writes the sidecar. The planner (`internal/maint`/`internal/plan`) consumes it via a richer `Confirmation` value so tail-position blockers sort ahead of lock-confirmed ones.

**Tech Stack:** Go, `github.com/microsoft/go-mssqldb`, `gopkg.in/yaml.v3`, SQL Server 2019+ (`sys.dm_db_page_info`).

**Spec:** `docs/superpowers/specs/2026-07-29-tail-object-identification-design.md`

## Global Constraints

- **Idiomatic Go, KISS.** No new layers/interfaces/generics beyond what a task needs. Match surrounding code.
- **English only** in all code, comments, identifiers, docs.
- **No query timeout** around executing DDL — never add `context.WithTimeout` around a chunk. (Not touched here, but keep it in mind.)
- **`make test` runs with `-race` and needs no database.** New unit tests must pass without a DB.
- **Integration tests** live behind the `//go:build integration` tag and run only when `SQLGOPACE_TEST_DSN` is set. The one live-SQL test here (Task 2) is integration-tagged.
- **US spelling** in comments/identifiers. golangci-lint v2 (`errcheck`/`govet`/`staticcheck`/`ineffassign`/`unused` on by default) must pass: `make lint`.
- **Version** lives in `internal/version/VERSION` (embedded); bump it in the final task, no build flags.
- **Windows:** stop any running `bin/sqlgopace.exe` before `make build` (locked binary).
- **After each task:** `go build ./... && go test -race ./<touched packages>`; commit.

---

### Task 1: Tail-walk page-cap math (pure)

The backward walk is bounded by the file's free-page count (a trailing unallocated run can't exceed total free pages), hard-capped by an absolute backstop. This is pure integer math — unit-test it in isolation.

**Files:**
- Modify: `internal/run/shrink_calc.go` (add constants + `tailWalkPages`)
- Test: `internal/run/shrink_calc_test.go` (add one test func)

**Interfaces:**
- Produces: `func tailWalkPages(freeMB int) int` — pages to walk back = `min(freeMB*128 + tailWalkMargin, tailWalkAbsCap)`, never negative. Consts `tailWalkMargin` (512) and `tailWalkAbsCap` (262144).

- [ ] **Step 1: Write the failing test**

Add to `internal/run/shrink_calc_test.go`:

```go
func TestTailWalkPages(t *testing.T) {
	tests := []struct {
		name   string
		freeMB int
		want   int
	}{
		{"small free space uses free pages plus margin", 10, 10*128 + 512},
		{"zero free still allows the margin", 0, 512},
		{"negative free clamps to the margin floor", -5, 512},
		{"huge free space clamps to the absolute cap", 1_000_000, 262144},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tailWalkPages(tt.freeMB); got != tt.want {
				t.Errorf("tailWalkPages(%d) = %d, want %d", tt.freeMB, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/run -run TestTailWalkPages`
Expected: FAIL — `undefined: tailWalkPages`.

- [ ] **Step 3: Write minimal implementation**

Add to `internal/run/shrink_calc.go`:

```go
// Tail-object walk bounds (see the tail-object design spec §1). The walk skips the
// file's trailing unallocated pages until it reaches the last allocated page; that run
// can never exceed the file's total free-page count, so the cap is derived from free
// space, with an absolute backstop for a mostly-free file whose last allocated page sits
// near the front.
const (
	tailWalkMargin = 512    // extra pages absorbing concurrent allocation churn
	tailWalkAbsCap = 262144 // ~2 GB at 8 KB/page: hard ceiling on the backward loop
)

// tailWalkPages returns how many pages back the tail-object walk may scan for a data file
// with freeMB megabytes free: min(free pages + margin, absolute cap), floored at the
// margin so a full/near-full file still gets a small look-back window.
func tailWalkPages(freeMB int) int {
	pages := freeMB*128 + tailWalkMargin
	if pages < tailWalkMargin {
		pages = tailWalkMargin
	}
	return min(pages, tailWalkAbsCap)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/run -run TestTailWalkPages`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/run/shrink_calc.go internal/run/shrink_calc_test.go
git commit -m "feat(shrink): tail-walk page-cap math (free-space-derived bound)"
```

---

### Task 2: `mssql.FileSpace.FileID` + `TailObject` + `FindTailObject`

Add the file_id to `FileSpace` (so the driver can pass it) and the productized backward-walk read. The read is DB-touching, so its behavioural test is integration-tagged.

**Files:**
- Modify: `internal/mssql/shrink.go` (add `FileID` to `FileSpace` + `fileSpaceSQL` column + scan; add `TailObject` struct, `tailObjectSQL`, `FindTailObject`)
- Test: `internal/mssql/shrink_tailobject_integration_test.go` (new, `//go:build integration`)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `FileSpace.FileID int`
  - `type TailObject struct { ObjectID int64; Schema, Table string; IndexID, PageFromEnd int }`
  - `func (c *Conn) FindTailObject(ctx context.Context, fileID, maxPagesBack int) (TailObject, bool, error)`

- [ ] **Step 1: Add `FileID` to `FileSpace` and the SQL**

In `internal/mssql/shrink.go`, add the field to the struct:

```go
type FileSpace struct {
	Name     string
	FileID   int
	TypeDesc string
	SizeMB   int
	UsedMB   int
	FreeMB   int
}
```

Add `file_id` to `fileSpaceSQL` and the scan. Change `fileSpaceSQL` to:

```go
const fileSpaceSQL = `
SELECT name, file_id, type_desc,
       CAST(size / 128.0 AS INT)                                                 AS size_mb,
       CAST(CEILING(CAST(FILEPROPERTY(name, 'SpaceUsed') AS BIGINT) / 128.0) AS INT) AS used_mb
FROM sys.database_files
WHERE type_desc = @type
ORDER BY file_id;`
```

and the scan in `FileSpace` (the method) to:

```go
if err := rows.Scan(&f.Name, &f.FileID, &f.TypeDesc, &f.SizeMB, &f.UsedMB); err != nil {
```

- [ ] **Step 2: Add the `TailObject` type and the walk**

Append to `internal/mssql/shrink.go`:

```go
// TailObject is the user object owning the physically-last allocated page of a data file:
// the object DBCC SHRINKFILE must relocate past, and the binding constraint on how far the
// file can shrink. PageFromEnd is how many pages from the file end that page sits (0 = the
// very last page).
type TailObject struct {
	ObjectID    int64
	Schema      string
	Table       string
	IndexID     int
	PageFromEnd int
}

// tailObjectSQL walks backward from the last page of @file_id, skipping pages with no user
// object (free/unallocated pages and allocation-bitmap pages return NULL object_id), and
// returns the first page owned by a user object. The size read and the walk are one batch,
// so the file size is consistent for the walk. A dropped object resolves to NULL names
// (recorded as object_id with empty schema/table). Requires SQL Server 2019+.
const tailObjectSQL = `
SET NOCOUNT ON;
DECLARE @file_id int = @fid, @max_back int = @maxback;
DECLARE @last_page_id int, @page_id int, @floor int;
SELECT @last_page_id = CAST(size AS int) - 1 FROM sys.database_files WHERE file_id = @file_id;
IF @last_page_id IS NULL OR @last_page_id < 0 RETURN;
SET @page_id = @last_page_id;
SET @floor = @last_page_id - @max_back;
IF @floor < 0 SET @floor = 0;
WHILE @page_id >= @floor
BEGIN
    DECLARE @obj int;
    SELECT @obj = object_id FROM sys.dm_db_page_info(DB_ID(), @file_id, @page_id, 'LIMITED');
    IF @obj IS NOT NULL
    BEGIN
        SELECT
            pi.object_id                        AS object_id,
            OBJECT_SCHEMA_NAME(pi.object_id)    AS schema_name,
            OBJECT_NAME(pi.object_id)           AS object_name,
            pi.index_id                         AS index_id,
            @last_page_id - @page_id            AS page_from_end
        FROM sys.dm_db_page_info(DB_ID(), @file_id, @page_id, 'LIMITED') AS pi;
        RETURN;
    END
    SET @page_id -= 1;
END`

// FindTailObject walks backward from the last page of fileID via sys.dm_db_page_info,
// returning the first page owned by a user object. It scans at most maxPagesBack pages;
// found=false means it reached that bound (or the file end) without hitting an allocated
// page. SQL 2019+ only — the caller gates on version before calling. Names may be empty if
// the object was dropped mid-walk. It takes no transaction locks (only brief page latches).
func (c *Conn) FindTailObject(ctx context.Context, fileID, maxPagesBack int) (TailObject, bool, error) {
	var (
		t                    TailObject
		schema, name         sql.NullString
	)
	err := c.pool.QueryRowContext(ctx, tailObjectSQL,
		sql.Named("fid", fileID), sql.Named("maxback", maxPagesBack)).
		Scan(&t.ObjectID, &schema, &name, &t.IndexID, &t.PageFromEnd)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TailObject{}, false, nil
	case err != nil:
		return TailObject{}, false, fmt.Errorf("find tail object (file_id %d): %w", fileID, err)
	default:
		t.Schema, t.Table = schema.String, name.String
		return t, true, nil
	}
}
```

(`errors` and `sql` are already imported in this file.)

- [ ] **Step 3: Verify it builds**

Run: `go build ./...`
Expected: builds. (No non-DB unit test for the SQL itself; conformance to `ShrinkReader` is asserted in Task 6.)

- [ ] **Step 4: Add the integration test**

Create `internal/mssql/shrink_tailobject_integration_test.go`:

```go
//go:build integration

package mssql_test

import (
	"context"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// TestFindTailObjectIntegration creates a table, forces it to own the tail of the primary
// data file, and asserts FindTailObject names it. Requires SQLGOPACE_TEST_DSN (2019+).
func TestFindTailObjectIntegration(t *testing.T) {
	ctx := context.Background()
	conn := openTestConn(t) // existing integration helper in this package
	defer conn.Close()

	// A dedicated table with enough rows to occupy pages near the file tail.
	exec(t, conn, `IF OBJECT_ID('dbo.sqlgopace_tail_test') IS NOT NULL DROP TABLE dbo.sqlgopace_tail_test;`)
	exec(t, conn, `CREATE TABLE dbo.sqlgopace_tail_test (id int identity primary key, filler char(4000) not null);`)
	exec(t, conn, `INSERT INTO dbo.sqlgopace_tail_test (filler) SELECT TOP (2000) 'x' FROM sys.all_objects a CROSS JOIN sys.all_objects b;`)
	defer exec(t, conn, `DROP TABLE dbo.sqlgopace_tail_test;`)

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
```

Note: reuse whatever connection/exec helpers the existing `internal/mssql` integration tests use (see `internal/mssql/integration_test.go`); adjust `openTestConn`/`exec` names to match. If no such helpers exist, inline a `mssql.Open(...)` from `SQLGOPACE_TEST_DSN` as those tests do.

- [ ] **Step 5: Run unit tests (no DB) and, if a DSN is available, the integration test**

Run: `go test -race ./internal/mssql`
Expected: PASS (integration test skipped without the tag).
If a 2019+ DSN is available: `go test -tags integration ./internal/mssql -run TestFindTailObjectIntegration`.

- [ ] **Step 6: Commit**

```bash
git add internal/mssql/shrink.go internal/mssql/shrink_tailobject_integration_test.go
git commit -m "feat(mssql): FileSpace.FileID and FindTailObject backward page walk"
```

---

### Task 3: `maint.ContendedObject` new fields + `ParseContended` round-trip

Additive fields on the sidecar object model so both confirmation kinds and the tail
diagnostics serialize. Backward-compatible under `KnownFields(true)`.

**Files:**
- Modify: `internal/maint/contended.go` (add fields to `ContendedObject`)
- Test: `internal/maint/contended_test.go` (add round-trip test; create the file if absent)

**Interfaces:**
- Produces: `ContendedObject` gains `IndexID int` (`yaml:"index_id,omitempty"`), `ConfirmedBy string` (`yaml:"confirmed_by,omitempty"`), `PageFromEnd int` (`yaml:"page_from_end,omitempty"`).

- [ ] **Step 1: Write the failing test**

Add to `internal/maint/contended_test.go`:

```go
func TestParseContendedRoundTripsNewFields(t *testing.T) {
	in := []byte(`database: MyDB
observed:
    - object_id: 42
      schema: dbo
      table: Big
      confirmed_by: tail_position
      index_id: 1
      page_from_end: 3
`)
	doc, err := maint.ParseContended(in)
	if err != nil {
		t.Fatalf("ParseContended: %v", err)
	}
	if len(doc.Observed) != 1 {
		t.Fatalf("observed = %d, want 1", len(doc.Observed))
	}
	o := doc.Observed[0]
	if o.ConfirmedBy != "tail_position" || o.IndexID != 1 || o.PageFromEnd != 3 {
		t.Errorf("new fields not decoded: %+v", o)
	}
}

func TestParseContendedAcceptsLegacySidecar(t *testing.T) {
	// A sidecar written before this change has none of the new fields.
	in := []byte(`database: MyDB
observed:
    - object_id: 7
      schema: dbo
      table: Old
      lock_mode: Sch-M
      times_blocked: 2
      first_seen: t0
      last_seen: t1
`)
	if _, err := maint.ParseContended(in); err != nil {
		t.Fatalf("legacy sidecar must still parse: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maint -run TestParseContended`
Expected: FAIL — `KnownFields(true)` rejects `confirmed_by`/`index_id`/`page_from_end` (fields not yet in the struct).

- [ ] **Step 3: Add the fields**

In `internal/maint/contended.go`, extend `ContendedObject`:

```go
type ContendedObject struct {
	ObjectID     int64  `yaml:"object_id"`
	Schema       string `yaml:"schema"`
	Table        string `yaml:"table"`
	LockMode     string `yaml:"lock_mode,omitempty"`
	TimesBlocked int    `yaml:"times_blocked,omitempty"`
	FirstSeen    string `yaml:"first_seen,omitempty"`
	LastSeen     string `yaml:"last_seen,omitempty"`
	// Tail-position capture (see the tail-object design spec). ConfirmedBy is "lock" for a
	// lock-held blocker or "tail_position" for one found by the backward page walk; empty is
	// a legacy sidecar, read as "lock". IndexID/PageFromEnd are set only for tail entries.
	IndexID     int    `yaml:"index_id,omitempty"`
	ConfirmedBy string `yaml:"confirmed_by,omitempty"`
	PageFromEnd int    `yaml:"page_from_end,omitempty"`
}
```

Note the added `,omitempty` on `LockMode`/`TimesBlocked`/`FirstSeen`/`LastSeen`: tail-only entries leave these zero and should not emit them. This changes lock-entry serialization only by omitting genuinely-empty values — verify the existing contended test (Task 5 touches it) still matches.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/maint -run TestParseContended`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/maint/contended.go internal/maint/contended_test.go
git commit -m "feat(maint): tail-position fields on ContendedObject sidecar model"
```

---

### Task 4: `Confirmation` type + `DecidePreShrink` ordering/annotation + callers

Change the `confirmed` map from `map[int64]int` to `map[int64]maint.Confirmation` so tail-position blockers sort ahead of lock-confirmed ones and are annotated correctly. This is *the* signature ripple — it must land with all callers in one commit to keep the build green.

**Files:**
- Modify: `internal/maint/shrink.go` (`Confirmation` type; `DecidePreShrink` signature, ordering, annotation, heap match)
- Modify: `internal/plan/shrink.go` (`AnalyzePreShrink` `confirmed` param type)
- Modify: `cmd/sqlgopace/shrink_plan.go` (`confirmedSetFor` return type; `planShrink` `confirmed` param)
- Modify: callers the compiler flags (the `plan` subcommand in `cmd/sqlgopace` that calls `planShrink`/`confirmedSetFor`)
- Test: `internal/maint/shrink_test.go` (extend `DecidePreShrink` table tests)

**Interfaces:**
- Consumes: `ContendedObject.ConfirmedBy/IndexID/PageFromEnd` (Task 3).
- Produces:
  - `type Confirmation struct { TimesBlocked int; ByTail bool; IndexID int; PageFromEnd int }`
  - `func DecidePreShrink(indexes []ShrinkIndexMeasurement, heaps []ShrinkHeapMeasurement, p *Profile, confirmed map[int64]Confirmation) PreShrinkPlan`
  - `func confirmedSetFor(doc maint.ContendedDoc, db string) (map[int64]maint.Confirmation, error)`

- [ ] **Step 1: Write the failing test**

Add to `internal/maint/shrink_test.go` (adapt to the file's existing helpers/fixtures):

```go
func TestDecidePreShrinkTailFirst(t *testing.T) {
	p := &Profile{}
	p.Shrink.ReorganizeBelowDensityPercent = 70
	p.Index.PageCountFloor = 100

	indexes := []ShrinkIndexMeasurement{
		{ObjectID: 1, Schema: "dbo", Table: "LockOnly", Index: "IX1", PageCount: 1000, AvgPageSpaceUsedPercent: 90},
		{ObjectID: 2, Schema: "dbo", Table: "TailOnly", Index: "IX2", PageCount: 1000, AvgPageSpaceUsedPercent: 90},
	}
	confirmed := map[int64]Confirmation{
		1: {TimesBlocked: 5},                       // lock-confirmed, dense
		2: {ByTail: true, IndexID: 2, PageFromEnd: 3}, // tail-confirmed, dense
	}

	pl := DecidePreShrink(indexes, nil, p, confirmed)

	if len(pl.Reorganizes) != 2 {
		t.Fatalf("reorganizes = %d, want 2", len(pl.Reorganizes))
	}
	if pl.Reorganizes[0].Table != "TailOnly" {
		t.Errorf("tail-confirmed object must lead; got %q first", pl.Reorganizes[0].Table)
	}
	if !strings.Contains(pl.ReorganizeNotes[0], "tail-position") {
		t.Errorf("tail note = %q, want it to mention tail-position", pl.ReorganizeNotes[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maint -run TestDecidePreShrinkTailFirst`
Expected: FAIL to compile — `Confirmation` undefined and `DecidePreShrink` still takes `map[int64]int`.

- [ ] **Step 3: Add `Confirmation` and update `DecidePreShrink`**

In `internal/maint/shrink.go`, add the type and change the signature + the confirmed handling:

```go
// Confirmation is how an object_id was confirmed as a shrink blocker, read from a
// .contended.yaml sidecar. ByTail entries (position-confirmed by the tail-object walk) are
// the definitive blocker and sort ahead of lock-confirmed ones.
type Confirmation struct {
	TimesBlocked int  // lock captures; 0 for a tail-position entry
	ByTail       bool // confirmed_by == "tail_position"
	IndexID      int  // tail entries: the allocation unit at the tail
	PageFromEnd  int  // tail entries: distance from file end (smaller = more binding)
}
```

Change the signature to `confirmed map[int64]Confirmation`. In the index loop, replace the
`tb, isConfirmed := confirmed[m.ObjectID]` block and the entry construction:

```go
c, isConfirmed := confirmed[m.ObjectID]
...
op := ddl.ReorganizeIndex{Schema: m.Schema, Table: m.Table, Index: m.Index, LOBCompaction: p.Index.LOBCompaction}
switch {
case isConfirmed && c.ByTail:
	confirmedEntries = append(confirmedEntries, entry{op,
		fmt.Sprintf("confirmed tail-position blocker (index_id=%d, %d pages from end)", c.IndexID, c.PageFromEnd),
		c})
case isConfirmed && dense:
	confirmedEntries = append(confirmedEntries, entry{op, "confirmed blocker — added despite density", c})
case isConfirmed:
	confirmedEntries = append(confirmedEntries, entry{op, fmt.Sprintf("confirmed blocker (times_blocked=%d)", c.TimesBlocked), c})
case dense:
	continue
default:
	rest = append(rest, entry{op, "", Confirmation{}})
}
```

Change the `entry` struct's `conf int` field to `conf Confirmation`, and the sort to put
tail first, then times_blocked desc, then page_from_end asc:

```go
type entry struct {
	op   ddl.ReorganizeIndex
	note string
	conf Confirmation
}
...
sort.SliceStable(confirmedEntries, func(i, j int) bool {
	a, b := confirmedEntries[i].conf, confirmedEntries[j].conf
	if a.ByTail != b.ByTail {
		return a.ByTail // tail-position blockers lead
	}
	if a.ByTail && b.ByTail && a.PageFromEnd != b.PageFromEnd {
		return a.PageFromEnd < b.PageFromEnd // closest to the file end is most binding
	}
	return a.TimesBlocked > b.TimesBlocked
})
```

In the heap loop, replace `tb, isConfirmed := confirmed[m.ObjectID]` with
`c, isConfirmed := confirmed[m.ObjectID]` and set `TimesBlocked: c.TimesBlocked` on the
`HeapAdvisory` (a `ByTail` confirmation still marks it `Confirmed: isConfirmed`).

- [ ] **Step 4: Update the callers**

`internal/plan/shrink.go` — change `AnalyzePreShrink`'s `confirmed map[int64]int` param to `map[int64]maint.Confirmation` (it only forwards it to `DecidePreShrink`).

`cmd/sqlgopace/shrink_plan.go` — change `confirmedSetFor` to build `Confirmation`s:

```go
func confirmedSetFor(doc maint.ContendedDoc, db string) (map[int64]maint.Confirmation, error) {
	if !strings.EqualFold(doc.Database, db) {
		return nil, fmt.Errorf("--confirmed sidecar is for database %q, connected to %q", doc.Database, db)
	}
	set := make(map[int64]maint.Confirmation, len(doc.Observed))
	for _, o := range doc.Observed {
		set[o.ObjectID] = maint.Confirmation{
			TimesBlocked: o.TimesBlocked,
			ByTail:       o.ConfirmedBy == "tail_position",
			IndexID:      o.IndexID,
			PageFromEnd:  o.PageFromEnd,
		}
	}
	return set, nil
}
```

and change `planShrink`'s `confirmed map[int64]int` param to `map[int64]maint.Confirmation`.
Then fix the compiler-flagged caller of `planShrink`/`confirmedSetFor` in the `plan`
subcommand (its local `confirmed` variable type). Run `go build ./...` and follow the
errors — they are all this one type change.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test -race ./internal/maint ./internal/plan ./cmd/sqlgopace`
Expected: PASS, including the existing `DecidePreShrink`/`confirmedSetFor` tests (update any that construct `map[int64]int{...}` literals to `map[int64]maint.Confirmation{...}` — e.g. `{2}` becomes `{TimesBlocked: 2}`).

- [ ] **Step 6: Commit**

```bash
git add internal/maint/shrink.go internal/maint/shrink_test.go internal/plan/shrink.go cmd/sqlgopace/shrink_plan.go cmd/sqlgopace/*.go
git commit -m "feat(maint): Confirmation type — tail-position blockers lead pre-shrink reorganizes"
```

---

### Task 5: Capture-recording side — `ReactionEvent.Tail`, `addTail`, `Engine.captureTail`

The engine's sink records a found tail object into the same accumulator the lock capture uses. This task lands the *consuming* side (nothing emits a `Tail` event yet, so it compiles and is dormant).

**Files:**
- Modify: `internal/run/reaction.go` (`TailFinding` type; `ReactionEvent.Tail`)
- Modify: `internal/run/contended.go` (`capturedObject` fields; `addTail`; `doc` emits new fields + explicit `confirmed_by`; extract `writeContended`; add `Engine.captureTail`)
- Modify: `internal/run/engine.go` (sink branch on `ev.Tail != nil`)
- Test: `internal/run/contended_test.go` (add merge/tail tests)

**Interfaces:**
- Consumes: `maint.ContendedObject` new fields (Task 3).
- Produces:
  - `type TailFinding struct { ObjectID int64; Schema, Table string; IndexID, PageFromEnd int }`
  - `ReactionEvent.Tail *TailFinding`
  - `func (c *contendedCapture) addTail(f TailFinding, now string)`
  - `func (e *Engine) captureTail(acc *contendedCapture, name, database string, f TailFinding)`

- [ ] **Step 1: Write the failing test**

Add to `internal/run/contended_test.go`:

```go
func TestContendedAddTailFreshEntry(t *testing.T) {
	var acc contendedCapture
	acc.addTail(TailFinding{ObjectID: 9, Schema: "dbo", Table: "Big", IndexID: 1, PageFromEnd: 4}, "t0")

	doc := acc.doc("DB")
	if len(doc.Observed) != 1 {
		t.Fatalf("observed = %d, want 1", len(doc.Observed))
	}
	o := doc.Observed[0]
	if o.ConfirmedBy != "tail_position" || o.IndexID != 1 || o.PageFromEnd != 4 || o.TimesBlocked != 0 {
		t.Errorf("tail entry wrong: %+v", o)
	}
}

func TestContendedLockThenTailMerges(t *testing.T) {
	var acc contendedCapture
	acc.add(mssql.LockedObject{ObjectID: 9, Schema: "dbo", Table: "Big", Mode: "Sch-M"}, "t0")
	acc.add(mssql.LockedObject{ObjectID: 9, Schema: "dbo", Table: "Big", Mode: "Sch-M"}, "t1")
	acc.addTail(TailFinding{ObjectID: 9, Schema: "dbo", Table: "Big", IndexID: 1, PageFromEnd: 2}, "t2")

	doc := acc.doc("DB")
	if len(doc.Observed) != 1 {
		t.Fatalf("observed = %d, want 1 (merge)", len(doc.Observed))
	}
	o := doc.Observed[0]
	if o.ConfirmedBy != "tail_position" {
		t.Errorf("merged entry must upgrade to tail_position, got %q", o.ConfirmedBy)
	}
	if o.TimesBlocked != 2 || o.LockMode != "Sch-M" || o.FirstSeen != "t0" {
		t.Errorf("merge must preserve lock stats: %+v", o)
	}
	if o.PageFromEnd != 2 || o.IndexID != 1 {
		t.Errorf("merge must fill tail fields: %+v", o)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/run -run TestContended`
Expected: FAIL — `addTail` undefined, `TailFinding` undefined.

- [ ] **Step 3: Add `TailFinding` and the event field**

In `internal/run/reaction.go`:

```go
// TailFinding is a tail object the shrink driver found and passes to the engine sink to
// record (kept in package run so ReactionEvent stays decoupled from internal/mssql).
type TailFinding struct {
	ObjectID    int64
	Schema      string
	Table       string
	IndexID     int
	PageFromEnd int
}
```

and add to `ReactionEvent`:

```go
Tail *TailFinding // non-nil only on a "tail object found" info event
```

- [ ] **Step 4: Extend `capturedObject`, add `addTail`, update `doc`, extract `writeContended`, add `captureTail`**

In `internal/run/contended.go`, extend `capturedObject`:

```go
type capturedObject struct {
	obj         mssql.LockedObject
	firstSeen   string
	lastSeen    string
	count       int
	byTail      bool // upgraded by a tail-object walk
	indexID     int
	pageFromEnd int
}
```

Add `addTail` (mirrors `add`, merge-preserving):

```go
// addTail records a tail-position blocker. On an existing key (the object was also
// lock-captured) it upgrades the entry to tail_position and fills the tail fields while
// preserving the lock stats; a fresh key creates a tail-only entry (no lock stats).
func (c *contendedCapture) addTail(f TailFinding, now string) {
	if c.byID == nil {
		c.byID = make(map[int64]*capturedObject)
	}
	e, ok := c.byID[f.ObjectID]
	if !ok {
		e = &capturedObject{obj: mssql.LockedObject{ObjectID: f.ObjectID, Schema: f.Schema, Table: f.Table}}
		c.byID[f.ObjectID] = e
		c.order = append(c.order, f.ObjectID)
	}
	e.byTail = true
	e.indexID = f.IndexID
	e.pageFromEnd = f.PageFromEnd
}
```

Update `doc` to emit the new fields and an explicit `confirmed_by`:

```go
func (c *contendedCapture) doc(database string) maint.ContendedDoc {
	doc := maint.ContendedDoc{Database: database}
	for _, id := range c.order {
		e := c.byID[id]
		confirmedBy := "lock"
		if e.byTail {
			confirmedBy = "tail_position"
		}
		doc.Observed = append(doc.Observed, maint.ContendedObject{
			ObjectID: e.obj.ObjectID, Schema: e.obj.Schema, Table: e.obj.Table,
			LockMode: e.obj.Mode, TimesBlocked: e.count,
			FirstSeen: e.firstSeen, LastSeen: e.lastSeen,
			IndexID: e.indexID, ConfirmedBy: confirmedBy, PageFromEnd: e.pageFromEnd,
		})
	}
	return doc
}
```

Extract the sidecar write from `captureContended` into a shared helper and add `captureTail`:

```go
// writeContended flushes the accumulator to the sidecar next to the manifest.
func (e *Engine) writeContended(name, database string, acc *contendedCapture) {
	path := filepath.Join(e.dirs.Processing, name+contendedCaptureSuffix)
	if err := os.WriteFile(path, renderContended(name, database, acc), 0o644); err != nil {
		fmt.Fprintf(e.out, "write contended capture %s: %v\n", name, err)
	}
}

// captureTail records a tail-position blocker the shrink driver found (via a ReactionEvent)
// and flushes the sidecar. Best-effort; called only for shrink operations.
func (e *Engine) captureTail(acc *contendedCapture, name, database string, f TailFinding) {
	acc.addTail(f, e.now())
	e.writeContended(name, database, acc)
}
```

In `captureContended`, replace the inline `os.WriteFile(...)` block with `e.writeContended(name, database, acc)`.

- [ ] **Step 5: Add the engine sink branch**

In `internal/run/engine.go`, inside the `sink := func(ev ReactionEvent) {` closure (around line 600), before the `reactionMu.Lock()` narration, add:

```go
if ev.Tail != nil {
	e.captureTail(contended, name, manifest.Database, *ev.Tail)
}
```

(The narration below still logs the info line via the existing `fmt.Fprintf(e.out, ...)`.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `go build ./... && go test -race ./internal/run -run 'TestContended'`
Expected: PASS. Also run the whole package: `go test -race ./internal/run` — fix any existing contended sidecar golden/string assertions that changed because empty `lock_mode`/`times_blocked` are now omitted (Task 3's `omitempty`); update those expectations to match the omitted-empty output.

- [ ] **Step 7: Commit**

```bash
git add internal/run/reaction.go internal/run/contended.go internal/run/engine.go internal/run/contended_test.go
git commit -m "feat(run): record tail-object findings into the contended sidecar via the engine sink"
```

---

### Task 6: Read + emit side — `ShrinkReader.FindTailObject`, `maybeCaptureTail`, driver wiring

The shrink driver runs the walk at give-up (always) and at chunk-loop entry (flag), gated on SQL 2019+, and emits the `Tail` event. This activates the feature end-to-end (once the major version is wired in Task 8).

**Files:**
- Modify: `internal/run/shrink.go` (`ShrinkReader` gains `FindTailObject`; `ShrinkRunnerConfig.SQLMajorVersion`; `ShrinkRunner.major`; `tailProbe`; `maybeCaptureTail`; thread through `Run`/`shrinkData`/`chunkLoop`)
- Modify: test fakes implementing `ShrinkReader` (`internal/run/shrink_driver_test.go` and any other `*_test.go` fake reader in `internal/run`)
- Test: `internal/run/shrink_tailobject_test.go` (new)

**Interfaces:**
- Consumes: `mssql.FileSpace.FileID`, `mssql.Conn.FindTailObject` (Task 2); `tailWalkPages` (Task 1); `TailFinding`, `ReactionEvent.Tail` (Task 5).
- Produces: `ShrinkReader.FindTailObject(ctx, fileID, maxPagesBack int) (mssql.TailObject, bool, error)`; `ShrinkRunnerConfig.SQLMajorVersion int`.

- [ ] **Step 1: Write the failing test**

Create `internal/run/shrink_tailobject_test.go`. It drives `maybeCaptureTail` directly
against the existing `fakeServer` (in `shrink_driver_test.go`), extended in Step 3 with the
tail fields/method. `maybeCaptureTail` uses only the reader and sink, so the runner can be
built minimally:

```go
package run

import (
	"context"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// tailRunner builds a ShrinkRunner for the tail tests: only reader + major version matter.
func tailRunner(s *fakeServer, major int) *ShrinkRunner {
	return NewShrinkRunner(s, s, noPressureSampler{}, &ManualClock{}, ShrinkRunnerConfig{
		Tuning:          testTuning(),
		SQLMajorVersion: major,
	})
}

func TestMaybeCaptureTailEmitsOn2019Plus(t *testing.T) {
	s := &fakeServer{
		tail:      mssql.TailObject{ObjectID: 5, Schema: "dbo", Table: "Big", IndexID: 1, PageFromEnd: 2},
		tailFound: true,
	}
	r := tailRunner(s, 15)

	var events []ReactionEvent
	sink := func(ev ReactionEvent) { events = append(events, ev) }
	warned := new(bool)
	r.maybeCaptureTail(context.Background(), mssql.FileSpace{Name: "d", FileID: 1, FreeMB: 10}, sink, warned)

	var tail *TailFinding
	for _, ev := range events {
		if ev.Tail != nil {
			tail = ev.Tail
		}
	}
	if tail == nil {
		t.Fatal("expected a Tail event on 2019+")
	}
	if tail.ObjectID != 5 || tail.PageFromEnd != 2 {
		t.Errorf("tail finding = %+v", tail)
	}
	if s.tailCalls != 1 {
		t.Errorf("FindTailObject calls = %d, want 1", s.tailCalls)
	}
}

func TestMaybeCaptureTailWarnsOnceBelow2019(t *testing.T) {
	s := &fakeServer{}
	r := tailRunner(s, 13)

	var warns int
	sink := func(ev ReactionEvent) {
		if ev.Kind == "warn" {
			warns++
		}
	}
	warned := new(bool)
	f := mssql.FileSpace{Name: "d", FileID: 1, FreeMB: 10}
	r.maybeCaptureTail(context.Background(), f, sink, warned)
	r.maybeCaptureTail(context.Background(), f, sink, warned)

	if warns != 1 {
		t.Errorf("warnings = %d, want 1 (once per operation)", warns)
	}
	if s.tailCalls != 0 {
		t.Errorf("FindTailObject must not be called below 2019, got %d calls", s.tailCalls)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/run -run TestMaybeCaptureTail`
Expected: FAIL — `maybeCaptureTail`, `ShrinkRunnerConfig.SQLMajorVersion`, and the fake's `tail`/`tailFound`/`tailCalls` are undefined.

- [ ] **Step 3: Extend `ShrinkReader`, the config/runner, and the fake**

In `internal/run/shrink.go`, add to the `ShrinkReader` interface:

```go
// FindTailObject names the user object owning the file's last allocated page (the tail the
// shrink cannot relocate past). SQL 2019+ only — the driver gates on SQLMajorVersion.
FindTailObject(ctx context.Context, fileID, maxPagesBack int) (mssql.TailObject, bool, error)
```

Add `SQLMajorVersion int` to `ShrinkRunnerConfig`, a `major int` field to `ShrinkRunner`, and set it in `NewShrinkRunner` (`major: cfg.SQLMajorVersion`).

Add the helper:

```go
// maybeCaptureTail runs the tail-object walk for one data file and emits the result through
// the sink for the engine to record. Below SQL 2019 it emits one warning per operation (via
// the warned guard) and does not walk. On a hit it emits an info event carrying the finding;
// not-found or a read error records nothing.
func (r *ShrinkRunner) maybeCaptureTail(ctx context.Context, f mssql.FileSpace, sink ReactionSink, warned *bool) {
	if r.major < 15 {
		if warned != nil && !*warned {
			*warned = true
			sink(ReactionEvent{Kind: "warn", Detail: "tail-object identification needs SQL Server 2019+ (sys.dm_db_page_info); skipped"})
		}
		return
	}
	o, found, err := r.reader.FindTailObject(ctx, f.FileID, tailWalkPages(f.FreeMB))
	if err != nil {
		sink(ReactionEvent{Kind: "warn", Detail: fmt.Sprintf("tail-object walk failed on %q: %v", f.Name, err)})
		return
	}
	if !found {
		return
	}
	sink(ReactionEvent{
		Kind:   "info",
		Detail: fmt.Sprintf("tail object %s.%s (index_id=%d, %d pages from end) on %q", o.Schema, o.Table, o.IndexID, o.PageFromEnd, f.Name),
		Tail:   &TailFinding{ObjectID: o.ObjectID, Schema: o.Schema, Table: o.Table, IndexID: o.IndexID, PageFromEnd: o.PageFromEnd},
	})
}
```

Extend the `fakeServer` fake (in `internal/run/shrink_driver_test.go`, which implements
`ShrinkReader`) with the tail fields and method — add to the struct `tail mssql.TailObject`,
`tailFound bool`, `tailCalls int`, and:

```go
func (s *fakeServer) FindTailObject(_ context.Context, _, _ int) (mssql.TailObject, bool, error) {
	s.tailCalls++
	return s.tail, s.tailFound, nil
}
```

`fakeServer` is the only `ShrinkReader` implementation in the run tests; if the compiler
flags another, add the same method to it.

- [ ] **Step 4: Thread the probe through the loop**

Add the probe type and thread it. In `internal/run/shrink.go`:

```go
// tailProbe carries the per-operation tail-object walk state into the shared chunk loop.
// Nil on the tempdb path (tempdb shrinks never walk). proactive runs a walk at loop entry;
// warned is the once-per-operation <2019 warning guard.
type tailProbe struct {
	proactive bool
	warned    *bool
}
```

In `Run`, create the guard once and thread it into `shrinkData`:

```go
warned := new(bool)
...
result, ferr = r.shrinkData(ctx, op, res, ignore, f, sink, &tailProbe{proactive: op.IdentifyTailObject, warned: warned})
```

Change `shrinkData`'s signature to accept `tp *tailProbe` and pass it into `chunkLoop` (its
`prof` stays `nil` for a normal shrink). Change `chunkLoop`'s signature to accept
`tp *tailProbe`. `RunTempdb` passes `nil` for `tp` at both its `chunkLoop` calls.

In `chunkLoop`, at entry (before the `for current > final` loop):

```go
if tp != nil && tp.proactive {
	r.maybeCaptureTail(ctx, f, sink, tp.warned)
}
```

At each give-up return (the two `else if stop { ... return result, nil }` blocks — the
DBCC-error path near "no further progress: %v" and the no-gain path near "no further
progress (work preserved)"), add before the `return`:

```go
if tp != nil {
	r.maybeCaptureTail(ctx, f, sink, tp.warned)
}
```

Note: `op.IdentifyTailObject` does not exist until Task 7. To keep this task compiling,
temporarily source `proactive` from a local `false` and add a `// TODO(task7): op.IdentifyTailObject`
comment — **or** implement Task 7 before this step. Recommended: do Task 7 first, then this
line reads `op.IdentifyTailObject` directly. (The plan lists Task 7 after 6 only because it
is smaller; if executing in order, use the `false` placeholder here and flip it in Task 7.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test -race ./internal/run`
Expected: PASS, including the two new tests and all existing shrink-driver tests (whose
`ShrinkRunnerConfig` has `SQLMajorVersion` defaulting to 0 → walks are gated off, so no
existing test triggers a walk).

- [ ] **Step 6: Commit**

```bash
git add internal/run/shrink.go internal/run/shrink_driver_test.go internal/run/shrink_tailobject_test.go
git commit -m "feat(run): shrink driver runs the tail-object walk at give-up and (flagged) at entry"
```

---

### Task 7: Manifest flag `identify_tail_object` + planner carry-through

Expose the proactive flag on the shrink operation and carry it from the maintenance profile into generated manifests.

**Files:**
- Modify: `internal/ddl/manifest.go` (`Shrink.IdentifyTailObject`)
- Modify: `internal/maint/profile.go` (`Shrink` profile gains `IdentifyTailObject`)
- Modify: `cmd/sqlgopace/shrink_plan.go` (`shrinkManifest` sets `shrink.IdentifyTailObject`)
- Test: `internal/ddl/*_test.go` (parse round-trip) and `cmd/sqlgopace/shrink_plan_test.go` (carry-through)

**Interfaces:**
- Consumes: `ddl.Shrink` (Task 6 reads `op.IdentifyTailObject`).
- Produces: `Shrink.IdentifyTailObject bool` (`yaml:"identify_tail_object,omitempty"`); `Profile.Shrink.IdentifyTailObject bool`.

- [ ] **Step 1: Write the failing test**

Add to the shrink-manifest test in `cmd/sqlgopace/shrink_plan_test.go` (mirroring the existing `max_block_minutes` test at line ~26):

```go
func TestShrinkManifestCarriesIdentifyTailObject(t *testing.T) {
	p := dataShrinkProfile(t, "  identify_tail_object: true\n")
	nm := shrinkManifest(p, "DB", maint.PreShrinkPlan{})
	if nm == nil {
		t.Fatal("shrinkManifest returned nil")
	}
	ops := nm.manifest.Operations
	sh, ok := ops[len(ops)-1].(ddl.Shrink) // the shrink is the last op (after any reorganizes)
	if !ok {
		t.Fatalf("last op = %T, want ddl.Shrink", ops[len(ops)-1])
	}
	if !sh.IdentifyTailObject {
		t.Error("identify_tail_object should be true on the generated shrink op")
	}
}
```

(This mirrors how `TestShrinkManifestReorganizesPrecedeShrink` reaches the op — an inline
type assertion on `nm.manifest.Operations`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sqlgopace -run TestShrinkManifestCarriesIdentifyTailObject`
Expected: FAIL — `identify_tail_object` unknown profile field / `Shrink.IdentifyTailObject` undefined.

- [ ] **Step 3: Add the fields and carry-through**

`internal/ddl/manifest.go` — add to `Shrink`:

```go
IdentifyTailObject bool `yaml:"identify_tail_object,omitempty"` // run the tail-object walk at shrink start
```

`internal/maint/profile.go` — add to the `Shrink` profile struct (near `MaxBlockMinutes`):

```go
IdentifyTailObject bool `yaml:"identify_tail_object"` // set identify_tail_object on the generated shrink op
```

`cmd/sqlgopace/shrink_plan.go` — in `shrinkManifest`, after building `shrink` and before appending, set it:

```go
shrink.IdentifyTailObject = s.IdentifyTailObject
```

- [ ] **Step 4: Flip the Task-6 placeholder (if used)**

If Task 6 Step 4 used a `false` placeholder for `proactive`, change it to
`op.IdentifyTailObject` now.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test -race ./internal/ddl ./internal/maint ./cmd/sqlgopace ./internal/run`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/ddl/manifest.go internal/maint/profile.go cmd/sqlgopace/shrink_plan.go cmd/sqlgopace/shrink_plan_test.go internal/run/shrink.go
git commit -m "feat(ddl,maint): identify_tail_object flag on shrink op + planner carry-through"
```

---

### Task 8: Final wiring — pass the detected major version + sidecar header + version bump

Turn the feature on in production by feeding the detected SQL major version into the shrink runners, extend the sidecar header comment to explain the two confirmation kinds, and bump the version.

**Files:**
- Modify: `cmd/sqlgopace/main.go` (`SQLMajorVersion: info.MajorVersion` on both `ShrinkRunnerConfig` literals, ~line 437 and ~457)
- Modify: `internal/run/contended.go` (`contendedHeader` text)
- Modify: `internal/version/VERSION`
- Test: manual `go run` / existing suite (this is wiring + copy)

**Interfaces:** none new.

- [ ] **Step 1: Pass the major version into both runners**

In `cmd/sqlgopace/main.go`, add `SQLMajorVersion: info.MajorVersion,` to the `ShrinkRunnerConfig{...}` literal at ~line 437 (the primary shrink runner) and the one at ~line 457 (the tempdb runner — harmless there, tempdb never walks, but keep it uniform). `info` is the `conn.Detect(ctx)` result already in scope.

- [ ] **Step 2: Extend the sidecar header comment**

In `internal/run/contended.go`, update `contendedHeader` to explain both kinds:

```go
const contendedHeader = `# Contended-object capture for %s
# Objects this shrink could not get past, by two confirmation kinds:
#   confirmed_by: lock          — held a Sch-M lock on the object while blocking other
#                                 sessions (empirical, partial: the shrink stops at the first).
#   confirmed_by: tail_position — owns the file's last allocated page (the tail the shrink
#                                 must relocate past), found by the backward page walk.
# Feed this to the planner:  sqlgopace plan --confirmed <this file>
`
```

- [ ] **Step 3: Bump the version**

Edit `internal/version/VERSION` — bump the patch/minor per the project's scheme (e.g. `0.8.0` → `0.9.0` for a feature). Confirm the current value first (`cat internal/version/VERSION`).

- [ ] **Step 4: Build and run the full suite**

Run: `go build ./... && make test && make lint`
Expected: build OK, all tests pass with `-race`, lint clean. (Stop any running `bin/sqlgopace.exe` on Windows before `make build`.)

- [ ] **Step 5: Commit**

```bash
git add cmd/sqlgopace/main.go internal/run/contended.go internal/version/VERSION
git commit -m "feat: wire tail-object identification (major version) + sidecar header + version bump"
```

---

## Post-implementation

- Run `/simplify` over the full diff before merging (project convention — collapse any duplication that accreted, e.g. the two `maybeCaptureTail` give-up call sites if a cleaner single point emerges).
- Consider adding a short note to `README.md` (manifest reference) and `specs/SHRINK.md` documenting `identify_tail_object` and the `confirmed_by: tail_position` sidecar entries — the README is the canonical user-facing reference.
- If a 2019+ throwaway DB is available, run `make integration` to exercise `FindTailObject` end-to-end.

## Notes on execution order

Tasks 1–5 are independent enough to land in order without activating the feature. Task 6
depends on 1, 2, and 5. **Task 7 is cleanest done immediately before Task 6 Step 4** (so
`op.IdentifyTailObject` exists); if you execute strictly in listed order, use the documented
`false` placeholder in Task 6 and flip it in Task 7. Task 8 activates everything and must be
last.
