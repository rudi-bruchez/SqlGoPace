# Transient-Maintenance-Blocker Recognition Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Teach the shrink driver to recognize when its blocker is a transient index-maintenance operation (`ALTER INDEX` / `DBCC`), say so clearly in the `.log` and TUI, and record the tail object as `transient_maintenance` (not a structural `tail_position` blocker) so `plan --confirmed` is not misled.

**Architecture:** Detection reuses existing reads — `mssql.ActiveSessions` + the pure `mssql.FindSelfBlock` — plus a new pure `IsMaintenanceCommand` predicate over the blocker's `command` verb. The shrink driver samples the self-block on the stall backoff cadence, stashes a `MaintBlock` on the per-operation `tailProbe`, emits a once-per-operation warning, and downgrades the recorded tail kind at give-up. The reaction model (bounded backoff → stop-with-work-preserved → re-queue) is unchanged; only labeling and messaging change.

**Tech Stack:** Go, existing SqlGoPace packages (`internal/mssql`, `internal/run`, `internal/maint`, `cmd/sqlgopace`). No new dependencies. Unit-testable without a database.

**Design source:** `docs/superpowers/specs/2026-07-31-transient-maintenance-blocker-design.md` (revised per the Kimi assessment). Consult it for rationale.

## Global Constraints

- **English only** — all code, comments, identifiers, docs.
- **Idiomatic Go, KISS** — match surrounding style; no new abstractions beyond what the tasks specify.
- **Do not change the reaction hierarchy or timings.** No unbounded wait, no killing the blocker. This feature only classifies, messages, and labels.
- **Allow-list is exactly `ALTER INDEX` and `DBCC`** (case-insensitive, trimmed). `DbccFilesCompact` is a wait/task name, NOT a `command` value — it must return `false`. Verified against the `sys.dm_exec_requests` reference.
- **`confirmed_by` values are lowercase**, consistent with existing `lock` / `tail_position`. The new value is `transient_maintenance`.
- **Below SQL 2019 or when the tail walk fails there is no sidecar entry** — the transient fact still reaches the operator via the `.log`/TUI message. That is acceptable and intended.
- **Separate warning guards** — the existing `tailProbe.warned` (2019+ tail warning) and the new `tailProbe.maintWarned` (maintenance-block warning) must be independent `*bool`s; neither may suppress the other.
- **Build/vet/test gate** — verify with `go build ./...`, `go vet ./...`, and `go test -race ./...`. Do NOT gate on `golangci-lint` (repo is CRLF; go1.26 gofmt flags all files repo-wide — a known false failure).
- **Version bump** — `internal/version/VERSION` from `0.9.0` to `0.10.0` (feature addition), done in the docs task.

---

## File Structure

- `internal/mssql/maintenance.go` (new) — `IsMaintenanceCommand`.
- `internal/mssql/dmv.go` (modify) — add `Command` to `SelfBlock`, fill it in `FindSelfBlock`.
- `internal/maint/contended.go` (modify) — add `BlockedByCommand` / `BlockedBySPID` to `ContendedObject`.
- `internal/run/reaction.go` (modify) — add transient fields to `TailFinding`.
- `internal/run/contended.go` (modify) — record `transient_maintenance` + blocked-by fields; update header comment.
- `internal/run/shrink.go` (modify) — `ShrinkReader.ActiveSessions`, `MaintBlock`, `tailProbe.maintBlock`/`maintWarned`, `probeMaintBlock`, give-up downgrade + reason wording.
- `internal/run/shrink_driver_test.go` (modify) — add `ActiveSessions` to `fakeServer` and `tempdbFakeServer`.
- `cmd/sqlgopace/shrink_plan.go` (modify) — `confirmedSetFor` skips `transient_maintenance`.
- Tests alongside each; docs in `specs/SHRINK.md` + `README.md`.

Task order: 1 (classification) → 2 (recording model) → 3 (planner filter) → 4 (runner detection + message) → 5 (runner downgrade at give-up) → 6 (docs + version). Tasks 1–3 are pure/independent; 4 depends on 1; 5 depends on 2+4.

---

### Task 1: Maintenance-command classification + `SelfBlock.Command`

**Files:**
- Create: `internal/mssql/maintenance.go`
- Create: `internal/mssql/maintenance_test.go`
- Modify: `internal/mssql/dmv.go` (`SelfBlock` struct ~line 192; `FindSelfBlock` ~line 208–234)
- Modify: `internal/mssql/dmv_test.go` (add one assertion)

