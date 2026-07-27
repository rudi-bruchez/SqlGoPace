# shrink_tempdb Operation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a dedicated `shrink_tempdb` operation that shrinks every tempdb data file to a common absolute size, best-effort and live, reusing `ShrinkRunner` via a new `RunTempdb` orchestrator with tempdb-specific escalation.

**Architecture:** A new pure-core operation `ddl.ShrinkTempdb` (parse → resolve → generate → plan) routes at run time to a **second `ShrinkRunner` instance bound to a tempdb-scoped connection**. The tempdb path is a `RunTempdb` orchestrator (Phase 0: `TRUNCATEONLY` all files; Phase 1: chunk-loop all files) that reuses the extracted `chunkLoop` and `runTruncateOnly` primitives. A `TempdbProfile` threads the equal-size target and the opt-in cache-flush escalation (`FREESYSTEMCACHE ('Temporary Tables & Table Variables')`, once per run) into the shared stall ladder, which also treats error 845 as a retryable no-progress event.

**Tech Stack:** Go, `github.com/microsoft/go-mssqldb`, standard `testing` (`-race`), golangci-lint v2.

## Global Constraints

- **Source of truth:** `specs/TEMPDB-SHRINK.md`. Re-read the relevant section before each task.
- **Idiomatic Go, KISS.** No new layers/interfaces/generics beyond what a task requires.
- **English only** for all code, comments, identifiers, docs.
- **Manifest-driven, never raw SQL.** The capability is a new operation type end-to-end.
- **No query timeout.** Duration is governed by monitoring + reaction, never `context.WithTimeout` around executing DDL.
- **Unit tests need NO database** (`make test`, `-race`). DB-touching tests go behind the `integration` build tag.
- **Version** lives in `internal/version/VERSION` (bump the file; no build flags).
- **Windows binary lock:** stop a running `bin/sqlgopace.exe` before rebuilding to the same path.
- **US spelling** in comments/identifiers.
- **Never kill a blocker** in the tempdb path: WALP resolves to `ABORT_AFTER_WAIT = SELF` always.
- **`NoProgressBeforeFlush` default = 2**, must be `< MaxNoProgress` (3 today) so the flush fires before the give-up *and* leaves retry room; config validation rejects `>= MaxNoProgress`. See Task 4.

---

## File structure

| File | Responsibility | Tasks |
|------|----------------|-------|
| `internal/ddl/manifest.go` | `ShrinkTempdb` op struct + `Validate`/`CommandType`/`Target`; decode case | 1 |
| `internal/ddl/resolve.go` | route `shrink_tempdb` to WALP-only resolution, force `SELF`; `overridesOf` case | 2 |
| `ddl_compatibility.yaml` | `shrink_tempdb` matrix rows for `wait_at_low_priority` | 2 |
| `internal/ddl/generate.go` | indicative statement for `ShrinkTempdb` (real SQL built at run time) | 3 |
| `internal/config/config.go` | `NoProgressBeforeFlush` config + default + validation | 4 |
| `internal/run/shrink_calc.go` | `ShrinkTuning.NoProgressBeforeFlush` field | 4 |
| `cmd/sqlgopace/main.go` | map config → tuning; open tempdb conn; wire second runner | 4, 10 |
| `internal/mssql/analysis.go` | `IsBufferLatchTimeout` (error 845) detector | 5 |
| `internal/run/shrink.go` | extract `chunkLoop`; `TempdbProfile`; flush escalation; `RunTempdb` | 6, 7, 8 |
| `internal/run/engine.go` | `TempdbShrinkDriver` interface; `processOne` case; `isShrinkOp` | 8 |
| `internal/preflight/preflight.go` | recognize `ShrinkTempdb` in the three op switches | 9 |
| `internal/mssql/conn.go` | `OpenScoped` — a `*Conn` whose context is another database (tempdb) | 10 |
| `README.md`, operator skill | document the op + non-goals + `flushcaches` trade-off | 11 |

---

## Task 1: `ddl.ShrinkTempdb` manifest operation

**Files:**
- Modify: `internal/ddl/manifest.go` (add struct + methods near `Shrink`, ~line 909; add decode case at ~line 411)
- Test: `internal/ddl/manifest_test.go`

**Interfaces:**
- Produces: `ddl.ShrinkTempdb{ TargetSizeMB int; FlushCaches bool; Options OptionOverrides }`; `CommandType() string == "shrink_tempdb"`; `Target() ObjectRef == ObjectRef{Database: "tempdb"}`; decode key `"shrink_tempdb"`.

- [ ] **Step 1: Write the failing test**

```go
func TestShrinkTempdbDecodeAndValidate(t *testing.T) {
	node := mustYAMLNode(t, `
operation: shrink_tempdb
targetsizemb: 20480
flushcaches: true
`)
	op, err := decodeOperation(node) // same entry point the other decode tests use
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	st, ok := op.(ShrinkTempdb)
	if !ok {
		t.Fatalf("got %T, want ShrinkTempdb", op)
	}
	if st.TargetSizeMB != 20480 || !st.FlushCaches {
		t.Fatalf("decoded = %+v", st)
	}
	if got := st.CommandType(); got != "shrink_tempdb" {
		t.Errorf("CommandType = %q", got)
	}
	if got := st.Target(); got != (ObjectRef{Database: "tempdb"}) {
		t.Errorf("Target = %+v", got)
	}
	if err := st.Validate(); err != nil {
		t.Errorf("Validate = %v, want nil", err)
	}
}

func TestShrinkTempdbValidateRejectsNonPositive(t *testing.T) {
	if err := (ShrinkTempdb{TargetSizeMB: 0}).Validate(); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}
```

