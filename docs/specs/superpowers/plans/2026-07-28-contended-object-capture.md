# Contended-object capture + `plan --confirmed` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record the object a shrink holds a `Sch-M` lock on while it blocks other sessions (the empirically confirmed tail blocker) into a machine `.contended.yaml` sidecar, and let `plan --confirmed <path>` prioritize / confirm those objects in the pre-shrink reorganize pass.

**Architecture:** A new `internal/mssql` read (`HeldObjectLocks`) is called from the engine's existing reaction `sink` — only for `ddl.Shrink` ops — at the instant it already captures blocked sessions (the `Sch-M` is still held there). A run-side accumulator dedups objects across reactions and writes `.contended.yaml`. On the planner side, `DecidePreShrink` gains a confirmed set that reorders/adds/annotates reorganizes and upgrades matching heap advisories to CONFIRMED; `plan` gains a `--confirmed` flag that loads and guards the sidecar.

**Tech Stack:** Go, `gopkg.in/yaml.v3` (hand-built `yaml.Node` trees in `ddl/render.go`), `sys.dm_tran_locks`. Tests via `go test -race`, no DB except the `integration`-tagged read.

## Global Constraints

- **Idiomatic Go, KISS.** No new layers/interfaces/generics beyond what a task needs. Match surrounding code.
- **English only** — all code, comments, identifiers, file names.
- **Manifest-driven, never raw SQL.** No user-SQL parsing.
- **No query timeout** around executing DDL.
- **`make test` needs no database.** DB-touching code goes behind interfaces; real reads are `integration`-tagged (`//go:build integration`) and skipped unless `SQLGOPACE_TEST_DSN` is set.
- **Lint:** golangci-lint v2 (`.golangci.yml`). US spelling in comments/identifiers.
- **Version** lives in `internal/version/VERSION` (do not bump in this plan).
- **Advisory sidecars are never read back by the engine.** `plan` reads only the machine `.contended.yaml`, never the human `.blocked.yaml`.
- **Design spec:** `docs/specs/superpowers/specs/2026-07-28-contended-object-capture-design.md` is the source of truth.

---

### Task 1: `HeldObjectLocks` read (`internal/mssql`)

Read the user objects a session holds a granted `Sch-M` lock on, resolved to `schema.table`, with a `NULL`-name fallback to `object_id` only. The row→struct mapping is a pure helper so it can be unit-tested; the DB round-trip is `integration`-tagged.

**Files:**
- Modify: `internal/mssql/dmv.go` (add `LockedObject`, `heldObjectLocksSQL`, `scanLockedObject`, `(*Conn).HeldObjectLocks`)
- Test: `internal/mssql/dmv_test.go` (unit test for `scanLockedObject` NULL handling)
- Test: `internal/mssql/held_locks_integration_test.go` (new, `//go:build integration`)

**Interfaces:**
- Produces: `type LockedObject struct { ObjectID int64; Schema, Table, Mode string }` and `func (c *Conn) HeldObjectLocks(ctx context.Context, spid int) ([]LockedObject, error)`.

- [ ] **Step 1: Write the failing unit test for the scan helper**

Add to `internal/mssql/dmv_test.go`:

```go
func TestScanLockedObjectNullNameFallsBackToID(t *testing.T) {
	// A dropped object resolves OBJECT_NAME/OBJECT_SCHEMA_NAME to NULL; keep the id.
	got := scanLockedObject(261575970, sql.NullString{}, sql.NullString{}, "Sch-M")
	want := LockedObject{ObjectID: 261575970, Schema: "", Table: "", Mode: "Sch-M"}
	if got != want {
		t.Errorf("scanLockedObject NULL name = %+v, want %+v", got, want)
	}

	got = scanLockedObject(42, sql.NullString{String: "dbo", Valid: true},
		sql.NullString{String: "MEASUREMENT", Valid: true}, "Sch-M")
	want = LockedObject{ObjectID: 42, Schema: "dbo", Table: "MEASUREMENT", Mode: "Sch-M"}
	if got != want {
		t.Errorf("scanLockedObject = %+v, want %+v", got, want)
	}
}
```

If `dmv_test.go` lacks the `database/sql` import, add it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mssql -run TestScanLockedObject`
Expected: FAIL — `undefined: scanLockedObject` / `undefined: LockedObject`.

- [ ] **Step 3: Implement the type, SQL, helper, and read**

Add to `internal/mssql/dmv.go`:

```go
// LockedObject is one user object a session holds a granted Sch-M lock on — for a
// shrink, the object it is relocating and holding other sessions up on. Schema/Table
// are empty when the object could not be name-resolved (dropped between lock and read);
// ObjectID always identifies it.
type LockedObject struct {
	ObjectID int64
	Schema   string
	Table    string
	Mode     string
}

const heldObjectLocksSQL = `
SELECT l.resource_associated_entity_id,
       OBJECT_SCHEMA_NAME(l.resource_associated_entity_id, l.resource_database_id),
       OBJECT_NAME(l.resource_associated_entity_id, l.resource_database_id),
       l.request_mode
FROM sys.dm_tran_locks l
WHERE l.request_session_id = @spid
  AND l.resource_type   = 'OBJECT'
  AND l.request_status  = 'GRANT'
  AND l.request_mode LIKE 'Sch-M%';`

// scanLockedObject maps one dm_tran_locks row to a LockedObject, keeping only the
// object_id when the name did not resolve.
func scanLockedObject(objectID int64, schema, table sql.NullString, mode string) LockedObject {
	return LockedObject{ObjectID: objectID, Schema: schema.String, Table: table.String, Mode: mode}
}