**Interfaces:**
- Produces: `func IsMaintenanceCommand(cmd string) bool`; `SelfBlock.Command string` (filled from the blocker's session row in `FindSelfBlock`).

- [ ] **Step 1: Write the failing test for `IsMaintenanceCommand`**

Create `internal/mssql/maintenance_test.go`:

```go
package mssql

import "testing"

func TestIsMaintenanceCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"ALTER INDEX", true},
		{"alter index", true},
		{"  ALTER INDEX  ", true},
		{"DBCC", true},
		{"dbcc", true},
		{"SELECT", false},
		{"INSERT", false},
		{"BACKUP DATABASE", false},
		{"", false},
		// DbccFilesCompact is an internal wait/task name, never a command verb.
		{"DbccFilesCompact", false},
	}
	for _, c := range cases {
		if got := IsMaintenanceCommand(c.cmd); got != c.want {
			t.Errorf("IsMaintenanceCommand(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./internal/mssql -run TestIsMaintenanceCommand`
Expected: FAIL — `undefined: IsMaintenanceCommand`.

- [ ] **Step 3: Implement `IsMaintenanceCommand`**

Create `internal/mssql/maintenance.go`:

```go
package mssql

import "strings"

// IsMaintenanceCommand reports whether cmd (a sys.dm_exec_requests.command verb) is a
// known index-maintenance / file-compaction operation the shrink driver treats as a
// transient, self-clearing blocker rather than a structural tail blocker. Conservative
// allow-list; case-insensitive and space-trimmed. Every DBCC statement (INDEXDEFRAG,
// SHRINKFILE, SHRINKDATABASE) reports the verb "DBCC"; ALTER INDEX covers both REBUILD
// and REORGANIZE. Unknown verbs return false, preserving today's behavior for
// application locks, ETL, and reporting workloads.
func IsMaintenanceCommand(cmd string) bool {
	switch strings.ToUpper(strings.TrimSpace(cmd)) {
	case "ALTER INDEX", "DBCC":
		return true
	default:
		return false
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/mssql -run TestIsMaintenanceCommand`
Expected: PASS.

- [ ] **Step 5: Add `Command` to `SelfBlock` and fill it in `FindSelfBlock`**

In `internal/mssql/dmv.go`, add a `Command` field to the `SelfBlock` struct (after `Program string`):

```go
	Program  string
	Command  string // the blocker's dm_exec_requests.command verb (for maintenance classification)
	Host     string
	Query    string
```

In `FindSelfBlock`, inside the loop that fills the blocker's identity (the `for _, s := range sessions { if s.SPID != self.BlockingSPID { continue } ... }` block), set `Command` alongside `Login`/`Program`/`Host`:

```go
		sb.Login, sb.Program, sb.Host = s.Login, s.Program, s.Host
		sb.Command = s.Command
		if sb.Query = s.ActiveQuery; sb.Query == "" {
			sb.Query = s.ParentQuery // an idle-in-transaction blocker has no active statement
		}
		break
```

- [ ] **Step 6: Extend the existing `FindSelfBlock` test to assert `Command`**

In `internal/mssql/dmv_test.go`, in `TestFindSelfBlock` (or `TestFindSelfBlockCapturesHost`), give the blocker session a `Command` and assert it propagates. Add to the snapshot's blocker row `Command: "ALTER INDEX"` and assert:

```go
	if sb.Command != "ALTER INDEX" {
		t.Errorf("SelfBlock.Command = %q, want ALTER INDEX", sb.Command)
	}
```

(Locate the blocker `Session{...}` literal — SPID 104 in the existing test — and add `Command: "ALTER INDEX"` to it.)

- [ ] **Step 7: Run the package tests**

Run: `go test -race ./internal/mssql`
Expected: PASS (all, including the extended self-block test).

- [ ] **Step 8: Commit**

```bash
git add internal/mssql/maintenance.go internal/mssql/maintenance_test.go internal/mssql/dmv.go internal/mssql/dmv_test.go
git commit -m "feat(mssql): classify maintenance command + expose blocker command in SelfBlock"
```

---

### Task 2: Sidecar recording model — `transient_maintenance` + blocked-by fields

**Files:**
- Modify: `internal/maint/contended.go` (`ContendedObject` struct ~line 12–26)
- Modify: `internal/run/reaction.go` (`TailFinding` struct ~line 59–65)
- Modify: `internal/run/contended.go` (`capturedObject` ~line 19–27; `addTail` ~line 55–72; `doc` ~line 75–91; `contendedHeader` ~line 93–100)
- Modify: `internal/run/contended_test.go` (add tests)

**Interfaces:**
- Consumes: nothing new.
- Produces: `TailFinding` gains `Transient bool`, `BlockedByCommand string`, `BlockedBySPID int`. `contendedCapture.doc()` emits `ConfirmedBy: "transient_maintenance"` and the blocked-by fields when a tail entry is transient. `maint.ContendedObject` gains `BlockedByCommand string \`yaml:"blocked_by_command,omitempty"\`` and `BlockedBySPID int \`yaml:"blocked_by_spid,omitempty"\``.

- [ ] **Step 1: Write the failing test**

Add to `internal/run/contended_test.go`:

```go
func TestContendedAddTailTransientMaintenance(t *testing.T) {
	var acc contendedCapture
	acc.addTail(TailFinding{
		ObjectID: 9, Schema: "dbo", Table: "Big", IndexID: 1, PageFromEnd: 4,
		Transient: true, BlockedByCommand: "ALTER INDEX", BlockedBySPID: 104,
	}, "t0")

	o := acc.doc("DB").Observed[0]
	if o.ConfirmedBy != "transient_maintenance" {
		t.Errorf("ConfirmedBy = %q, want transient_maintenance", o.ConfirmedBy)
	}
	if o.BlockedByCommand != "ALTER INDEX" || o.BlockedBySPID != 104 {
		t.Errorf("blocked-by = (%q, %d), want (ALTER INDEX, 104)", o.BlockedByCommand, o.BlockedBySPID)
	}
	if o.IndexID != 1 || o.PageFromEnd != 4 {
		t.Errorf("tail fields lost: %+v", o)
	}
}

func TestRenderContendedTransientRoundTrips(t *testing.T) {
	var acc contendedCapture
	acc.addTail(TailFinding{
		ObjectID: 9, Schema: "dbo", Table: "Big", IndexID: 1, PageFromEnd: 4,
		Transient: true, BlockedByCommand: "DBCC", BlockedBySPID: 55,
	}, "t0")
	out := renderContended("050_shrink.yaml", "DB", &acc)
	doc, err := maint.ParseContended(out) // KnownFields(true): guards new-field drift
	if err != nil {
		t.Fatalf("ParseContended: %v", err)
	}
	o := doc.Observed[0]
	if o.ConfirmedBy != "transient_maintenance" || o.BlockedByCommand != "DBCC" || o.BlockedBySPID != 55 {
		t.Errorf("round-tripped = %+v", o)
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./internal/run -run 'TestContendedAddTailTransientMaintenance|TestRenderContendedTransientRoundTrips'`
Expected: FAIL — unknown fields `Transient`/`BlockedByCommand`/`BlockedBySPID` on `TailFinding`; `ParseContended` rejects unknown YAML keys.

- [ ] **Step 3: Add fields to `TailFinding`**

In `internal/run/reaction.go`, extend `TailFinding`:

```go
type TailFinding struct {
	ObjectID    int64
	Schema      string
	Table       string
	IndexID     int
	PageFromEnd int
	// Transient marks a tail found while a concurrent maintenance operation blocked the
	// shrink: recorded as confirmed_by=transient_maintenance, never a structural blocker.
	Transient        bool
	BlockedByCommand string // the blocker's command verb (e.g. "ALTER INDEX"), when Transient
	BlockedBySPID    int    // the blocker's session id, when Transient
}
```

- [ ] **Step 4: Add fields to `maint.ContendedObject`**

In `internal/maint/contended.go`, after `PageFromEnd`:

```go
	IndexID          int    `yaml:"index_id,omitempty"`
	ConfirmedBy      string `yaml:"confirmed_by,omitempty"`
	PageFromEnd      int    `yaml:"page_from_end,omitempty"`
	// Transient-maintenance capture: the concurrent op that pinned the tail at capture time.
	// Set only when ConfirmedBy == "transient_maintenance"; empty otherwise.
	BlockedByCommand string `yaml:"blocked_by_command,omitempty"`
	BlockedBySPID    int    `yaml:"blocked_by_spid,omitempty"`
```

Update the doc comment on `ConfirmedBy` to mention the third value `transient_maintenance`.

- [ ] **Step 5: Carry the fields through `capturedObject`, `addTail`, and `doc`**

In `internal/run/contended.go`, add to `capturedObject`:

```go
	byTail           bool
	transient        bool
	blockedByCommand string
	blockedBySPID    int
	indexID          int
	pageFromEnd      int
```

In `addTail`, after setting `e.byTail`/`e.indexID`/`e.pageFromEnd`, carry the transient fields:

```go
	e.byTail = true
	e.indexID = f.IndexID
	e.pageFromEnd = f.PageFromEnd
	if f.Transient {
		e.transient = true
		e.blockedByCommand = f.BlockedByCommand
		e.blockedBySPID = f.BlockedBySPID
	}
```

In `doc`, choose `confirmedBy` and emit the fields. **`transient` deliberately takes
precedence over `byTail` and `lock`**: if a concurrent maintenance op was found blocking the
shrink on this object, the transient explanation is the salient one and the entry must not
drive `plan --confirmed`, regardless of any lock/tail evidence also captured for it. The
retained `times_blocked`/`page_from_end` fields stay in the sidecar as an audit trail.

```go
		confirmedBy := "lock"
		switch {
		case e.transient:
			confirmedBy = "transient_maintenance"
		case e.byTail:
			confirmedBy = "tail_position"
		}
		doc.Observed = append(doc.Observed, maint.ContendedObject{
			ObjectID: e.obj.ObjectID, Schema: e.obj.Schema, Table: e.obj.Table,
			LockMode: e.obj.Mode, TimesBlocked: e.count,
			FirstSeen: e.firstSeen, LastSeen: e.lastSeen,
			IndexID: e.indexID, ConfirmedBy: confirmedBy, PageFromEnd: e.pageFromEnd,
			BlockedByCommand: e.blockedByCommand, BlockedBySPID: e.blockedBySPID,
		})
```

- [ ] **Step 6: Update the `contendedHeader` comment**

Add the third kind to the header block in `internal/run/contended.go`:

```go
const contendedHeader = `# Contended-object capture for %s
# Objects this shrink could not get past, by three confirmation kinds:
#   confirmed_by: lock          — held a Sch-M lock on the object while blocking other
#                                 sessions (empirical, partial: the shrink stops at the first).
#   confirmed_by: tail_position — owns the file's last allocated page (the tail the shrink
#                                 must relocate past), found by the backward page walk.
#   confirmed_by: transient_maintenance — the tail was pinned by a concurrent maintenance op
#                                 (e.g. ALTER INDEX) at capture time. Informational only —
#                                 NOT fed to a pre-shrink reorganize.
# Feed this to the planner:  sqlgopace plan --confirmed <this file>
`
```

- [ ] **Step 7: Run the tests**

Run: `go test -race ./internal/run -run 'Contended|RenderContended'` and `go test -race ./internal/maint`
Expected: PASS (new tests plus the existing round-trip/dedup tests).

- [ ] **Step 8: Commit**

```bash
git add internal/maint/contended.go internal/run/reaction.go internal/run/contended.go internal/run/contended_test.go
git commit -m "feat(run): record transient_maintenance tail entries in the contended sidecar"
```

---

### Task 3: Planner filter — `confirmedSetFor` skips transient entries

**Files:**
- Modify: `cmd/sqlgopace/shrink_plan.go` (`confirmedSetFor` ~line 21–35)
- Modify: `cmd/sqlgopace/shrink_plan_test.go` (add a test)

**Interfaces:**
- Consumes: `maint.ContendedObject.ConfirmedBy == "transient_maintenance"`.
- Produces: `confirmedSetFor` omits transient entries from the returned `map[int64]maint.Confirmation`, so `DecidePreShrink` never sees them and stays unchanged.

- [ ] **Step 1: Write the failing test**

Add to `cmd/sqlgopace/shrink_plan_test.go`:

```go
func TestConfirmedSetForSkipsTransientMaintenance(t *testing.T) {
	doc := maint.ContendedDoc{
		Database: "DB",
		Observed: []maint.ContendedObject{
			{ObjectID: 1, ConfirmedBy: "tail_position", IndexID: 1, PageFromEnd: 2},
			{ObjectID: 2, ConfirmedBy: "transient_maintenance", BlockedByCommand: "ALTER INDEX", BlockedBySPID: 104},
		},
	}
	set, err := confirmedSetFor(doc, "DB")
	if err != nil {
		t.Fatalf("confirmedSetFor: %v", err)
	}
	if _, ok := set[2]; ok {
		t.Error("transient_maintenance entry must not become a Confirmation")
	}
	if c, ok := set[1]; !ok || !c.ByTail {
		t.Errorf("tail_position sibling must survive: %+v (ok=%v)", c, ok)
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./cmd/sqlgopace -run TestConfirmedSetForSkipsTransientMaintenance`
Expected: FAIL — object 2 is currently mapped (as a non-tail, times_blocked=0 Confirmation).

- [ ] **Step 3: Skip transient entries in `confirmedSetFor`**

In `cmd/sqlgopace/shrink_plan.go`, inside the `for _, o := range doc.Observed` loop, add a guard at the top:

```go
	for _, o := range doc.Observed {
		if o.ConfirmedBy == "transient_maintenance" {
			continue // informational only — never drives a pre-shrink reorganize
		}
		set[o.ObjectID] = maint.Confirmation{
			TimesBlocked: o.TimesBlocked,
			ByTail:       o.ConfirmedBy == "tail_position",
			IndexID:      o.IndexID,
			PageFromEnd:  o.PageFromEnd,
		}
	}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/sqlgopace -run TestConfirmedSetForSkipsTransientMaintenance`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/sqlgopace/shrink_plan.go cmd/sqlgopace/shrink_plan_test.go
git commit -m "feat(plan): ignore transient_maintenance entries in confirmedSetFor"
```

---

### Task 4: Runner — detect maintenance self-block and message it

**Files:**
- Modify: `internal/run/shrink.go` (`ShrinkReader` ~line 17–29; `tailProbe` ~line 92–96; `Run` ~line 205–243; `chunkLoop` ~line 413–551; add `MaintBlock` + `probeMaintBlock` + `formatWait`)
- Modify: `internal/run/shrink_driver_test.go` (`fakeServer` + `tempdbFakeServer` gain `ActiveSessions`)
- Create: `internal/run/shrink_maintblock_test.go`

**Interfaces:**
- Consumes: `mssql.FindSelfBlock`, `mssql.IsMaintenanceCommand` (Task 1); `r.exec.SPID()`.
- Produces: `type MaintBlock struct { SPID int; Command string; WaitMS int64 }`; `tailProbe.maintBlock *MaintBlock` and `tailProbe.maintWarned *bool`; `(*ShrinkRunner).probeMaintBlock(ctx, f, sink, tp, noProgress)`. Consumed by Task 5.

- [ ] **Step 1: Add `ActiveSessions` to the two test fakes (so the package still compiles once the interface grows)**

In `internal/run/shrink_driver_test.go`, add a `sessions` field to `fakeServer`:

```go
	sessions []mssql.Session // returned by ActiveSessions (constant across calls)
```

and the method (near `SessionWaits`):

```go
func (s *fakeServer) ActiveSessions(context.Context) ([]mssql.Session, error) {
	return s.sessions, nil
}
```

Add the same method to `tempdbFakeServer` (returns `nil, nil`):

```go
func (s *tempdbFakeServer) ActiveSessions(context.Context) ([]mssql.Session, error) {
	return nil, nil
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/run/shrink_maintblock_test.go`:

```go
package run

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// blockedByMaint scripts an ActiveSessions snapshot where our shrink (SPID 99) is
// blocked by session 104 running the given command.
func blockedByMaint(command string) []mssql.Session {
	return []mssql.Session{
		{SPID: 99, BlockingSPID: 104, WaitType: "LCK_M_SCH_M", WaitMS: 90000},
		{SPID: 104, Command: command},
	}
}

func warnDetail(events []ReactionEvent) string {
	for _, e := range events {
		if e.Kind == "warn" {
			return e.Detail
		}
	}
	return ""
}

// TestMaintBlockWarnsOnceUnderMaintenance drives a no-gain give-up while ActiveSessions
// reports our shrink blocked by a concurrent ALTER INDEX. The driver must emit exactly one
// clear "concurrent maintenance" warning naming the verb, the blocker SPID, and the wait.
func TestMaintBlockWarnsOnceUnderMaintenance(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions: blockedByMaint("ALTER INDEX"),
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))

	var events []ReactionEvent
	warns := 0
	sink := func(e ReactionEvent) {
		events = append(events, e)
		if e.Kind == "warn" && strings.Contains(e.Detail, "concurrent maintenance") {
			warns++
		}
	}
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if warns != 1 {
		t.Errorf("maintenance warnings = %d, want exactly 1", warns)
	}
	d := warnDetail(events)
	if !strings.Contains(d, "ALTER INDEX") || !strings.Contains(d, "104") {
		t.Errorf("warning must name the verb and blocker SPID: %q", d)
	}
}