> Match the existing decode-test entry point in `manifest_test.go` (find how `Shrink` is decoded in tests — reuse that helper name rather than `decodeOperation`/`mustYAMLNode` if they differ).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ddl -run TestShrinkTempdb -v`
Expected: FAIL (undefined `ShrinkTempdb`).

- [ ] **Step 3: Write minimal implementation**

In `manifest.go`, after the `Shrink` methods (~line 924):

```go
// ShrinkTempdb shrinks every tempdb data file to a common absolute size. It is a
// dedicated operation (not a Shrink variant): tempdb is always database_id 2, has
// no schema.table target, and the runtime driver adds tempdb-specific escalation
// (see specs/TEMPDB-SHRINK.md). Like check_db it is database-scoped.
type ShrinkTempdb struct {
	TargetSizeMB int             `yaml:"targetsizemb"`          // common absolute target per data file, MB
	FlushCaches  bool            `yaml:"flushcaches,omitempty"` // opt-in cache-flush escalation on persistent stall
	Options      OptionOverrides `yaml:"options"`               // only WaitAtLowPriority is relevant
}

func (o ShrinkTempdb) CommandType() string { return "shrink_tempdb" }
func (o ShrinkTempdb) Target() ObjectRef   { return ObjectRef{Database: "tempdb"} }

func (o ShrinkTempdb) Validate() error {
	if o.TargetSizeMB <= 0 {
		return fmt.Errorf("shrink_tempdb: targetsizemb must be > 0, got %d: %w", o.TargetSizeMB, ErrInvalidManifest)
	}
	return nil
}
```

Add the decode case at ~line 412 (after `case "shrink":`):

```go
	case "shrink_tempdb":
		return decodeInto[ShrinkTempdb](node)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ddl -run TestShrinkTempdb -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ddl/manifest.go internal/ddl/manifest_test.go
git commit -m "feat(ddl): add shrink_tempdb manifest operation"
```

---

## Task 2: Resolve `shrink_tempdb` (WALP only, always SELF)

**Files:**
- Modify: `internal/ddl/resolve.go` (dispatch ~line 68; `resolveShrink` ~line 196; `overridesOf` ~line 292)
- Modify: `ddl_compatibility.yaml` (add `shrink_tempdb` rows mirroring `shrink_data` for `wait_at_low_priority`)
- Test: `internal/ddl/resolve_test.go`

**Interfaces:**
- Consumes: `ddl.ShrinkTempdb` (Task 1); `Matrix`, `Policy`, `Target`, `ResolvedOptions`.
- Produces: for `shrink_tempdb`, `ResolvedOptions{WaitAtLowPriority: <matrix/policy>, AbortAfterWait: "SELF"}` — **never** `"BLOCKERS"`, even when `Policy.AllowAbortBlockers` is set.

- [ ] **Step 1: Write the failing test**

```go
func TestResolveShrinkTempdbAlwaysSelf(t *testing.T) {
	m := testMatrix(t) // same helper other resolve tests use
	// 2022 tier where WALP is supported, with AllowAbortBlockers ON:
	res, _ := Resolve(ShrinkTempdb{TargetSizeMB: 20480},
		Target{MajorVersion: 16, Tier: TierEnterprise},
		m, Policy{AllowAbortBlockers: true})
	if !res.WaitAtLowPriority {
		t.Fatalf("expected WALP eligible on 2022")
	}
	if res.AbortAfterWait != "SELF" {
		t.Errorf("AbortAfterWait = %q, want SELF (never BLOCKERS for tempdb)", res.AbortAfterWait)
	}
}

func TestResolveShrinkTempdbNoWALPOn2019(t *testing.T) {
	m := testMatrix(t)
	res, _ := Resolve(ShrinkTempdb{TargetSizeMB: 20480},
		Target{MajorVersion: 15, Tier: TierEnterprise}, m, Policy{})
	if res.WaitAtLowPriority {
		t.Errorf("WALP must be off for shrink on 2019")
	}
}
```

> Verify the exact `Target`/`Tier` constructors and matrix helper names against neighboring tests in `resolve_test.go` before writing. If the shared `Resolve` entry differs, use it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ddl -run TestResolveShrinkTempdb -v`
Expected: FAIL (shrink_tempdb not routed; `AbortAfterWait` may be `BLOCKERS` or empty).

- [ ] **Step 3: Write minimal implementation**

In `resolve.go` dispatch (~line 68), extend the shrink branch:

```go
	if cmd == "shrink_data" || cmd == "shrink_log" || cmd == "shrink_tempdb" {
		return resolveShrink(op, t, m, p)
	}
```

In `resolveShrink` (~line 204), force SELF for the tempdb command type:

```go
	res := ResolvedOptions{WaitAtLowPriority: walp}
	if walp {
		res.AbortAfterWait = "SELF"
		if p.AllowAbortBlockers && cmd != "shrink_tempdb" {
			res.AbortAfterWait = "BLOCKERS"
		}
		// No MAX_DURATION for shrink WALP: the engine uses a fixed ~1-minute wait.
	}
```

In `overridesOf` (~line 292), add:

```go
	case ShrinkTempdb:
		return o.Options
```