// HeldObjectLocks returns the user objects spid currently holds a granted Sch-M lock on.
// Best-effort: the caller treats an error as "nothing captured this snapshot".
func (c *Conn) HeldObjectLocks(ctx context.Context, spid int) ([]LockedObject, error) {
	rows, err := c.pool.QueryContext(ctx, heldObjectLocksSQL, sql.Named("spid", spid))
	if err != nil {
		return nil, fmt.Errorf("query held object locks: %w", err)
	}
	defer rows.Close()

	var out []LockedObject
	for rows.Next() {
		var (
			objectID      int64
			schema, table sql.NullString
			mode          string
		)
		if err := rows.Scan(&objectID, &schema, &table, &mode); err != nil {
			return nil, fmt.Errorf("scan held object lock: %w", err)
		}
		out = append(out, scanLockedObject(objectID, schema, table, mode))
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run the unit test to verify it passes**

Run: `go test ./internal/mssql -run TestScanLockedObject`
Expected: PASS.

- [ ] **Step 5: Add the integration test**

Create `internal/mssql/held_locks_integration_test.go`:

```go
//go:build integration

package mssql

import (
	"context"
	"testing"
)

// TestHeldObjectLocksEmptyForIdleSession verifies the read runs and returns nothing for
// a session holding no Sch-M lock (the shape/permission smoke test; the populated case
// is exercised by the shrink e2e).
func TestHeldObjectLocksEmptyForIdleSession(t *testing.T) {
	conn := openTestConn(t) // existing integration helper
	defer conn.Close()
	spid, err := conn.SPID(context.Background())
	if err != nil {
		t.Fatalf("SPID: %v", err)
	}
	got, err := conn.HeldObjectLocks(context.Background(), spid)
	if err != nil {
		t.Fatalf("HeldObjectLocks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("idle session holds %d Sch-M object locks, want 0", len(got))
	}
}
```

If the integration helper is named differently, mirror the pattern in the nearest existing `*_integration_test.go` in `internal/mssql` (open a `*Conn`, read `SPID`). Adjust the helper/method names to what exists; keep the assertion.

- [ ] **Step 6: Verify build of both unit and integration tags**

Run: `go vet ./internal/mssql && go test ./internal/mssql -run TestScanLockedObject`
Then: `go build -tags integration ./internal/mssql`
Expected: both succeed (integration test compiles; unit test passes).

- [ ] **Step 7: Commit**

```bash
git add internal/mssql/dmv.go internal/mssql/dmv_test.go internal/mssql/held_locks_integration_test.go
git commit -m "feat(mssql): HeldObjectLocks reads Sch-M object locks held by a session"
```

---

### Task 2: `.contended.yaml` machine schema + parser (`internal/maint`)

Define the shared machine document and a pure parser. `plan` uses this to read the sidecar; the run-side writer (Task 3) produces bytes that parse back through it (guarded by a round-trip test in Task 3).

**Files:**
- Create: `internal/maint/contended.go`
- Test: `internal/maint/contended_test.go`

**Interfaces:**
- Produces:
  - `type ContendedObject struct { ObjectID int64; Schema, Table, LockMode string; TimesBlocked int; FirstSeen, LastSeen string }`
  - `type ContendedDoc struct { Database string; Observed []ContendedObject }`
  - `func ParseContended(data []byte) (ContendedDoc, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/maint/contended_test.go`:

```go
package maint

import "testing"

func TestParseContendedValid(t *testing.T) {
	data := []byte(`
database: PRODDB
observed:
  - object_id: 261575970
    schema: dbo
    table: MEASUREMENT
    lock_mode: Sch-M
    times_blocked: 3
    first_seen: "2026-07-28T11:10:09Z"
    last_seen: "2026-07-28T11:19:09Z"
`)
	doc, err := ParseContended(data)
	if err != nil {
		t.Fatalf("ParseContended: %v", err)
	}
	if doc.Database != "PRODDB" || len(doc.Observed) != 1 {
		t.Fatalf("doc = %+v", doc)
	}
	o := doc.Observed[0]
	if o.ObjectID != 261575970 || o.Table != "MEASUREMENT" || o.TimesBlocked != 3 {
		t.Errorf("object = %+v", o)
	}
}

func TestParseContendedRejectsUnknownField(t *testing.T) {
	if _, err := ParseContended([]byte("database: X\nbogus: 1\n")); err == nil {
		t.Fatal("expected error on unknown field")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maint -run TestParseContended`
Expected: FAIL — `undefined: ParseContended`.

- [ ] **Step 3: Implement the schema and parser**

Create `internal/maint/contended.go`:

```go
package maint

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ContendedObject is one user object a shrink held a Sch-M lock on while blocking other
// sessions — an empirically confirmed tail blocker. Read back by `plan --confirmed`.
type ContendedObject struct {
	ObjectID     int64  `yaml:"object_id"`
	Schema       string `yaml:"schema"`
	Table        string `yaml:"table"`
	LockMode     string `yaml:"lock_mode"`
	TimesBlocked int    `yaml:"times_blocked"`
	FirstSeen    string `yaml:"first_seen"`
	LastSeen     string `yaml:"last_seen"`
}

// ContendedDoc is the machine body of a .contended.yaml sidecar.
type ContendedDoc struct {
	Database string            `yaml:"database"`
	Observed []ContendedObject `yaml:"observed"`
}

// ParseContended decodes a .contended.yaml sidecar, rejecting unknown fields so a
// malformed file fails loudly rather than silently dropping data.
func ParseContended(data []byte) (ContendedDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc ContendedDoc
	if err := dec.Decode(&doc); err != nil {
		return ContendedDoc{}, fmt.Errorf("parse contended sidecar: %w", err)
	}
	return doc, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/maint -run TestParseContended`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/maint/contended.go internal/maint/contended_test.go
git commit -m "feat(maint): .contended.yaml schema + ParseContended"
```

---

### Task 3: Run-side accumulator + renderer (`internal/run`)

Accumulate distinct held objects across reactions and render `.contended.yaml` (commented header + `maint.ContendedDoc` body). Mirrors `capture.go`'s `blockerCapture`/`renderCapture`/`flushCapture`/`relocateCapture`.

**Files:**
- Create: `internal/run/contended.go`
- Test: `internal/run/contended_test.go`

**Interfaces:**
- Consumes: `mssql.LockedObject` (Task 1), `maint.ContendedDoc`/`ContendedObject` + `maint.ParseContended` (Task 2, test only).
- Produces:
  - `type contendedCapture struct { ... }` with `func (c *contendedCapture) add(o mssql.LockedObject, now string)`, `func (c *contendedCapture) len() int`, `func (c *contendedCapture) doc(database string) maint.ContendedDoc`.
  - `const contendedCaptureSuffix = ".contended.yaml"`
  - `func renderContended(name, database string, acc *contendedCapture) []byte`

- [ ] **Step 1: Write the failing tests**

Create `internal/run/contended_test.go`:

```go
package run

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestContendedCaptureDedupsAndCounts(t *testing.T) {
	var acc contendedCapture
	obj := mssql.LockedObject{ObjectID: 100, Schema: "dbo", Table: "MEASUREMENT", Mode: "Sch-M"}
	acc.add(obj, "2026-07-28T11:10:09Z")
	acc.add(obj, "2026-07-28T11:14:29Z") // same object, later snapshot
	acc.add(mssql.LockedObject{ObjectID: 200, Schema: "dbo", Table: "OTHER", Mode: "Sch-M"}, "2026-07-28T11:14:29Z")

	doc := acc.doc("PRODDB")
	if len(doc.Observed) != 2 {
		t.Fatalf("observed = %d, want 2", len(doc.Observed))
	}
	first := doc.Observed[0] // first-seen order
	if first.ObjectID != 100 || first.TimesBlocked != 2 {
		t.Errorf("first = %+v, want id 100 times_blocked 2", first)
	}
	if first.FirstSeen != "2026-07-28T11:10:09Z" || first.LastSeen != "2026-07-28T11:14:29Z" {
		t.Errorf("first seen/last = %q/%q", first.FirstSeen, first.LastSeen)
	}
}

func TestRenderContendedRoundTrips(t *testing.T) {
	var acc contendedCapture
	acc.add(mssql.LockedObject{ObjectID: 261575970, Schema: "dbo", Table: "MEASUREMENT", Mode: "Sch-M"}, "2026-07-28T11:10:09Z")

	out := renderContended("020_shrink.yaml", "PRODDB", &acc)
	if !strings.HasPrefix(string(out), "# Contended-object capture") {
		t.Errorf("missing comment header:\n%s", out)
	}
	doc, err := maint.ParseContended(out) // guards format drift against the parser
	if err != nil {
		t.Fatalf("ParseContended(renderContended output): %v", err)
	}
	if doc.Database != "PRODDB" || len(doc.Observed) != 1 || doc.Observed[0].ObjectID != 261575970 {
		t.Errorf("round-tripped doc = %+v", doc)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/run -run 'TestContendedCapture|TestRenderContended'`
Expected: FAIL — `undefined: contendedCapture` / `renderContended`.

- [ ] **Step 3: Implement the accumulator and renderer**

Create `internal/run/contended.go`:

```go
package run

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// contendedCaptureSuffix names the machine sidecar written next to a shrink manifest.
const contendedCaptureSuffix = ".contended.yaml"

type capturedObject struct {
	obj       mssql.LockedObject
	firstSeen string
	lastSeen  string
	count     int
}

// contendedCapture accumulates the distinct objects a shrink held a Sch-M lock on across
// its reactions, in first-seen order, keyed by object_id.
type contendedCapture struct {
	order []int64
	byID  map[int64]*capturedObject
}

func (c *contendedCapture) add(o mssql.LockedObject, now string) {
	if c.byID == nil {
		c.byID = make(map[int64]*capturedObject)
	}
	e, ok := c.byID[o.ObjectID]
	if !ok {
		e = &capturedObject{obj: o, firstSeen: now}
		c.byID[o.ObjectID] = e
		c.order = append(c.order, o.ObjectID)
	}
	e.lastSeen = now
	e.count++
}

func (c *contendedCapture) len() int { return len(c.order) }

// doc builds the machine document in first-seen order.
func (c *contendedCapture) doc(database string) maint.ContendedDoc {
	doc := maint.ContendedDoc{Database: database}
	for _, id := range c.order {
		e := c.byID[id]
		doc.Observed = append(doc.Observed, maint.ContendedObject{
			ObjectID: e.obj.ObjectID, Schema: e.obj.Schema, Table: e.obj.Table,
			LockMode: e.obj.Mode, TimesBlocked: e.count,
			FirstSeen: e.firstSeen, LastSeen: e.lastSeen,
		})
	}
	return doc
}

const contendedHeader = `# Contended-object capture for %s
# Objects this shrink held a Sch-M lock on while blocking other sessions —
# i.e. the objects it was relocating and could not get past. These are
# EMPIRICALLY CONFIRMED tail blockers (partial: the shrink stops at the first).
# Feed this to the planner:  sqlgopace plan --confirmed <this file>
`

// renderContended builds the sidecar: a commented human header + the machine body.
func renderContended(name, database string, acc *contendedCapture) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, contendedHeader, name)
	body, err := yaml.Marshal(acc.doc(database))
	if err != nil {
		// yaml.Marshal of this fixed struct cannot fail; keep the header if it ever does.
		return []byte(b.String())
	}
	b.Write(body)
	return []byte(b.String())
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/run -run 'TestContendedCapture|TestRenderContended'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/run/contended.go internal/run/contended_test.go
git commit -m "feat(run): contended-object accumulator + .contended.yaml renderer"
```

---

### Task 4: Wire capture into the engine (shrink-only) + relocate on finalize

Extend `BlockerReader` with `HeldObjectLocks`, add `captureContended`, call it from the reaction `sink` only for `ddl.Shrink`, and relocate the sidecar on finalize.

**Files:**
- Modify: `internal/run/engine.go` (interface `BlockerReader` ~line 116; `Engine` field for the accumulator per-manifest; `sink` block ~lines 597-624; finalize/relocate path ~near `relocateCapture`)
- Modify: `internal/run/capture.go` (add `relocateContended`, mirroring `relocateCapture`)
- Modify: `internal/run/engine_test.go` (fake `fakeBlockerReader` gains `HeldObjectLocks`)

**Interfaces:**
- Consumes: `contendedCapture`, `renderContended`, `contendedCaptureSuffix` (Task 3); `mssql.LockedObject` (Task 1).
- Produces: `func (e *Engine) captureContended(ctx context.Context, spid int, acc *contendedCapture, name, database string)`; `func (e *Engine) relocateContended(name, dir string)`.

- [ ] **Step 1: Write the failing injection-gate test**

Add to `internal/run/engine_test.go` (adapt to the existing test harness for `ProcessAll`; reuse the pattern from `TestProcessAllCapturesBlockedSessions`):

```go
func TestProcessAllCapturesContendedObjectsForShrinkOnly(t *testing.T) {
	// A shrink whose sink fires a pause while we hold a Sch-M lock writes .contended.yaml.
	held := []mssql.LockedObject{{ObjectID: 100, Schema: "dbo", Table: "MEASUREMENT", Mode: "Sch-M"}}
	dir := t.TempDir()
	// Build an engine whose blocker reader reports one held object, running a manifest
	// with a single shrink op through a fake shrink driver that emits one "pause".
	// (Mirror the existing shrink-path engine test setup; wire WithShrinkRunner to a fake
	// that calls sink(ReactionEvent{Kind:"pause"}) once, then returns an incomplete result.)
	runShrinkManifest(t, dir, held) // helper below or inline per existing patterns

	got := readFile(t, filepath.Join(dir /* done or failed */, "050_shrink_db_data.yaml"+contendedCaptureSuffix))
	if !strings.Contains(got, "MEASUREMENT") || !strings.Contains(got, "object_id: 100") {
		t.Errorf("contended sidecar missing held object:\n%s", got)
	}
}

func TestProcessAllSkipsContendedForNonShrink(t *testing.T) {
	// The same held-object reader + a pause on a rebuild_index op must NOT write a sidecar.
	held := []mssql.LockedObject{{ObjectID: 100, Schema: "dbo", Table: "T", Mode: "Sch-M"}}
	dir := t.TempDir()
	runRebuildManifestWithPause(t, dir, held) // mirror existing monitored-runner engine test
	if fileExists(filepath.Join(dir, "010_rebuild.yaml"+contendedCaptureSuffix)) {
		t.Error("wrote a contended sidecar for a non-shrink operation")
	}
}
```

Use the existing engine-test scaffolding (fakes for `BlockerReader`, `ShrinkDriver`, queue dirs). The two assertions that matter: shrink writes the sidecar with the held object; a non-shrink op does not.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/run -run 'TestProcessAllCapturesContended|TestProcessAllSkipsContended'`
Expected: FAIL (compile error until `HeldObjectLocks` is on the interface, then assertion failure until wired).

- [ ] **Step 3: Extend `BlockerReader` and the fake**

In `internal/run/engine.go`, extend the interface (~line 116):

```go
type BlockerReader interface {
	ActiveSessions(ctx context.Context) ([]mssql.Session, error)
	HeldObjectLocks(ctx context.Context, spid int) ([]mssql.LockedObject, error)
}
```

In `internal/run/engine_test.go`, add to `fakeBlockerReader`:

```go
func (f fakeBlockerReader) HeldObjectLocks(context.Context, int) ([]mssql.LockedObject, error) {
	return f.held, nil // add a `held []mssql.LockedObject` field to the fake
}
```

(Also add the `held` field and set it in the test constructors that need it; default nil is fine elsewhere.)

- [ ] **Step 4: Add `captureContended` and `relocateContended`**

In `internal/run/contended.go` add the engine methods (or place `captureContended` next to `captureBlockers` in `capture.go` for symmetry — keep both relocate helpers together in `capture.go`):

```go
// captureContended records the objects our shrink (spid) currently holds a Sch-M lock
// on, into acc, and flushes the sidecar. Best-effort: a nil reader or a read error is a
// no-op. Called only for shrink operations.
func (e *Engine) captureContended(ctx context.Context, spid int, acc *contendedCapture, name, database string) {
	if e.blockers == nil || spid == 0 {
		return
	}
	held, err := e.blockers.HeldObjectLocks(ctx, spid)
	if err != nil {
		return
	}
	now := e.now()
	for _, o := range held {
		acc.add(o, now)
	}
	if acc.len() > 0 {
		path := filepath.Join(e.dirs.Processing, name+contendedCaptureSuffix)
		if err := os.WriteFile(path, renderContended(name, database, acc), 0o644); err != nil {
			fmt.Fprintf(e.out, "write contended capture %s: %v\n", name, err)
		}
	}
}
```

In `internal/run/capture.go`, add next to `relocateCapture`:

```go
// relocateContended moves the .contended.yaml sidecar from processing to dir on finalize.
func (e *Engine) relocateContended(name, dir string) {
	src := filepath.Join(e.dirs.Processing, name+contendedCaptureSuffix)
	if _, err := os.Stat(src); err != nil {
		return
	}
	if err := os.Rename(src, filepath.Join(dir, name+contendedCaptureSuffix)); err != nil {
		fmt.Fprintf(e.out, "relocate contended capture %s: %v\n", name, err)
	}
}
```

- [ ] **Step 5: Call the capture from the `sink`, gated on `ddl.Shrink`**

In `internal/run/engine.go`, inside `processOne`, declare a per-manifest accumulator alongside `captured := &blockerCapture{}` (~line 550):

```go
contended := &contendedCapture{}
```

In the `sink` (~line 606-613), after the existing `captureBlockers` call, add the shrink-only capture:

```go
if capture {
	blocked = e.captureBlockers(ctx, ignore, captured, name)
	if _, isShrink := step.Operation.(ddl.Shrink); isShrink {
		e.captureContended(ctx, e.session.SPID(), contended, name, manifest.Database)
	}
	if blocked > 0 {
		detail = fmt.Sprintf("%s; blocking %d session(s)", detail, blocked)
	}
}
```

(`step` and `manifest` are in scope in `processOne`. If `manifest.Database` is empty for a shrink, fall back to `opTarget`/the connected DB name already used elsewhere; the shrink manifest always carries `database:`.)

- [ ] **Step 6: Relocate the sidecar on finalize**

Find where `relocateCapture(name, dir)` is called at the end of `processOne` (the terminal `03.done`/`04.failed` routing) and add beside it:

```go
e.relocateContended(name, dir)
```

- [ ] **Step 7: Run tests**

Run: `go test -race ./internal/run -run 'TestProcessAllCapturesContended|TestProcessAllSkipsContended|TestProcessAllCapturesBlockedSessions'`
Expected: PASS (new tests pass; the existing blocked-session test still passes).

- [ ] **Step 8: Full package build + vet**

Run: `go vet ./internal/run && go test -race ./internal/run`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/run/engine.go internal/run/capture.go internal/run/contended.go internal/run/engine_test.go
git commit -m "feat(run): capture contended objects for shrink reactions + relocate on finalize"
```

---

### Task 5: `.log` pointer line at INCOMPLETE

Add a one-line pointer to the run report when a shrink captured contended objects.

**Files:**
- Modify: `internal/report/report.go` (the report struct + text renderer — locate the operation/report rendering that emits the `waits`/`shrink` lines)
- Modify: `internal/run/engine.go` (populate the new report field from `contended.len()`)
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `contendedCapture.len()` (Task 3), `contendedCaptureSuffix` (Task 3).
- Produces: a `ContendedCount int` and `ContendedFile string` field on the per-operation report struct (names to match the existing report field style), rendered as one line.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go` (mirror an existing report-rendering test that checks a substring of the text output):

```go
func TestReportRendersContendedPointer(t *testing.T) {
	rep := Report{ /* minimal INCOMPLETE report with one operation */ }
	rep.Operations = []OperationReport{{
		Index: 1, CommandType: "shrink_data", Target: "PRODDB",
		ContendedCount: 2, ContendedFile: "020_shrink.yaml.contended.yaml",
	}}
	out := renderText(rep) // use whatever the package's text renderer is named
	if !strings.Contains(out, "contended objects: 2 — see 020_shrink.yaml.contended.yaml") {
		t.Errorf("missing contended pointer line:\n%s", out)
	}
}
```

Match the actual `Report`/`OperationReport` type names and text-render entry point in the package.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/report -run TestReportRendersContendedPointer`
Expected: FAIL — unknown field `ContendedCount` / missing line.

- [ ] **Step 3: Add the fields and render the line**

Add `ContendedCount int` and `ContendedFile string` to the per-operation report struct. In the text renderer, where the operation's `waits`/`shrink` block is emitted, add:

```go
if op.ContendedCount > 0 {
	fmt.Fprintf(w, "      contended objects: %d — see %s\n", op.ContendedCount, op.ContendedFile)
}
```

Add the equivalent field to the machine-JSON struct if the report emits JSON per operation (mirror how `PeakBlocked` is carried), so the JSON stays consistent.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/report -run TestReportRendersContendedPointer`
Expected: PASS.

- [ ] **Step 5: Populate the field in the engine**

In `internal/run/engine.go`, where the per-operation report is assembled (the block that sets `PeakBlocked: peakBlocked` ~line 721), add:

```go
ContendedCount: contended.len(),
ContendedFile:  name + contendedCaptureSuffix,
```

Set `ContendedFile` to empty when `contended.len() == 0` so the pointer never names a file that was not written (guard inline: only set the file when count > 0, or leave the renderer's `> 0` check to gate it — the renderer already does).

- [ ] **Step 6: Build, vet, test**

Run: `go vet ./internal/report ./internal/run && go test -race ./internal/report ./internal/run`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go internal/run/engine.go
git commit -m "feat(report): pointer line to .contended.yaml on shrink runs"
```

---

### Task 6: Measurements carry `ObjectID`

Add `ObjectID` to the two pre-shrink measurement structs and populate it (free — `head.ObjectID` is already read), so the confirmed set can join by object id.

**Files:**
- Modify: `internal/maint/shrink.go` (`ShrinkIndexMeasurement`, `ShrinkHeapMeasurement`)
- Modify: `internal/plan/shrink.go` (populate `ObjectID: head.ObjectID` in both measurement builders)
- Test: `internal/plan/shrink_test.go` (assert the built measurement carries the object id)

**Interfaces:**
- Produces: `ShrinkIndexMeasurement.ObjectID int64`, `ShrinkHeapMeasurement.ObjectID int64`.

- [ ] **Step 1: Write the failing test**

Add to `internal/plan/shrink_test.go` (extend the existing measurement-building test; use its fake `Reader`):

```go
func TestPreShrinkMeasurementCarriesObjectID(t *testing.T) {
	// Using the existing fake reader/inventory in this test file, build the measurements
	// and assert the index/heap measurement's ObjectID matches the inventory head.
	got := buildIndexMeasurementForTest(t) // reuse the file's existing helper/flow
	if got.ObjectID == 0 {
		t.Errorf("index measurement ObjectID = 0, want the inventory object id")
	}
}
```

Match the existing test scaffolding in `shrink_test.go`; the assertion is simply that `ObjectID` is populated (non-zero for the fixture's object).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/plan -run TestPreShrinkMeasurementCarriesObjectID`
Expected: FAIL — unknown field `ObjectID` (then zero value once the field exists but before it is populated).

- [ ] **Step 3: Add and populate the field**

In `internal/maint/shrink.go`:

```go
type ShrinkIndexMeasurement struct {
	ObjectID                int64
	Schema, Table, Index    string
	PageCount               int64
	AvgPageSpaceUsedPercent float64
}

type ShrinkHeapMeasurement struct {
	ObjectID                int64
	Schema, Table           string
	SizeMB                  int64
	ForwardedRecordPercent  float64
	AvgPageSpaceUsedPercent float64
}
```

In `internal/plan/shrink.go`, in both `maint.ShrinkIndexMeasurement{...}` (~line 63) and `maint.ShrinkHeapMeasurement{...}` (~line 98) literals, add `ObjectID: head.ObjectID,`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/plan -run TestPreShrinkMeasurementCarriesObjectID`
Expected: PASS.

- [ ] **Step 5: Build + vet (DecidePreShrink still compiles)**

Run: `go vet ./internal/maint ./internal/plan && go test -race ./internal/maint ./internal/plan`
Expected: PASS (adding a field does not break existing `DecidePreShrink` tests).

- [ ] **Step 6: Commit**

```bash
git add internal/maint/shrink.go internal/plan/shrink.go internal/plan/shrink_test.go
git commit -m "feat(maint): pre-shrink measurements carry ObjectID for confirmed-set join"
```

---

### Task 7: `DecidePreShrink` augment + HeapAdvisory confirmation (`internal/maint`)

Extend `DecidePreShrink` with a confirmed set (object_id → times_blocked) that reorders confirmed reorganizes to the head, adds a reorganize for a confirmed-but-dense index-bearing object, annotates confirmed reorganizes, and marks matching heap advisories CONFIRMED. Annotations travel as a parallel notes slice.

**Files:**
- Modify: `internal/maint/shrink.go` (`HeapAdvisory` fields; `PreShrinkPlan` notes; `DecidePreShrink` signature + logic)
- Modify: `internal/maint/shrink_test.go`
- Modify callers to pass `nil` for now: `internal/plan/shrink.go` (`AnalyzePreShrink` returns the plan; the confirmed set is threaded in Task 9 — for this task, add a `confirmed` parameter defaulting to `nil` at the `DecidePreShrink` call site).

**Interfaces:**
- Consumes: `ShrinkIndexMeasurement.ObjectID`, `ShrinkHeapMeasurement.ObjectID` (Task 6).
- Produces:
  - `HeapAdvisory` gains `Confirmed bool` and `TimesBlocked int`.
  - `PreShrinkPlan` gains `ReorganizeNotes []string` (parallel to `Reorganizes`; empty string = no note).
  - `func DecidePreShrink(indexes []ShrinkIndexMeasurement, heaps []ShrinkHeapMeasurement, p *Profile, confirmed map[int64]int) PreShrinkPlan` (new trailing `confirmed` param; `nil` = today's behavior).

- [ ] **Step 1: Write the failing tests**

Add to `internal/maint/shrink_test.go`:

```go
func TestDecidePreShrinkNilConfirmedUnchanged(t *testing.T) {
	// Baseline: with nil confirmed, output equals the pre-existing behavior.
	idx := []ShrinkIndexMeasurement{{ObjectID: 1, Schema: "dbo", Table: "A", Index: "IX", PageCount: 5000, AvgPageSpaceUsedPercent: 40}}
	p := profileWithShrink(70, 1000) // helper: threshold 70, page_count_floor 1000
	pl := DecidePreShrink(idx, nil, p, nil)
	if len(pl.Reorganizes) != 1 || len(pl.ReorganizeNotes) != 1 || pl.ReorganizeNotes[0] != "" {
		t.Fatalf("baseline = %+v notes %v", pl.Reorganizes, pl.ReorganizeNotes)
	}
}

func TestDecidePreShrinkConfirmedReordersToHead(t *testing.T) {
	idx := []ShrinkIndexMeasurement{
		{ObjectID: 1, Schema: "dbo", Table: "A", Index: "IXA", PageCount: 5000, AvgPageSpaceUsedPercent: 40},
		{ObjectID: 2, Schema: "dbo", Table: "B", Index: "IXB", PageCount: 5000, AvgPageSpaceUsedPercent: 40},
	}
	p := profileWithShrink(70, 1000)
	pl := DecidePreShrink(idx, nil, p, map[int64]int{2: 3}) // B confirmed
	if pl.Reorganizes[0].Table != "B" {
		t.Errorf("confirmed B not first: %+v", pl.Reorganizes)
	}
	if pl.ReorganizeNotes[0] != "confirmed blocker (times_blocked=3)" {
		t.Errorf("note = %q", pl.ReorganizeNotes[0])
	}
}

func TestDecidePreShrinkConfirmedDenseAddedDespiteDensity(t *testing.T) {
	// C is DENSE (85% >= threshold 70) so density skips it, but it is confirmed.
	idx := []ShrinkIndexMeasurement{
		{ObjectID: 3, Schema: "dbo", Table: "C", Index: "IXC", PageCount: 5000, AvgPageSpaceUsedPercent: 85},
	}
	p := profileWithShrink(70, 1000)
	pl := DecidePreShrink(idx, nil, p, map[int64]int{3: 1})
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Table != "C" {
		t.Fatalf("dense-confirmed not added: %+v", pl.Reorganizes)
	}
	if pl.ReorganizeNotes[0] != "confirmed blocker — added despite density" {
		t.Errorf("note = %q", pl.ReorganizeNotes[0])
	}
}

func TestDecidePreShrinkConfirmedHeapMarked(t *testing.T) {
	heaps := []ShrinkHeapMeasurement{{ObjectID: 9, Schema: "dbo", Table: "H", SizeMB: 500, AvgPageSpaceUsedPercent: 40}}
	p := profileWithShrink(70, 1000) // heap.min_size_mb small enough in the helper
	pl := DecidePreShrink(nil, heaps, p, map[int64]int{9: 4})
	if len(pl.HeapAdvisories) != 1 || !pl.HeapAdvisories[0].Confirmed || pl.HeapAdvisories[0].TimesBlocked != 4 {
		t.Errorf("heap not marked confirmed: %+v", pl.HeapAdvisories)
	}
}
```

If `profileWithShrink` does not exist, add a small helper in the test file building a `*Profile` with `Shrink.ReorganizeBelowDensityPercent`, `Index.PageCountFloor`, and a low `Heap.MinSizeMB`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/maint -run TestDecidePreShrink`
Expected: FAIL — signature mismatch (`DecidePreShrink` takes no `confirmed`), missing `ReorganizeNotes`/`Confirmed`.

- [ ] **Step 3: Implement the augment**

In `internal/maint/shrink.go`:

```go
type HeapAdvisory struct {
	Schema, Table          string
	SizeMB                 int64
	ForwardedRecordPercent float64
	PageDensityPercent     float64
	Confirmed              bool // observed blocking the shrink (from a .contended.yaml)
	TimesBlocked           int  // reaction snapshots that saw it, when Confirmed
}

type PreShrinkPlan struct {
	Reorganizes     []ddl.ReorganizeIndex
	ReorganizeNotes []string // parallel to Reorganizes; "" = no annotation
	HeapAdvisories  []HeapAdvisory
}

// DecidePreShrink ... confirmed maps an object_id observed blocking a prior shrink to its
// times_blocked count (nil = no capture). Confirmed index-bearing objects are reordered
// to the head and annotated; a confirmed-but-dense object is added anyway; confirmed
// heaps are marked CONFIRMED. It never emits a REBUILD.
func DecidePreShrink(indexes []ShrinkIndexMeasurement, heaps []ShrinkHeapMeasurement, p *Profile, confirmed map[int64]int) PreShrinkPlan {
	var pl PreShrinkPlan
	threshold := p.Shrink.ReorganizeBelowDensityPercent

	type entry struct {
		op    ddl.ReorganizeIndex
		note  string
		conf  int // times_blocked when confirmed, else 0
	}
	var confirmedEntries, rest []entry

	for _, m := range indexes {
		if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
			continue
		}
		tooSmall := m.PageCount < int64(p.Index.PageCountFloor)
		dense := m.AvgPageSpaceUsedPercent >= threshold
		tb, isConfirmed := confirmed[m.ObjectID]

		if tooSmall {
			continue // never reorganize a tiny index, confirmed or not
		}
		op := ddl.ReorganizeIndex{Schema: m.Schema, Table: m.Table, Index: m.Index, LOBCompaction: p.Index.LOBCompaction}
		switch {
		case isConfirmed && dense:
			confirmedEntries = append(confirmedEntries, entry{op, "confirmed blocker — added despite density", tb})
		case isConfirmed:
			confirmedEntries = append(confirmedEntries, entry{op, fmt.Sprintf("confirmed blocker (times_blocked=%d)", tb), tb})
		case dense:
			continue // density skips it and it is not confirmed
		default:
			rest = append(rest, entry{op, "", 0})
		}
	}

	// Confirmed first, by times_blocked desc then original order (stable).
	sort.SliceStable(confirmedEntries, func(i, j int) bool {
		return confirmedEntries[i].conf > confirmedEntries[j].conf
	})
	for _, e := range append(confirmedEntries, rest...) {
		pl.Reorganizes = append(pl.Reorganizes, e.op)
		pl.ReorganizeNotes = append(pl.ReorganizeNotes, e.note)
	}

	for _, m := range heaps {
		if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
			continue
		}
		if m.SizeMB < p.Heap.MinSizeMB || m.AvgPageSpaceUsedPercent >= threshold {
			continue
		}
		tb, isConfirmed := confirmed[m.ObjectID]
		pl.HeapAdvisories = append(pl.HeapAdvisories, HeapAdvisory{
			Schema: m.Schema, Table: m.Table, SizeMB: m.SizeMB,
			ForwardedRecordPercent: m.ForwardedRecordPercent, PageDensityPercent: m.AvgPageSpaceUsedPercent,
			Confirmed: isConfirmed, TimesBlocked: tb,
		})
	}
	return pl
}
```

Add `"fmt"` and `"sort"` to the imports.

- [ ] **Step 4: Update the existing call site to pass `nil`**

In `internal/plan/shrink.go`, the `DecidePreShrink(...)` call gains a trailing `nil` (the confirmed set is threaded in Task 9). If `AnalyzePreShrink` calls it directly, update that call.

- [ ] **Step 5: Run tests**

Run: `go test -race ./internal/maint -run TestDecidePreShrink`
Expected: PASS. Then `go test -race ./internal/maint ./internal/plan` (existing pre-shrink tests still green with the `nil` arg).

- [ ] **Step 6: Commit**

```bash
git add internal/maint/shrink.go internal/maint/shrink_test.go internal/plan/shrink.go
git commit -m "feat(maint): DecidePreShrink augments selection with confirmed blockers"
```

---

### Task 8: Per-operation comment support in the renderer (`internal/ddl`)

Let the manifest renderer attach a YAML `HeadComment` to specific operations, so the planner can annotate confirmed reorganizes without a manifest field.

**Files:**
- Modify: `internal/ddl/render.go` (`MarshalManifest` + new `MarshalManifestAnnotated`)
- Test: `internal/ddl/render_test.go`

**Interfaces:**
- Produces: `func MarshalManifestAnnotated(m *Manifest, opComments map[int]string) ([]byte, error)`; `MarshalManifest(m)` delegates with `nil`.

- [ ] **Step 1: Write the failing test**

Add to `internal/ddl/render_test.go`:

```go
func TestMarshalManifestAnnotatedEmitsComment(t *testing.T) {
	m := &Manifest{
		Database: "PRODDB",
		Operations: []Operation{
			ReorganizeIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "PK"},
		},
	}
	out, err := MarshalManifestAnnotated(m, map[int]string{0: "confirmed blocker (times_blocked=3)"})
	if err != nil {
		t.Fatalf("MarshalManifestAnnotated: %v", err)
	}
	if !strings.Contains(string(out), "# confirmed blocker (times_blocked=3)") {
		t.Errorf("comment not emitted:\n%s", out)
	}
	// The comment must not break round-trip.
	if _, err := ParseManifest(out); err != nil {
		t.Errorf("annotated manifest does not parse: %v", err)
	}
}

func TestMarshalManifestUnannotatedUnchanged(t *testing.T) {
	m := &Manifest{Operations: []Operation{ReorganizeIndex{Schema: "dbo", Table: "A", Index: "IX"}}}
	a, _ := MarshalManifest(m)
	b, _ := MarshalManifestAnnotated(m, nil)
	if string(a) != string(b) {
		t.Errorf("nil-annotated output differs from MarshalManifest")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/ddl -run TestMarshalManifest`
Expected: FAIL — `undefined: MarshalManifestAnnotated`.

- [ ] **Step 3: Implement the annotated marshaller**

In `internal/ddl/render.go`, refactor `MarshalManifest` to delegate:

```go
func MarshalManifest(m *Manifest) ([]byte, error) {
	return MarshalManifestAnnotated(m, nil)
}

// MarshalManifestAnnotated renders the manifest, attaching opComments[i] as a YAML
// head comment on operation i (a "# ..." line above it). Comments are ignored by
// ParseManifest, so annotation never affects round-trip.
func MarshalManifestAnnotated(m *Manifest, opComments map[int]string) ([]byte, error) {
	operations := &yaml.Node{Kind: yaml.SequenceNode}
	for i, op := range m.Operations {
		node, err := operationNode(op)
		if err != nil {
			return nil, fmt.Errorf("operation %d (%s): %w", i, op.CommandType(), err)
		}
		if c := opComments[i]; c != "" {
			node.HeadComment = c
		}
		operations.Content = append(operations.Content, node)
	}
	// ... the remainder of the current MarshalManifest body, unchanged ...
}
```

Move the existing body (root node assembly + encoder) into `MarshalManifestAnnotated` verbatim after the loop above.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/ddl -run TestMarshalManifest`
Expected: PASS.

- [ ] **Step 5: Full package test (round-trip suite still green)**

Run: `go vet ./internal/ddl && go test -race ./internal/ddl`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/ddl/render.go internal/ddl/render_test.go
git commit -m "feat(ddl): MarshalManifestAnnotated attaches per-op YAML comments"
```

---

### Task 9: `plan --confirmed` wiring + heap advisory CONFIRMED rendering (`cmd/sqlgopace`)

Add the flag, load and guard the sidecar, thread the confirmed set through `planShrink`/`shrinkManifest`, emit reorganize annotations, and render confirmed heaps.

**Files:**
- Modify: `cmd/sqlgopace/plan.go` (flag, guard, load, thread; use `MarshalManifestAnnotated` for the shrink manifest)
- Modify: `cmd/sqlgopace/shrink_plan.go` (`planShrink`/`shrinkManifest` signatures; heap advisory CONFIRMED text; carry `ReorganizeNotes` into the write path)
- Test: `cmd/sqlgopace/shrink_plan_test.go`

**Interfaces:**
- Consumes: `maint.ParseContended`/`ContendedDoc` (Task 2), `DecidePreShrink(..., confirmed)` (Task 7), `MarshalManifestAnnotated` (Task 8), `maint.HeapAdvisory.Confirmed/TimesBlocked` (Task 7).
- Produces: `--confirmed` flag; `confirmed map[int64]int` derived from the sidecar; a `namedManifest` whose shrink manifest carries per-op comments.

- [ ] **Step 1: Write the failing tests (guards + annotation)**

Add to `cmd/sqlgopace/shrink_plan_test.go`:

```go
func TestLoadConfirmedRejectsDatabaseMismatch(t *testing.T) {
	doc := maint.ContendedDoc{Database: "OTHER"}
	_, err := confirmedSetFor(doc, "PRODDB")
	if err == nil {
		t.Fatal("expected error on database mismatch")
	}
}

func TestConfirmedSetForBuildsMap(t *testing.T) {
	doc := maint.ContendedDoc{Database: "PRODDB", Observed: []maint.ContendedObject{
		{ObjectID: 100, TimesBlocked: 3}, {ObjectID: 200, TimesBlocked: 1},
	}}
	got, err := confirmedSetFor(doc, "PRODDB")
	if err != nil {
		t.Fatalf("confirmedSetFor: %v", err)
	}
	if got[100] != 3 || got[200] != 1 {
		t.Errorf("map = %v", got)
	}
}

func TestHeapAdvisorySidecarMarksConfirmed(t *testing.T) {
	adv := []maint.HeapAdvisory{{Schema: "dbo", Table: "H", SizeMB: 500, PageDensityPercent: 40, Confirmed: true, TimesBlocked: 4}}
	dir := t.TempDir()
	path, err := writeHeapAdvisorySidecar(dir, "050_shrink_db_data.yaml", "PRODDB", adv)
	if err != nil || path == "" {
		t.Fatalf("writeHeapAdvisorySidecar: %v", err)
	}
	body := readFile(t, path)
	if !strings.Contains(body, "confirmed: true") || !strings.Contains(body, "times_blocked: 4") {
		t.Errorf("heap sidecar missing confirmation:\n%s", body)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/sqlgopace -run 'TestLoadConfirmed|TestConfirmedSetFor|TestHeapAdvisorySidecarMarksConfirmed'`
Expected: FAIL — `undefined: confirmedSetFor`; heap sidecar has no confirmation fields.

- [ ] **Step 3: Add the flag and loader**

In `cmd/sqlgopace/plan.go`, add to the flag block (~line 104):

```go
confirmed = fs.String("confirmed", "", "path to a .contended.yaml from a prior failed shrink; prioritise its objects in the pre-shrink pass")
```

Add a loader/guard helper (in `shrink_plan.go`):

```go
// confirmedSetFor validates a contended doc against the connected database and returns
// the object_id -> times_blocked map. Empty database in the doc, or a mismatch, is an error.
func confirmedSetFor(doc maint.ContendedDoc, db string) (map[int64]int, error) {
	if !strings.EqualFold(doc.Database, db) {
		return nil, fmt.Errorf("--confirmed sidecar is for database %q, connected to %q", doc.Database, db)
	}
	set := make(map[int64]int, len(doc.Observed))
	for _, o := range doc.Observed {
		set[o.ObjectID] = o.TimesBlocked
	}
	return set, nil
}
```

In `plan.go`, after `db` is resolved and before `planShrink`, load the sidecar when the flag is set:

```go
var confirmedSet map[int64]int
if *confirmed != "" {
	if !profile.Shrink.Enabled {
		return errors.New("--confirmed requires shrink.enabled in the maintenance profile")
	}
	data, err := os.ReadFile(*confirmed)
	if err != nil {
		return fmt.Errorf("read --confirmed: %w", err)
	}
	doc, err := maint.ParseContended(data)
	if err != nil {
		return err
	}
	if confirmedSet, err = confirmedSetFor(doc, db); err != nil {
		return err
	}
}
```

- [ ] **Step 4: Thread the confirmed set through `planShrink`/`shrinkManifest`**

Change `planShrink` to accept `confirmed map[int64]int` and pass it to `DecidePreShrink` (inside `AnalyzePreShrink` or after it — the confirmed set augments the returned plan). Simplest: have `AnalyzePreShrink` gather measurements, then call `DecidePreShrink(indexes, heaps, profile, confirmed)`. If `AnalyzePreShrink` currently calls `DecidePreShrink` internally, add a `confirmed` param to it; otherwise call `DecidePreShrink` in `planShrink`.

Skip-with-log for unresolvable ids: after `DecidePreShrink`, the confirmed ids that matched no measurement are those not represented in `pre.Reorganizes`/`HeapAdvisories`. Log them:

```go
matched := map[int64]bool{}
// mark ids that produced a reorganize or heap advisory (compare by object id via the
// measurements you already have in scope), then:
for id := range confirmedSet {
	if !matched[id] {
		fmt.Fprintf(logw, "-- confirmed object %d not found in %s; skipping\n", id, db)
	}
}
```

(Implement `matched` by keeping the measurement object-id set from `AnalyzePreShrink` and checking membership; a confirmed id absent from all measurements is unresolvable.)

Change `shrinkManifest` to also return the per-op comment map so `plan.go` can pass it to `MarshalManifestAnnotated`. The comments come from `pre.ReorganizeNotes` (index-aligned to `pre.Reorganizes`, which occupy manifest operation indices `0..len-1`; the trailing shrink op has no note). Build:

```go
comments := map[int]string{}
for i, note := range pre.ReorganizeNotes {
	if note != "" {
		comments[i] = note
	}
}
```

Store the comments on the `namedManifest` (add a `comments map[int]string` field) or return them alongside. In the manifest write path (`plan.go` ~line 351/371 and `writeManifests`), render the shrink manifest with `MarshalManifestAnnotated(nm.manifest, nm.comments)` and other manifests with `MarshalManifest`. If `namedManifest` gains a `comments` field, have `writeManifests`/`renderManifests` use `MarshalManifestAnnotated(nm.manifest, nm.comments)` uniformly (nil comments for non-shrink manifests → identical output).

- [ ] **Step 5: Render CONFIRMED heaps in the sidecar**

In `cmd/sqlgopace/shrink_plan.go`, extend `heapAdvisoryItem` and the writer:

```go
type heapAdvisoryItem struct {
	Schema                 string  `yaml:"schema"`
	Table                  string  `yaml:"table"`
	SizeMB                 int64   `yaml:"size_mb"`
	ForwardedRecordPercent float64 `yaml:"forwarded_record_percent"`
	PageDensityPercent     float64 `yaml:"page_density_percent"`
	Confirmed              bool    `yaml:"confirmed,omitempty"`
	TimesBlocked           int     `yaml:"times_blocked,omitempty"`
}
```

In `writeHeapAdvisorySidecar`, carry `Confirmed: a.Confirmed, TimesBlocked: a.TimesBlocked` into each item. In `printHeapAdvisory`, append ` [CONFIRMED, times_blocked=N]` to the line when `a.Confirmed`.

- [ ] **Step 6: Run the tests**

Run: `go test ./cmd/sqlgopace -run 'TestLoadConfirmed|TestConfirmedSetFor|TestHeapAdvisorySidecarMarksConfirmed'`
Expected: PASS.

- [ ] **Step 7: Full build, vet, and the whole suite**

Run: `go build ./... && go vet ./... && make test`
Expected: all PASS, no DB needed.

- [ ] **Step 8: Commit**

```bash
git add cmd/sqlgopace/plan.go cmd/sqlgopace/shrink_plan.go cmd/sqlgopace/shrink_plan_test.go
git commit -m "feat(cmd): plan --confirmed prioritises captured blockers + marks confirmed heaps"
```

---

### Task 10: Docs — README + spec cross-reference

Document the flag and the sidecar so the CLI surface stays canonical.

**Files:**
- Modify: `README.md` (the `plan` subcommand section; the sidecar/advisory listing)
- Modify: `docs/specs/SHRINK.md` (a short note that a failed shrink emits `.contended.yaml`, fed back via `plan --confirmed`)

- [ ] **Step 1: Update README**

In the `plan` flags list, add:

```
--confirmed <path>   Path to a .contended.yaml written by a prior failed shrink run.
                     Prioritises those empirically-confirmed blocker objects in the
                     pre-shrink reorganize pass and marks matching heap advisories
                     CONFIRMED. Requires shrink.enabled in the profile.
```

In the sidecars/outputs section, add a line describing `<manifest>.contended.yaml` (objects the shrink held a Sch-M lock on while blocking others; machine-readable; consumed by `plan --confirmed`).

- [ ] **Step 2: Update `docs/specs/SHRINK.md`**

Add a short paragraph: on an `INCOMPLETE`/`FAILED` shrink, the engine writes `<manifest>.contended.yaml` listing the objects it held a `Sch-M` lock on while blocking others; a `.log` pointer line references it; `plan --confirmed <path>` feeds it into the next pre-shrink pass. Cross-reference `docs/specs/superpowers/specs/2026-07-28-contended-object-capture-design.md`.

- [ ] **Step 3: Verify no stale references**

Run: `grep -rn "contended" README.md docs/specs/SHRINK.md`
Expected: the new references present and consistent.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/specs/SHRINK.md
git commit -m "docs: document plan --confirmed and the .contended.yaml sidecar"
```

---

## Self-Review

**Spec coverage:**
- §1 `HeldObjectLocks` + NULL fallback → Task 1. ✓
- §2 accumulator, injection at the sink while lock held, shrink-only gate → Tasks 3, 4. ✓
- §3 sidecar format (`object_id`, `database:` guard, empty→no file), `.log` pointer → Tasks 2, 3, 5. ✓
- §4 `--confirmed` flag, db guard, shrink-enabled guard, augment (prioritize/add-despite-density/annotate), confirmed-heap advisory, stale-id skip-with-log, comment mechanism (`HeadComment`, not a field), heap render-time param → Tasks 6, 7, 8, 9. ✓
- §5 units/boundaries → task decomposition matches the table. ✓
- §6 tests incl. engine injection gate, round-trip, non-regression → Tasks 3, 4, 7, 8, 9. ✓
- Non-goals (no page_allocations, no auto rebuild_heap, no .blocked.yaml read, shrink-only) → respected; nothing in the tasks violates them. ✓

**Placeholder scan:** No "TBD"/"handle edge cases"; every code step carries real code. The two engine-test helpers in Task 4 (`runShrinkManifest`/`runRebuildManifestWithPause`) are described as "mirror the existing engine-test scaffolding" because the exact fixtures depend on the current test harness — the implementer must reuse `TestProcessAllCapturesBlockedSessions`'s setup; the assertions are concrete.

**Type consistency:**
- `LockedObject{ObjectID int64; Schema, Table, Mode string}` — consistent across Tasks 1, 3, 4.
- `maint.ContendedDoc`/`ContendedObject` fields (`ObjectID`, `TimesBlocked`, `FirstSeen`, `LastSeen`) — consistent across Tasks 2, 3, 9.
- `DecidePreShrink(indexes, heaps, p, confirmed map[int64]int)` — new signature used consistently in Tasks 7, 9; call site updated to `nil` in Task 7 then the real set in Task 9.
- `PreShrinkPlan.ReorganizeNotes` parallel slice — produced in Task 7, consumed in Task 9.
- `HeapAdvisory.Confirmed/TimesBlocked` — produced in Task 7, rendered in Task 9.
- `MarshalManifestAnnotated(m, opComments map[int]string)` — produced in Task 8, consumed in Task 9.

Fixed inline during review: Task 7's confirmed-dense case is placed in `confirmedEntries` (so it is both added and reordered to the head), and `tooSmall` is checked before the confirmed branch so a tiny confirmed index is still skipped (matches spec: the page-count floor is not overridden by confirmation).