// TestMaintBlockIgnoresApplicationBlocker: an application UPDATE blocking the shrink is NOT
// maintenance — no maintenance warning is emitted (today's behavior is preserved).
func TestMaintBlockIgnoresApplicationBlocker(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions: blockedByMaint("UPDATE"),
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if d := warnDetail(events); strings.Contains(d, "concurrent maintenance") {
		t.Errorf("application blocker must not emit a maintenance warning: %q", d)
	}
}
```

- [ ] **Step 3: Run to confirm failure**

Run: `go test ./internal/run -run TestMaintBlock`
Expected: FAIL — `sessions` scripting has no effect; no `probeMaintBlock` yet, so no warning.

- [ ] **Step 4: Add `ActiveSessions` to `ShrinkReader`**

In `internal/run/shrink.go`, add to the `ShrinkReader` interface:

```go
	// ActiveSessions is the running-request snapshot used to identify a session blocking the
	// shrink (via mssql.FindSelfBlock) and classify it as transient maintenance.
	ActiveSessions(ctx context.Context) ([]mssql.Session, error)
```

- [ ] **Step 5: Add `MaintBlock` and the `tailProbe` fields**

In `internal/run/shrink.go`, extend `tailProbe`:

```go
type tailProbe struct {
	proactive  bool
	warned     *bool
	maintWarned *bool       // separate once-per-op guard for the maintenance-block warning (#kimi-1)
	finding    *TailFinding
	maintBlock *MaintBlock  // set when a concurrent maintenance op is found blocking the shrink
}