In `ddl_compatibility.yaml`, duplicate every `shrink_data` entry for `wait_at_low_priority` under command type `shrink_tempdb` (same version/tier gating — WALP for DBCC SHRINKFILE is 2022+/major ≥ 16). Find the `shrink_data` block and mirror it.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ddl -run TestResolveShrinkTempdb -v`
Expected: PASS. Then `go test ./internal/ddl` (matrix-load tests must still pass).

- [ ] **Step 5: Commit**

```bash
git add internal/ddl/resolve.go ddl_compatibility.yaml internal/ddl/resolve_test.go
git commit -m "feat(ddl): resolve shrink_tempdb WALP as SELF only"
```

---

## Task 3: Generate + plan for `shrink_tempdb`

**Files:**
- Modify: `internal/ddl/generate.go` (`Generate` switch ~line 17; add `generateShrinkTempdb`)
- Test: `internal/ddl/generate_test.go`

**Interfaces:**
- Consumes: `ddl.ShrinkTempdb`, `ResolvedOptions`.
- Produces: `Generate(ShrinkTempdb{...}, res)` returns a non-empty indicative statement and `nil` error, so `Plan` yields a `PlannedOperation` (real per-file SQL is built at run time by the driver).

- [ ] **Step 1: Write the failing test**

```go
func TestGenerateShrinkTempdbIsIndicative(t *testing.T) {
	sql, err := Generate(ShrinkTempdb{TargetSizeMB: 20480}, ResolvedOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(sql, "DBCC SHRINKFILE") || !strings.Contains(sql, "tempdb") {
		t.Errorf("indicative statement missing tempdb/DBCC: %q", sql)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ddl -run TestGenerateShrinkTempdb -v`
Expected: FAIL (`ErrUnsupportedOperation`).

- [ ] **Step 3: Write minimal implementation**

Add a `case` in the `Generate` switch:

```go
	case ShrinkTempdb:
		return generateShrinkTempdb(o, res), nil
```

Add the helper near `generateShrink` (~line 307):

```go
// generateShrinkTempdb returns an INDICATIVE statement. Real SQL is multi-statement
// and built at run time (per file, target from live DMV reads), run in tempdb context.
func generateShrinkTempdb(o ShrinkTempdb, res ResolvedOptions) string {
	return fmt.Sprintf(
		"-- shrink_tempdb is built at run time per file (tempdb context); representative statement:\n"+
			"USE [tempdb]; DBCC SHRINKFILE (<file>, %d)%s;",
		o.TargetSizeMB, shrinkWith(res))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ddl -run TestGenerateShrinkTempdb -v`
Expected: PASS. Then `go test ./internal/ddl` (plan tests exercising all ops stay green).

- [ ] **Step 5: Commit**

```bash
git add internal/ddl/generate.go internal/ddl/generate_test.go
git commit -m "feat(ddl): generate indicative statement for shrink_tempdb"
```

---

## Task 4: `NoProgressBeforeFlush` config + tuning plumbing

**Files:**
- Modify: `internal/config/config.go` (`ShrinkConfig` ~line 158; the defaults function; `Validate` if it bounds fields)
- Modify: `internal/run/shrink_calc.go` (`ShrinkTuning` struct — add field)
- Modify: `cmd/sqlgopace/main.go` (`shrinkTuning` mapping ~line 424 area)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.ShrinkConfig.NoProgressBeforeFlush int` (yaml `no_progress_before_flush`, default `2`); `run.ShrinkTuning.NoProgressBeforeFlush int`; `config.Validate` rejects `NoProgressBeforeFlush >= MaxNoProgress`.

- [ ] **Step 1: Write the failing test**

In `config_test.go`, add to the defaults table (near line 162, alongside `max_no_progress`):

```go
		{"no_progress_before_flush", s.NoProgressBeforeFlush, 2},
```

And a validation-rejection test (mirror the existing rejection-table style):

```go
func TestShrinkConfigRejectsFlushNotBelowMaxNoProgress(t *testing.T) {
	cfg := validParsedConfig(t)          // however the other tests build a valid cfg
	cfg.Shrink.MaxNoProgress = 3
	cfg.Shrink.NoProgressBeforeFlush = 3 // not < MaxNoProgress → invalid
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected Validate error when no_progress_before_flush >= max_no_progress")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config -run TestDefaults -v` (use the actual defaults-test name)
Expected: FAIL (unknown field `NoProgressBeforeFlush`).

- [ ] **Step 3: Write minimal implementation**

In `config.go` `ShrinkConfig` add (after `MaxNoProgress`, ~line 167):

```go
	NoProgressBeforeFlush int `yaml:"no_progress_before_flush"` // no-progress events before the tempdb cache-flush escalation; must be < MaxNoProgress
```

In the shrink defaults (find where `MaxNoProgress` gets its default of 3) add:

```go
	if s.NoProgressBeforeFlush <= 0 {
		s.NoProgressBeforeFlush = 2
	}
```

In `config.Validate` (where other shrink/config bounds are enforced), add:

```go
	if s := c.Shrink; s.NoProgressBeforeFlush >= s.MaxNoProgress {
		return fmt.Errorf("shrink.no_progress_before_flush (%d) must be < max_no_progress (%d)", s.NoProgressBeforeFlush, s.MaxNoProgress)
	}
```

> Apply defaults **before** this check so a zero-value config validates against the defaulted `2`, not `0`. Match how the existing shrink-field validation is structured (it may live in a `ShrinkConfig.validate()` helper — put it there if so).

In `internal/run/shrink_calc.go`, add to `ShrinkTuning`:

```go
	NoProgressBeforeFlush int // no-progress events before the tempdb cache flush (tempdb path only)
```

In `main.go` `shrinkTuning(cfg.Shrink)` mapping, add:

```go
		NoProgressBeforeFlush: cfg.Shrink.NoProgressBeforeFlush,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config ./internal/run ./cmd/sqlgopace` (compile + defaults)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/run/shrink_calc.go cmd/sqlgopace/main.go internal/config/config_test.go
git commit -m "feat(config): add no_progress_before_flush (default 3) for tempdb shrink"
```

---

## Task 5: Error 845 detector (`internal/mssql`)

**Files:**
- Modify: `internal/mssql/analysis.go` (or wherever `IsFileAllocationError` lives — grep it)
- Test: the matching `_test.go`

**Interfaces:**
- Produces: `func IsBufferLatchTimeout(err error) bool` — true when the error is SQL Server Msg 845 (buffer-latch time-out).

- [ ] **Step 1: Write the failing test**

```go
func TestIsBufferLatchTimeout(t *testing.T) {
	// mssql.Error has a Number field; build one the way IsFileAllocationError's test does.
	err845 := mssql.Error{Number: 845, Message: "Time-out occurred while waiting for buffer latch..."}
	if !IsBufferLatchTimeout(err845) {
		t.Errorf("845 not detected")
	}
	if IsBufferLatchTimeout(mssql.Error{Number: 5240}) {
		t.Errorf("5240 wrongly detected as 845")
	}
	if IsBufferLatchTimeout(nil) {
		t.Errorf("nil wrongly detected")
	}
}
```

> Mirror the construction used by the existing `IsFileAllocationError` test (same package, same error type). Reuse its `errors.As` unwrap pattern.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mssql -run TestIsBufferLatchTimeout -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Write minimal implementation**

Next to `IsFileAllocationError`:

```go
// IsBufferLatchTimeout reports whether err is SQL Server Msg 845 (a time-out
// waiting for a buffer latch), a severe-contention signal the tempdb shrink treats
// as a retryable no-progress event rather than a hard failure.
func IsBufferLatchTimeout(err error) bool {
	var e Error
	return errors.As(err, &e) && e.Number == 845
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mssql -run TestIsBufferLatchTimeout -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mssql/analysis.go internal/mssql/*_test.go
git commit -m "feat(mssql): detect error 845 (buffer latch timeout)"
```

---

## Task 6: Extract `chunkLoop` primitive from `shrinkData` (refactor, no behavior change)

**Files:**
- Modify: `internal/run/shrink.go` (`shrinkData` Phase B loop, lines ~287-399)
- Test: existing `internal/run/shrink_driver_test.go` must stay green (this is a pure refactor)

**Interfaces:**
- Produces: `func (r *ShrinkRunner) chunkLoop(ctx context.Context, f mssql.FileSpace, start, final int, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink, prof *TempdbProfile) (ShrinkResult, error)` — runs the page-moving chunk loop for one already-truncated file from `start` down to `final`. `prof == nil` = normal shrink (no tempdb escalation). Used by both `shrinkData` (Task unchanged) and `RunTempdb` (Task 8).
- Declares (empty for now, filled in Task 7): `type TempdbProfile struct{}` placeholder so the signature compiles.

- [ ] **Step 1: Add the placeholder type and run the full suite (baseline green)**

Add near the top of `shrink.go`:

```go
// TempdbProfile carries the tempdb-shrink-specific knobs into the shared chunk loop.
// Nil for a normal (non-tempdb) shrink. Fields are added in the flush-escalation task.
type TempdbProfile struct {
	FlushCaches bool
	flushed     *bool // once-per-run guard, shared across a RunTempdb's files
}
```

Run: `go test ./internal/run` — Expected: PASS (baseline before refactor).

- [ ] **Step 2: Extract the loop**

Move the Phase B block (from `start := size` through the `return result, nil` that ends the `for current > final` loop, lines ~288-400) into a new method `chunkLoop`, parameterized by `f`, `start`, `final`, `res`, `ignore`, `sink`, `prof`. Have `shrinkData` call it:

```go
	// Phase B — chunked page-moving shrink.
	return r.chunkLoop(ctx, f, size, final, res, ignore, sink, nil)
```

`chunkLoop` builds its own `result := ShrinkResult{File: f.Name, Type: "data", InitialMB: f.SizeMB, TargetMB: final, FinalMB: start}` and returns it. Keep every existing branch (Msg 5240, no-gain stall, progress adjust) byte-for-byte; only the enclosing signature and the `result` init move. `prof` is unused this task (threaded, not read).

- [ ] **Step 3: Run the full suite to verify no behavior change**

Run: `go test -race ./internal/run`
Expected: PASS — identical to Step 1. If any shrink test fails, the extraction changed behavior; revert and redo.

- [ ] **Step 4: Vet + lint**

Run: `go vet ./internal/run && golangci-lint run internal/run/...`
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add internal/run/shrink.go
git commit -m "refactor(run): extract chunkLoop primitive from shrinkData"
```

---

## Task 7: `TempdbProfile` flush escalation in the stall ladder

**Files:**
- Modify: `internal/run/shrink.go` (`stall` ~line 620; `chunkLoop` from Task 6; add `flushCaches` helper)
- Test: `internal/run/shrink_driver_test.go` (new cases)

**Interfaces:**
- Consumes: `chunkLoop` (Task 6), `IsBufferLatchTimeout` (Task 5), `r.tuning.NoProgressBeforeFlush` (Task 4), `r.exec.ExecDDL`.
- Produces: `stall` performs the cache flush once when `prof != nil && prof.FlushCaches` and the no-progress count reaches `NoProgressBeforeFlush` and `*prof.flushed` is false; sets `*prof.flushed = true`, resets the no-progress counter, and continues. Error 845 from a chunk is routed into the same no-progress path as Msg 5240.

- [ ] **Step 1: Write the failing test**

```go
func TestTempdbChunkLoopFlushesOnceOnStall(t *testing.T) {
	// Fake reader: file never shrinks (every chunk is no-gain) → stall ladder engages.
	// Fake exec: records ExecDDL statements so we can assert the flush ran exactly once.
	exec := &recordingExec{}
	reader := &stuckReader{sizeMB: 1000} // FileSizeMB always 1000 → no progress
	r := newTestShrinkRunner(t, exec, reader, ShrinkTuning{
		MinStepMB: 50, MaxNoProgress: 3, NoProgressBeforeFlush: 2,
		NoProgressBackoff: 0, // instant retries in test via fake wait
	})
	flushed := false
	prof := &TempdbProfile{FlushCaches: true, flushed: &flushed}

	_, _ = r.chunkLoop(ctx, mssql.FileSpace{Name: "tempdev", SizeMB: 1000, UsedMB: 500},
		1000, 500, ddl.ResolvedOptions{}, noIgnore, discardSink, prof)

	if got := exec.countMatching("FREESYSTEMCACHE ('Temporary Tables & Table Variables')"); got != 1 {
		t.Errorf("flush ran %d times, want exactly 1", got)
	}
	if !flushed {
		t.Errorf("flushed guard not set")
	}
}

func TestTempdb845IsRetryableNotFatal(t *testing.T) {
	// Exec returns Msg 845 on the first chunk, then succeeds thereafter.
	exec := &scriptedExec{errs: []error{mssql.Error{Number: 845, Message: "buffer latch time-out"}}}
	// Reader: first size read 1000, then shrinks 100 MB per successful chunk down to 500.
	reader := &steppingReader{sizes: []int{1000, 1000, 900, 800, 700, 600, 500}}
	r := newTestShrinkRunner(t, exec, reader, ShrinkTuning{
		MinStepMB: 50, MaxNoProgress: 3, NoProgressBeforeFlush: 2, NoProgressBackoff: 0,
	})
	prof := &TempdbProfile{flushed: new(bool)} // FlushCaches false: prove 845 alone is non-fatal

	res, err := r.chunkLoop(ctx, mssql.FileSpace{Name: "tempdev", SizeMB: 1000, UsedMB: 500},
		1000, 500, ddl.ResolvedOptions{}, noIgnore, discardSink, prof)

	if err != nil {
		t.Fatalf("845 must not be fatal, got err = %v", err)
	}
	if res.FinalMB >= 1000 {
		t.Errorf("expected progress after 845 retry, FinalMB = %d", res.FinalMB)
	}
}
```

> Study `shrink_driver_test.go` for the real fake names (`recordingExec`, `stuckReader`, `newTestShrinkRunner`, `discardSink`, `noIgnore` are illustrative — use the actual ones). Reuse the existing manual-clock/fake-wait harness so backoff does not sleep.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/run -run 'TestTempdb(ChunkLoopFlushes|845)' -v`
Expected: FAIL (no flush logic; 845 may propagate).

- [ ] **Step 3: Write minimal implementation**

Add the flush helper:

```go
// flushTempdbCaches releases the temp-object cachestore that pins tempdb pages,
// preceded by a CHECKPOINT. Targeted, not instance-wide: it deliberately avoids
// FREEPROCCACHE / FREESYSTEMCACHE('ALL') (whole-plan-cache recompile storm) and
// DROPCLEANBUFFERS (buffer-pool wipe). Runs on the tempdb-scoped exec connection.
func (r *ShrinkRunner) flushTempdbCaches(ctx context.Context, sink ReactionSink) error {
	sink(ReactionEvent{Kind: "pause", Detail: "tempdb stall: flushing temp-object cache (CHECKPOINT + FREESYSTEMCACHE)"})
	return r.exec.ExecDDL(ctx, "CHECKPOINT;\nDBCC FREESYSTEMCACHE ('Temporary Tables & Table Variables') WITH NO_INFOMSGS;")
}
```

In `stall`, add the escalation before the give-up/backoff decision (pass `prof` into `stall` — update its signature and the two call sites in `chunkLoop`):

```go
func (r *ShrinkRunner) stall(ctx context.Context, file string, noProgress *int, backoff, stallWaited *time.Duration, sink ReactionSink, prof *TempdbProfile) (stop bool, err error) {
	*noProgress++
	// tempdb escalation: once, when the stall is persistent and flushing is enabled.
	if prof != nil && prof.FlushCaches && prof.flushed != nil && !*prof.flushed && *noProgress >= r.tuning.NoProgressBeforeFlush {
		if ferr := r.flushTempdbCaches(ctx, sink); ferr != nil {
			return false, ferr
		}
		*prof.flushed = true
		*noProgress = 0 // give the freed pages a fresh budget
		return false, nil
	}
	if *noProgress >= r.tuning.MaxNoProgress || *stallWaited >= r.tuning.SelfWaitTimeout {
		return true, nil
	}
	sink(ReactionEvent{Kind: "pause", Detail: fmt.Sprintf("shrink %q made no progress; backing off %s", file, *backoff)})
	if werr := r.wait(ctx, *backoff); werr != nil {
		return false, werr
	}
	*stallWaited += *backoff
	*backoff = nextBackoff(*backoff, r.tuning.NoProgressBackoffMax)
	return false, nil
}
```

In `chunkLoop`, extend the chunk-error branch so 845 joins the Msg 5240 path:

```go
		if err != nil {
			if !mssql.IsFileAllocationError(err) && !mssql.IsBufferLatchTimeout(err) {
				return result, err
			}
			step = clampStep(step/2, r.tuning.MinStepMB, maxStep)
			blocked += r.clk.Since(t0)
			if stop, werr := r.stall(ctx, f.Name, &noProgress, &backoff, &stallWaited, sink, prof); werr != nil {
				return result, werr
			} else if stop {
				result.FinalMB = current
				result.Reason = "shrink could not adjust the file allocation (work preserved)"
				return result, nil
			}
			continue
		}
```

Update the no-gain `stall` call in `chunkLoop` to pass `prof` too.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/run -run 'TestTempdb' -v` then `go test -race ./internal/run`
Expected: PASS (new + all existing).

- [ ] **Step 5: Commit**

```bash
git add internal/run/shrink.go internal/run/shrink_driver_test.go
git commit -m "feat(run): tempdb cache-flush escalation and 845 retry in the stall ladder"
```

---

## Task 8: `RunTempdb` orchestrator + engine routing

**Files:**
- Modify: `internal/run/shrink.go` (add `RunTempdb`, `TempdbProfile.TargetSizeMB`, unbalanced check)
- Modify: `internal/run/engine.go` (`TempdbShrinkDriver` interface ~line 37; `processOne` switch ~line 652; `isShrinkOp` for stop-short ~line 750)
- Test: `internal/run/shrink_driver_test.go`, `internal/run/engine_*_test.go`

**Interfaces:**
- Consumes: `chunkLoop`, `runTruncateOnly`, `resolveFiles` (all in `shrink.go`); `ddl.ShrinkTempdb`.
- Produces:
  - `type TempdbShrinkDriver interface { RunTempdb(ctx context.Context, op ddl.ShrinkTempdb, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) ([]ShrinkResult, error) }`, satisfied by `*ShrinkRunner`.
  - `WithTempdbShrinkRunner(TempdbShrinkDriver) EngineOption` and engine field `e.tempdbShrink`.
  - `TempdbProfile` gains `TargetSizeMB int`.
  - `func isShrinkOp(op ddl.Operation) bool` (true for `ddl.Shrink` and `ddl.ShrinkTempdb`).

- [ ] **Step 1: Write the failing test**

```go
func TestRunTempdbTruncatesAllFilesBeforeChunking(t *testing.T) {
	// Fake reader lists 2 data files, each SizeMB 1000, UsedMB 100.
	// Record the order of ExecDDL statements; assert BOTH TRUNCATEONLY statements
	// appear before ANY page-moving "DBCC SHRINKFILE (<file>, <n>)" statement.
	exec := &recordingExec{}
	reader := &twoFileReader{}
	r := newTestShrinkRunner(t, exec, reader, defaultTestTuning())
	_, err := r.RunTempdb(ctx, ddl.ShrinkTempdb{TargetSizeMB: 200}, ddl.ResolvedOptions{}, noIgnore, discardSink)
	if err != nil {
		t.Fatalf("RunTempdb: %v", err)
	}
	assertTruncateOnlyBeforeAnyChunk(t, exec.statements)
}

func TestRunTempdbClampsTargetToUsed(t *testing.T) {
	// One file UsedMB 500 with TargetSizeMB 200 → its chunk target floor is 500, not 200.
	// Assert no chunk statement targets below 500 for that file.
}

func TestRunTempdbWarnsOnUnbalancedFinalSizes(t *testing.T) {
	// Two files end at different sizes → a sink event of kind "tempdb" (warning)
	// containing "Unbalanced tempdb files" is emitted.
}

func TestIsShrinkOpCoversBoth(t *testing.T) {
	if !isShrinkOp(ddl.Shrink{}) || !isShrinkOp(ddl.ShrinkTempdb{}) {
		t.Fatal("isShrinkOp must be true for both shrink ops")
	}
	if isShrinkOp(ddl.CheckDB{}) {
		t.Fatal("isShrinkOp false positive")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/run -run 'TestRunTempdb|TestIsShrinkOp' -v`
Expected: FAIL (undefined `RunTempdb`/`isShrinkOp`).

- [ ] **Step 3: Write minimal implementation**

Add `TargetSizeMB int` to `TempdbProfile`. Add `RunTempdb`:

```go
// RunTempdb shrinks every tempdb data file to a common absolute target, two-phase:
// Phase 0 runs TRUNCATEONLY on all files (free tails returned to the OS first), then
// Phase 1 runs the shared chunkLoop per file. The per-file target is clamped to the
// file's used space. It reuses this runner's exec/reader, which the wiring binds to a
// tempdb-scoped connection. See specs/TEMPDB-SHRINK.md.
func (r *ShrinkRunner) RunTempdb(ctx context.Context, op ddl.ShrinkTempdb, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) ([]ShrinkResult, error) {
	files, err := r.resolveFiles(ctx, ddl.Shrink{Type: "data", Files: "all"})
	if err != nil {
		return nil, err
	}
	flushed := false
	prof := &TempdbProfile{FlushCaches: op.FlushCaches, TargetSizeMB: op.TargetSizeMB, flushed: &flushed}

	// State the total explicitly (spec §6): targetsizemb is PER FILE, easy to misread.
	sink(ReactionEvent{Kind: "tempdb", Detail: fmt.Sprintf(
		"shrinking %d tempdb data files to %d MB each (total target %d MB)",
		len(files), op.TargetSizeMB, len(files)*op.TargetSizeMB)})

	// Phase 0 — TRUNCATEONLY on all files first.
	for _, f := range files {
		if stopRequested(r.stop) {
			return nil, ErrStopped
		}
		if stopped, terr := r.runTruncateOnly(ctx, f.Name, sink); terr != nil {
			return nil, fmt.Errorf("shrink_tempdb %q: truncateonly: %w", f.Name, terr)
		} else if stopped {
			return nil, ErrStopped
		}
	}

	// Phase 1 — per-file chunk loop.
	results := make([]ShrinkResult, 0, len(files))
	for _, f := range files {
		if stopRequested(r.stop) {
			return results, ErrStopped
		}
		size, ferr := r.reader.FileSizeMB(ctx, f.Name)
		if ferr != nil {
			return results, ferr
		}
		final := op.TargetSizeMB
		if final < f.UsedMB {
			final = f.UsedMB // clamp: cannot shrink below used
		}
		f.SizeMB = size
		if size <= final {
			results = append(results, ShrinkResult{File: f.Name, Type: "data", InitialMB: size, TargetMB: final, FinalMB: size, NoOp: true, Reason: "already at or below target"})
			continue
		}
		res2, cerr := r.chunkLoop(ctx, f, size, final, res, ignore, sink, prof)
		if cerr != nil {
			if errors.Is(cerr, ErrStopped) {
				results = append(results, res2)
			}
			return results, cerr
		}
		results = append(results, res2)
	}
	warnIfUnbalanced(results, sink)
	return results, nil
}

// warnIfUnbalanced emits a warning when the data files did not all end at the same
// whole-MB size: an asymmetric tempdb defeats proportional fill (see spec §6).
func warnIfUnbalanced(results []ShrinkResult, sink ReactionSink) {
	first := -1
	for _, r := range results {
		if first == -1 {
			first = r.FinalMB
		} else if r.FinalMB != first {
			sink(ReactionEvent{Kind: "tempdb", Detail: "Unbalanced tempdb files: data files ended at different sizes; proportional fill will skew — re-run or intervene"})
			return
		}
	}
}
```

In `engine.go`, add the interface, option, field, and routing:

```go
// TempdbShrinkDriver runs a shrink_tempdb operation. *ShrinkRunner satisfies it;
// it is wired to a tempdb-scoped connection so its DBCC/reads run in tempdb context.
type TempdbShrinkDriver interface {
	RunTempdb(ctx context.Context, op ddl.ShrinkTempdb, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) ([]ShrinkResult, error)
}
```

Add `tempdbShrink TempdbShrinkDriver` to `Engine`, a `WithTempdbShrinkRunner` option, and in the `processOne` switch after `case ddl.Shrink:`:

```go
			case ddl.ShrinkTempdb:
				if e.tempdbShrink != nil {
					shrinkResults, runErr = e.tempdbShrink.RunTempdb(ctx, op, res, ignore, sink)
				} else {
					runErr = fmt.Errorf("shrink_tempdb requires a tempdb shrink runner (not configured)")
				}
```

> Match the exact locals the `case ddl.Shrink:` arm uses (`shrinkResults`, `res`, `ignore`, `sink`) — read lines ~652-700 first and mirror them.

Replace the stop-short type assertion (~line 750) with the predicate:

```go
		if isShrinkOp(step.Operation) && shrinkStoppedShort(shrinkResults) {
```

Add:

```go
func isShrinkOp(op ddl.Operation) bool {
	switch op.(type) {
	case ddl.Shrink, ddl.ShrinkTempdb:
		return true
	default:
		return false
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/run`
Expected: PASS (new + existing).

- [ ] **Step 5: Commit**

```bash
git add internal/run/shrink.go internal/run/engine.go internal/run/*_test.go
git commit -m "feat(run): RunTempdb two-phase orchestrator and engine routing"
```

---

## Task 9: Preflight — recognize `shrink_tempdb`

**Files:**
- Modify: `internal/preflight/preflight.go` (`requiresElevatedRights` ~line 124; `CheckOperation` ~line 170; `objectExistence` ~line 322)
- Test: `internal/preflight/preflight_test.go`

**Why this is required:** `CheckOperation` and `objectExistence` only skip the `schema.table` existence check for `ddl.CheckDB, ddl.Shrink`. `ShrinkTempdb` has empty `Schema`/`Table`, so without adding it to these switches it falls through and **fails preflight with `table [].[] does not exist`** — the exact regression CLAUDE.md records was fixed for shrink/check_db in 028602a. It also issues `DBCC SHRINKFILE`, so it needs db_owner/sysadmin like the others.

**Interfaces:**
- Consumes: `ddl.ShrinkTempdb` (Task 1).
- Produces: preflight passes `shrink_tempdb` with "no table precondition (database/file-scoped)" and includes it in the elevated-rights probe.

- [ ] **Step 1: Write the failing test**

```go
func TestPreflightShrinkTempdbSkipsTableCheck(t *testing.T) {
	op := ddl.ShrinkTempdb{TargetSizeMB: 20480}
	// objectExistence must not consult the table; CheckOperation must Pass.
	c := CheckOperation(op, false /*tableExists*/, false /*targetExists*/)
	if c.Severity != Pass {
		t.Fatalf("CheckOperation = %v, want Pass (database/file-scoped)", c)
	}
}

func TestShrinkTempdbNeedsElevatedRights(t *testing.T) {
	if !requiresElevatedRights(ddl.ShrinkTempdb{}) {
		t.Fatal("shrink_tempdb issues DBCC SHRINKFILE; must require db_owner/sysadmin")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/preflight -run 'TestPreflightShrinkTempdb|TestShrinkTempdbNeedsElevated' -v`
Expected: FAIL (CheckOperation returns Fail "table [].[] does not exist"; `requiresElevatedRights` false).

- [ ] **Step 3: Write minimal implementation**

Add `ddl.ShrinkTempdb` to all three switches:

```go
// requiresElevatedRights (~line 124)
	case ddl.CheckDB, ddl.Shrink, ddl.ShrinkTempdb:
		return true
```

```go
// CheckOperation (~line 170)
	case ddl.CheckDB, ddl.Shrink, ddl.ShrinkTempdb:
		return Check{fmt.Sprintf("%s %s", op.CommandType(), ref), Pass, "no table precondition (database/file-scoped)"}
```

```go
// objectExistence (~line 322)
	case ddl.CheckDB, ddl.Shrink, ddl.ShrinkTempdb:
		return true, true, nil
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/preflight`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/preflight/preflight.go internal/preflight/preflight_test.go
git commit -m "feat(preflight): recognize shrink_tempdb (skip table check, require db_owner)"
```

---

## Task 10: tempdb-scoped connection + main.go wiring

**Files:**
- Modify: `internal/mssql/conn.go` (add `OpenScoped` / DSN-with-database helper)
- Modify: `cmd/sqlgopace/main.go` (open tempdb conn; build + wire the tempdb runner)
- Test: `internal/mssql/*_integration_test.go` (behind `integration` tag) — optional but recommended

**Interfaces:**
- Consumes: existing `mssql.Open`/`Conn` construction; `run.NewShrinkRunner`; `run.WithTempdbShrinkRunner` (Task 8).
- Produces: a `*mssql.Conn` whose connection context is `tempdb`, wired as exec+reader of a second `ShrinkRunner` passed to `WithTempdbShrinkRunner`.

- [ ] **Step 1: Read the current open path**

Run: read `internal/mssql/conn.go` fully and the `main.go` block that builds `conn` and `shrinkRunner` (lines ~410-455). Determine how the DSN/connection string is parsed so a `database=tempdb` override can be produced (the driver accepts a `Database` in the connection string / `msdsn` parse).

- [ ] **Step 2: Add `OpenScoped`**

In `conn.go`, add a constructor that opens a `*Conn` with the database overridden (parse the existing connection string via `msdsn.Parse`, set `p.Database = db`, re-serialize, open). Signature:

```go
// OpenScoped opens a second connection identical to the primary but with its
// database context set to db (used to run tempdb shrinks in tempdb context).
func OpenScoped(ctx context.Context, connString, db string, opts ...Option) (*Conn, error)
```

Reuse the existing `Open` internals; only the database segment of the DSN changes.

- [ ] **Step 3: Wire in main.go**

After the primary `shrinkRunner` is built (~line 423), open the tempdb connection and build the second runner. `OpenScoped` connects immediately, so treat its error as fatal to startup (like the primary `conn`):

```go
	tempdbConn, err := mssql.OpenScoped(ctx, cfg.Database.ConnectionString, "tempdb")
	if err != nil {
		return fmt.Errorf("open tempdb connection: %w", err)
	}
	defer tempdbConn.Close()
	tempdbShrinkRunner := run.NewShrinkRunner(tempdbConn, tempdbConn, sampler, run.System, run.ShrinkRunnerConfig{
		Tuning:          shrinkTuning(cfg.Shrink),
		PollInterval:    cfg.Monitoring.BlockingPoll(),
		LogPollInterval: cfg.Monitoring.LogPoll(),
		BlockingTimeout: cfg.Monitoring.BlockingTimeout(),
		LogDrainTimeout: cfg.Monitoring.LogDrainTimeout(),
		KillGrace:       cfg.Monitoring.KillGrace(),
	}, /* same WithShrinkProgress / WithShrinkStop opts as the primary runner */)
```

Add to the engine option list (~line 452):

```go
		run.WithTempdbShrinkRunner(tempdbShrinkRunner),
```

> The tempdb runner shares the primary `sampler` (instance-wide DMVs); only exec+reader are tempdb-scoped. Mirror the primary runner's `WithShrinkProgress`/`WithShrinkStop` options so the TUI and graceful stop work identically.

- [ ] **Step 4: Build + smoke**

Run: `make build && go vet ./... && golangci-lint run`
Expected: builds clean, vet/lint clean. (Stop any running `bin/sqlgopace.exe` first on Windows.)

Optional integration check (needs `SQLGOPACE_TEST_DSN` on a throwaway instance): a manifest with `operation: shrink_tempdb` + `targetsizemb` runs, connects to tempdb, and reports per-file results.

- [ ] **Step 5: Commit**

```bash
git add internal/mssql/conn.go cmd/sqlgopace/main.go internal/mssql/*_integration_test.go
git commit -m "feat: wire tempdb-scoped connection for shrink_tempdb"
```

---

## Task 11: Documentation + version bump

**Files:**
- Modify: `README.md` (operations section — add `shrink_tempdb`)
- Modify: `.claude/skills/sqlgopace-operator/SKILL.md` (if it enumerates operations)
- Modify: `internal/version/VERSION` (bump minor, e.g. `0.6.0` → `0.7.0`)
- Test: none (docs) — but run `make test` to confirm the tree is green before tagging.

- [ ] **Step 1: Document the operation**

In `README.md`, add a `shrink_tempdb` subsection near `shrink`:
- Manifest example (`targetsizemb`, `flushcaches`).
- Non-goals: not a monitor, not guaranteed, data files only.
- The `flushcaches` trade-off (targeted flush; `('ALL')`/`FREEPROCCACHE` deliberately excluded); note the `aggressive` `('ALL')` widening is deferred out of v1.
- Never kills blockers; WALP is `SELF`-only and 2022+.
- The `Unbalanced tempdb files` warning and what it means.

- [ ] **Step 2: Bump the version**

Set `internal/version/VERSION` to the next minor.

- [ ] **Step 3: Full green check**

Run: `make test && make vet && make lint`
Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add README.md .claude/skills/sqlgopace-operator/SKILL.md internal/version/VERSION
git commit -m "docs: document shrink_tempdb; bump version"
```

---

## Post-implementation

- Run the `/simplify` pass over the full diff (CLAUDE.md convention) before merging: collapse any duplication that accreted, especially between `shrinkData` and `RunTempdb`.
- Request code review (superpowers:requesting-code-review) focusing on: the two-phase order, the once-per-run flush guard across files, the never-kill-blocker guarantee, and the tempdb connection scoping.
- Merge `feat/shrink-tempdb` per superpowers:finishing-a-development-branch.