// MaintBlock identifies a concurrent maintenance operation blocking the shrink, captured
// from an ActiveSessions self-block snapshot. Its presence downgrades a give-up tail record
// to confirmed_by=transient_maintenance (see shrinkData / captureGiveUpTail).
type MaintBlock struct {
	SPID    int
	Command string
	WaitMS  int64
}
```

In `Run`, allocate the guard alongside `warned`:

```go
	warned := new(bool)
	tp := &tailProbe{proactive: op.IdentifyTailObject, warned: warned, maintWarned: new(bool)}
```

- [ ] **Step 6: Reset `maintBlock` per file and add `probeMaintBlock` + `formatWait`**

In `chunkLoop`, extend the per-file reset block (currently `tp.finding = nil`):

```go
	if tp != nil {
		tp.finding = nil    // per-file reset: tp is shared across a files:all run
		tp.maintBlock = nil // per-file reset: a later file may be blocked by a different op
		if tp.proactive {
			if tf := r.walkTail(ctx, f, sink, tp.warned, true); tf != nil {
				r.emitTail(sink, tf, f.Name, false)
				tp.finding = tf
			}
		}
	}
```

Add the helper functions (near `walkTail`):

```go
// probeMaintBlock samples the self-block once the shrink has stalled (noProgress >= 2) and,
// if a concurrent maintenance operation (ALTER INDEX / DBCC) is blocking us, stashes it on tp
// and emits one clear warning per operation. Best-effort: a nil tp, an already-known block, a
// read error, or a non-maintenance blocker leaves today's behavior unchanged. tp == nil on the
// tempdb path, so tempdb never probes.
func (r *ShrinkRunner) probeMaintBlock(ctx context.Context, f mssql.FileSpace, sink ReactionSink, tp *tailProbe, noProgress int) {
	if tp == nil || tp.maintBlock != nil || noProgress < 2 {
		return
	}
	sessions, err := r.reader.ActiveSessions(ctx)
	if err != nil {
		return
	}
	sb := mssql.FindSelfBlock(sessions, r.exec.SPID())
	if !sb.Blocked || !mssql.IsMaintenanceCommand(sb.Command) {
		return
	}
	tp.maintBlock = &MaintBlock{SPID: sb.SPID, Command: sb.Command, WaitMS: sb.WaitMS}
	if tp.maintWarned != nil && !*tp.maintWarned {
		*tp.maintWarned = true
		sink(ReactionEvent{Kind: "warn", Detail: fmt.Sprintf(
			"shrink of %q blocked by a concurrent maintenance operation — %s on session %d (waiting %s). "+
				"Transient; SqlGoPace is yielding, not forcing. Re-run after maintenance completes.",
			f.Name, sb.Command, sb.SPID, formatWait(sb.WaitMS))})
	}
}

// formatWait renders a millisecond wait as a compact duration (e.g. "12m31s").
func formatWait(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}
```

- [ ] **Step 7: Call `probeMaintBlock` at both stall sites in `chunkLoop`**

In the DBCC-error branch, restructure the inline `if stop, werr := stall(...)` into a plain
assignment so `probeMaintBlock` runs **before** the give-up decision (symmetric with the
no-gain branch, so `maintBlock` is always set before `giveUpReason` reads it):

```go
		stop, werr := r.stall(ctx, f.Name, &noProgress, &backoff, &stallWaited, sink, prof)
		r.probeMaintBlock(ctx, f, sink, tp, noProgress)
		if werr != nil {
			return result, werr
		} else if stop {
			result.FinalMB = current
			result.Reason = fmt.Sprintf("no further progress: %v (work preserved)", err)
			r.captureGiveUpTail(ctx, f, sink, tp)
			return result, nil
		}
		continue
```

(Task 5 replaces the `result.Reason = ...` line here with the `giveUpReason(...)` form.)

In the no-gain branch, right after `stall(...)`:

```go
		stop, werr := r.stall(ctx, f.Name, &noProgress, &backoff, &stallWaited, sink, prof)
		blocked += r.clk.Since(s0)
		r.probeMaintBlock(ctx, f, sink, tp, noProgress)
		if werr != nil {
			return result, werr
		} else if stop {
			result.FinalMB = current
			result.Reason = "no further progress (work preserved)"
			r.captureGiveUpTail(ctx, f, sink, tp)
			return result, nil
		}
		continue
```

Note: `probeMaintBlock` runs even when `stop` is true — that is fine (it early-returns because `tp.maintBlock` was set on the prior iteration at `noProgress == 2`, or sets it now for the give-up). Task 5 consumes `tp.maintBlock` in the give-up reason and record.

- [ ] **Step 8: Add a guard-independence test**

Prove the two once-per-operation warnings are independent (the 2019+ tail warning via
`tp.warned`, the maintenance warning via `tp.maintWarned`): a proactive walk below 2019 that
is ALSO maintenance-blocked must emit BOTH warnings. Add to `shrink_maintblock_test.go`:

```go
// TestTailAndMaintWarningsAreIndependent: below 2019 with IdentifyTailObject set AND a
// concurrent maintenance block, both once-per-operation warnings fire (separate guards).
func TestTailAndMaintWarningsAreIndependent(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions: blockedByMaint("ALTER INDEX"),
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.major = 13 // below 2019: the proactive walk warns about the missing DMV

	var tailWarn, maintWarn bool
	sink := func(e ReactionEvent) {
		if e.Kind == "warn" && strings.Contains(e.Detail, "2019") {
			tailWarn = true
		}
		if e.Kind == "warn" && strings.Contains(e.Detail, "concurrent maintenance") {
			maintWarn = true
		}
	}
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%", IdentifyTailObject: true}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !tailWarn || !maintWarn {
		t.Errorf("both warnings must fire independently: tailWarn=%v maintWarn=%v", tailWarn, maintWarn)
	}
}
```

- [ ] **Step 9: Run the tests**

Run: `go test -race ./internal/run -run 'TestMaintBlock|TestTailAndMaint|Shrink|Tail'`
Expected: PASS — the three new tests pass; all existing shrink/tail tests still pass (they script `sessions: nil`, so `probeMaintBlock` is a silent no-op).

- [ ] **Step 10: Commit**

```bash
git add internal/run/shrink.go internal/run/shrink_driver_test.go internal/run/shrink_maintblock_test.go
git commit -m "feat(shrink): detect and warn on concurrent maintenance blocking the shrink"
```

---

### Task 5: Runner — record the give-up tail as transient and name it in the reason

**Files:**
- Modify: `internal/run/shrink.go` (`shrinkData` post-loop ~line 398–407; `chunkLoop` two give-up reason lines; `captureGiveUpTail` ~line 851–858; add `markTransient`)
- Modify: `internal/run/shrink_maintblock_test.go` (add record + reason assertions)

**Interfaces:**
- Consumes: `tailProbe.maintBlock` (Task 4); `TailFinding.Transient`/`BlockedByCommand`/`BlockedBySPID` (Task 2).
- Produces: give-up under maintenance records `confirmed_by: transient_maintenance` and the give-up `Reason` names the maintenance op.

- [ ] **Step 1: Write the failing test**

Add to `internal/run/shrink_maintblock_test.go` (reuse `blockedByMaint`, `wantTail`, `warnDetail` from Tasks 4 / tail tests):

```go
// TestMaintBlockRecordsTransientTail: a give-up under concurrent ALTER INDEX records the tail
// as transient (Transient + blocked-by set), and the give-up reason names the maintenance op.
func TestMaintBlockRecordsTransientTail(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions:  blockedByMaint("ALTER INDEX"),
		tail:      mssql.TailObject{ObjectID: 21, Schema: "dbo", Table: "Rebuilt", IndexID: 1, PageFromEnd: 3},
		tailFound: true,
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.major = 15

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(res) != 1 || res[0].Reason == "" {
		t.Fatalf("got %+v, want a give-up result with a reason", res)
	}
	tf := wantTail(events)
	if tf == nil || !tf.Transient {
		t.Fatalf("want a transient tail-bearing event, got %+v", tf)
	}
	if tf.BlockedByCommand != "ALTER INDEX" || tf.BlockedBySPID != 104 {
		t.Errorf("blocked-by = (%q, %d), want (ALTER INDEX, 104)", tf.BlockedByCommand, tf.BlockedBySPID)
	}
	if !strings.Contains(res[0].Reason, "maintenance") || !strings.Contains(res[0].Reason, "ALTER INDEX") {
		t.Errorf("give-up reason must name the maintenance op: %q", res[0].Reason)
	}
}

// TestApplicationBlockerStaysTailPosition: the SAME give-up, but the blocker is an application
// UPDATE, records a normal structural tail_position (Transient false) — unchanged behavior.
func TestApplicationBlockerStaysTailPosition(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions:  blockedByMaint("UPDATE"),
		tail:      mssql.TailObject{ObjectID: 22, Schema: "dbo", Table: "Hot", IndexID: 1, PageFromEnd: 1},
		tailFound: true,
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.major = 15

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	tf := wantTail(events)
	if tf == nil || tf.Transient {
		t.Fatalf("want a non-transient tail_position record, got %+v", tf)
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./internal/run -run 'TestMaintBlockRecordsTransientTail|TestApplicationBlockerStaysTailPosition'`
Expected: FAIL — the tail is recorded but `Transient` is false and the reason is generic.

- [ ] **Step 3: Add `markTransient` and use it in `captureGiveUpTail`**

In `internal/run/shrink.go`, add:

```go
// markTransient tags a give-up tail finding as transient-maintenance when a concurrent
// maintenance op was found blocking the shrink, so it is recorded as informational rather
// than a structural blocker.
func (r *ShrinkRunner) markTransient(tf *TailFinding, tp *tailProbe) {
	if tp == nil || tp.maintBlock == nil || tf == nil {
		return
	}
	tf.Transient = true
	tf.BlockedByCommand = tp.maintBlock.Command
	tf.BlockedBySPID = tp.maintBlock.SPID
}
```

Update `captureGiveUpTail` to tag before emitting:

```go
func (r *ShrinkRunner) captureGiveUpTail(ctx context.Context, f mssql.FileSpace, sink ReactionSink, tp *tailProbe) {
	if tp == nil || tp.finding != nil {
		return
	}
	tf := r.walkTail(ctx, f, sink, tp.warned, false)
	if tf == nil {
		return
	}
	r.markTransient(tf, tp)
	r.emitTail(sink, tf, f.Name, true)
}
```

- [ ] **Step 4: Tag the proactive-stash record in `shrinkData`**

In `shrinkData`, the post-loop record on a missed target:

```go
	if tp != nil && tp.finding != nil && out.FinalMB > out.TargetMB {
		r.markTransient(tp.finding, tp)
		r.emitTail(sink, tp.finding, f.Name, true)
	}
```

- [ ] **Step 5: Name the maintenance op in the two give-up reasons**

In `chunkLoop`, replace the two give-up `result.Reason = ...` lines with a maintenance-aware reason. Add a small helper:

```go
// giveUpReason builds the give-up reason, naming a concurrent maintenance op when one was
// found blocking the shrink; base is the default reason for a non-maintenance give-up.
func giveUpReason(tp *tailProbe, base string) string {
	if tp != nil && tp.maintBlock != nil {
		return fmt.Sprintf("stopped: file tail pinned by concurrent maintenance (%s, session %d) — "+
			"transient, not a structural blocker; work preserved, re-run after maintenance",
			tp.maintBlock.Command, tp.maintBlock.SPID)
	}
	return base
}
```

No-gain branch:

```go
		} else if stop {
			result.FinalMB = current
			result.Reason = giveUpReason(tp, "no further progress (work preserved)")
			r.captureGiveUpTail(ctx, f, sink, tp)
			return result, nil
		}
```

DBCC-error branch:

```go
		} else if stop {
			result.FinalMB = current
			result.Reason = giveUpReason(tp, fmt.Sprintf("no further progress: %v (work preserved)", err))
			r.captureGiveUpTail(ctx, f, sink, tp)
			return result, nil
		}
```

- [ ] **Step 6: Run the tests**

Run: `go test -race ./internal/run -run 'TestMaintBlock|TestApplicationBlockerStaysTailPosition|Shrink|Tail'`
Expected: PASS — the transient record + reason tests pass; the application-blocker test still records `tail_position`; existing tail tests (no `sessions`) are unaffected.

- [ ] **Step 7: Run the full package to catch regressions**

Run: `go test -race ./internal/run`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/run/shrink.go internal/run/shrink_maintblock_test.go
git commit -m "feat(shrink): record give-up tail as transient_maintenance and name the op in the reason"
```

---

### Task 6: Documentation + version bump

**Files:**
- Modify: `specs/SHRINK.md` (add a "Concurrency with index maintenance" subsection)
- Modify: `README.md` (one line under the shrink/contended-capture description)
- Modify: `internal/version/VERSION` (`0.9.0` → `0.10.0`)

**Interfaces:** none (docs only).

- [ ] **Step 1: Add the SHRINK.md subsection**

Locate the insertion point: `grep -n "tail_position\|contended\|tail object" specs/SHRINK.md` and add
the new subsection near the existing tail-object / contended-capture material. Write it in the
doc's existing prose style, English. Content to include:

- A shrink blocked by a concurrent `ALTER INDEX` / `DBCC` (rebuild, reorganize, indexdefrag, another shrink) yields as usual — bounded backoff, stop with work preserved, re-queue — and does **not** wait indefinitely or kill the blocker.
- It recognizes the blocker via `ActiveSessions` + `FindSelfBlock` + `IsMaintenanceCommand` (needs only `VIEW SERVER STATE`, so it works below SQL 2019 too) and emits a clear `.log`/TUI warning naming the operation verb, the blocker session, and the wait, with the recommended action (re-run after maintenance).
- On a give-up under maintenance the tail object is recorded as `confirmed_by: transient_maintenance` (with `blocked_by_command`/`blocked_by_spid`), which `plan --confirmed` ignores — so a transient rebuild is never mistaken for a structural tail blocker. Below 2019 / on a failed walk there is no sidecar entry, only the message.

- [ ] **Step 2: Add the README line**

Locate the section: `grep -n "confirmed_by\|contended\|tail object\|--confirmed" README.md` and add
one line under the shrink / contended-capture documentation: a shrink blocked by concurrent
index maintenance is reported as transient (clear log/TUI message) and recorded as
`transient_maintenance`, which `plan --confirmed` ignores.

- [ ] **Step 3: Bump the version**

Set `internal/version/VERSION` to `0.10.0`.

- [ ] **Step 4: Verify the whole build/test gate**

Run: `go build ./...` and `go vet ./...` and `go test -race ./...`
Expected: all succeed (833+ tests pass).

- [ ] **Step 5: Commit**

```bash
git add specs/SHRINK.md README.md internal/version/VERSION
git commit -m "docs(shrink): document transient-maintenance recognition; bump to 0.10.0"
```

---

## Self-Review

**Spec coverage:**
- §1 classification helper → Task 1 (`IsMaintenanceCommand`, allow-list `ALTER INDEX`/`DBCC`, `DbccFilesCompact`→false).
- §2 self-block read (no new SQL) → Task 1 (`SelfBlock.Command`) + Task 4 (`ActiveSessions` on `ShrinkReader`, `FindSelfBlock` use).
- §3 sampling cadence + separate `maintWarned` guard → Task 4 (`probeMaintBlock` at `noProgress >= 2`, `tailProbe.maintWarned`).
- §4 clear log AND TUI messaging (verb, SPID, elapsed wait, transient, action) → Task 4 (`probeMaintBlock` warn) + Task 5 (give-up reason). Both go through `ReactionSink`, which feeds `.log` and TUI.
- §5 sidecar: `transient_maintenance`, filtered in `confirmedSetFor`, no-object case → Task 2 (record) + Task 3 (filter). No-object case needs no code (no `TailFinding` → no entry); covered by existing behavior.
- §6 interaction with proactive/reactive walk → Task 5 (`captureGiveUpTail` + `shrinkData` post-loop both call `markTransient`).

**Placeholder scan:** every code step carries concrete code; no TBD/TODO.

**Type consistency:** `MaintBlock{SPID,Command,WaitMS}`, `TailFinding.{Transient,BlockedByCommand,BlockedBySPID}`, `ContendedObject.{BlockedByCommand,BlockedBySPID}`, `tailProbe.{maintBlock,maintWarned}`, `probeMaintBlock(ctx,f,sink,tp,noProgress)`, `markTransient(tf,tp)`, `giveUpReason(tp,base)`, `formatWait(ms)` — names used identically across Tasks 2/4/5. `confirmed_by` string literal `"transient_maintenance"` is identical in `contended.go` (write) and `shrink_plan.go` (filter).

**Note for the executor:** Tasks 4 and 5 both edit `internal/run/shrink.go` and share the new `shrink_maintblock_test.go`; run each task's test subset, but run the full `go test -race ./internal/run` after Task 5 before considering the runner work done. Do NOT gate on golangci-lint (CRLF/gofmt repo-wide false failure).
