# Operation Intent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `rebuild_index` an `intent` field (`compression` | `fragmentation`) so the engine skips an already-compressed index only when the goal was compression, and retire the manifest-level `skip_if_satisfied` flag it replaces.

**Architecture:** `intent` is a per-operation field on `ddl.RebuildIndex` plus a manifest-level default (`Manifest.Intent`) resolved at the single engine call site, never at load. The planner stamps it from the `fragRebuild` boolean it already computes. `skipSatisfied` gates on effective intent instead of the boolean flag. `skip_if_satisfied` is then removed entirely — manifest decoding already rejects unknown keys, so a stale flag fails to load.

**Tech Stack:** Go 1.26+, `gopkg.in/yaml.v3`, `github.com/google/go-cmp`. Tests are pure (no database) and run with `-race`.

**Spec:** `docs/specs/OPERATION-INTENT.md`.

## Global Constraints

- **Idiomatic Go, KISS.** Plain Go matching the surrounding code; no new layers or abstractions the task does not need.
- **English only** — all code, comments, identifiers. US spelling.
- **No query timeout.** Do not add `context.WithTimeout` anywhere near executing DDL. (Not touched here, but the rule stands.)
- **Version** lives in `internal/version/VERSION`; do not touch it.
- **`intent` values are exactly `compression` and `fragmentation`.** Any other value is a validation error. Unset means "unknown" and **runs** (never skips) — wasted work is recoverable, a silent skip is not.
- **`data_compression` is NOT defaultable at the manifest level** (only `intent` is): its empty value is a deliberate "defrag, no compression change", not "unset".
- Windows note: `bin/sqlgopace.exe` is locked while running; stop any running instance before a rebuild. Not needed for these tasks (tests only).

---

## Task Order and Dependencies

1. **Task 1** — `Intent` type + `RebuildIndex.Intent` field + `Validate` (internal/ddl). Additive.
2. **Task 2** — `Manifest.Intent` default: field, parse, marshal, validate, round-trip (internal/ddl). Additive, depends on Task 1.
3. **Task 3** — planner stamps intent in `decideIndex` (internal/maint). Additive, depends on Task 1.
4. **Task 4** — `effectiveIntent` + `skipSatisfied` gates on intent; migrate the skip tests; add the fragmentation-intent regression (internal/run). Behavior change, depends on Tasks 1–2.
5. **Task 5** — remove `skip_if_satisfied` (internal/ddl, run, report, mssql). Cleanup, depends on Task 4.
6. **Task 6** — docs: README + spec cross-references. Depends on Task 5.

Each task ends green (`go build ./... && go test -race ./...`) and is committed.

---

### Task 1: The `Intent` type and `RebuildIndex.Intent` field

**Files:**
- Modify: `internal/ddl/manifest.go` (add `Intent` type near `OnFailure` ~line 120; add field to `RebuildIndex` ~line 542; extend `RebuildIndex.Validate` ~line 561)
- Test: `internal/ddl/manifest_intent_test.go` (create)
- Test: `internal/ddl/expand_test.go` (extend `TestExpandRebuildAllCarriesEveryField`)

**Interfaces:**
- Produces: `ddl.Intent` (string type); `ddl.IntentCompression`, `ddl.IntentFragmentation` constants; `ddl.RebuildIndex.Intent Intent` field (yaml `intent,omitempty`). `RebuildIndex.Validate()` rejects any intent value other than the two constants or empty.

- [ ] **Step 1: Write the failing test**

Create `internal/ddl/manifest_intent_test.go`:

```go
package ddl_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestParseRebuildIndexIntent(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want ddl.Intent
	}{
		{"unset", "", ""},
		{"compression", "    intent: compression\n", ddl.IntentCompression},
		{"fragmentation", "    intent: fragmentation\n", ddl.IntentFragmentation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n" + tt.yaml
			m, err := ddl.ParseManifest(strings.NewReader(src))
			if err != nil {
				t.Fatalf("ParseManifest() error = %v", err)
			}
			got := m.Operations[0].(ddl.RebuildIndex).Intent
			if got != tt.want {
				t.Errorf("Intent = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRebuildIndexRejectsUnknownIntent(t *testing.T) {
	src := "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n    intent: banana\n"
	_, err := ddl.ParseManifest(strings.NewReader(src))
	if err == nil {
		t.Fatal("ParseManifest() error = nil, want an invalid-intent error")
	}
	if !errors.Is(err, ddl.ErrInvalidManifest) {
		t.Errorf("error is not ErrInvalidManifest: %v", err)
	}
	if !strings.Contains(err.Error(), "banana") {
		t.Errorf("error does not name the offending value: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ddl -run 'TestParseRebuildIndexIntent|TestRebuildIndexRejectsUnknownIntent' -v`
Expected: FAIL — `m.Operations[0].(ddl.RebuildIndex).Intent` is a compile error (`Intent` undefined), so the package does not build.

- [ ] **Step 3: Add the type, the field, and validation**

In `internal/ddl/manifest.go`, add the type just after the `OnFailure` const block (after its closing `)`):

```go
// Intent records why a rebuild was scheduled. A rebuild both applies a compression
// target (a state, idempotent) and defragments (an act, never idempotent); only the
// manifest knows which motivated it, and the engine cannot skip correctly without
// being told. Empty means "unknown" and always runs.
type Intent string

const (
	IntentCompression   Intent = "compression"
	IntentFragmentation Intent = "fragmentation"
)
```

In the `RebuildIndex` struct, add the field after `DataCompression` and before `Options`:

```go
	DataCompression string          `yaml:"data_compression"`
	Intent          Intent          `yaml:"intent,omitempty"`
	Options         OptionOverrides `yaml:"options"`
```

Extend `RebuildIndex.Validate` to check the intent:

```go
func (o RebuildIndex) Validate() error {
	if err := requireFields("rebuild_index", map[string]string{
		"schema": o.Schema, "table": o.Table, "index": o.Index,
	}); err != nil {
		return err
	}
	return validateIntent(o.Intent)
}
```

Add the helper near the other validation helpers (e.g. after `requireFields`):

```go
// validateIntent accepts an empty intent (unset) or one of the two constants.
func validateIntent(i Intent) error {
	switch i {
	case "", IntentCompression, IntentFragmentation:
		return nil
	default:
		return fmt.Errorf("intent must be %q or %q, got %q: %w",
			IntentCompression, IntentFragmentation, i, ErrInvalidManifest)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ddl -run 'TestParseRebuildIndexIntent|TestRebuildIndexRejectsUnknownIntent' -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Extend the expansion guard so intent carries through `index: ALL`**

In `internal/ddl/expand_test.go`, in `TestExpandRebuildAllCarriesEveryField`, add `Intent` to the `src` literal so the whole-struct diff covers it:

```go
	src := ddl.RebuildIndex{
		Schema: "dbo", Table: "BigFact", Index: "ALL",
		Partition:       intPtr(3),
		DataCompression: "PAGE",
		Intent:          ddl.IntentCompression,
		Options:         ddl.OptionOverrides{MaxDOP: intPtr(4)},
	}
```

- [ ] **Step 6: Run the expansion test**

Run: `go test ./internal/ddl -run TestExpandRebuildAllCarriesEveryField -v`
Expected: PASS — `expandedRebuild` copies the whole struct (`op := src`), so `Intent` survives without code change. (If it FAILS, `expandedRebuild` regressed to a field-by-field copy — restore `op := src`.)

- [ ] **Step 7: Full package check and commit**

Run: `go build ./... && go test -race ./internal/ddl`
Expected: PASS.

```bash
git add internal/ddl/manifest.go internal/ddl/manifest_intent_test.go internal/ddl/expand_test.go
git commit -m "feat(ddl): add intent to rebuild_index

An intent of compression | fragmentation records why a rebuild was scheduled;
empty means unknown. Validate rejects any other value. Expansion already copies
the whole operation, so intent survives index: ALL.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: The manifest-level `intent` default

**Files:**
- Modify: `internal/ddl/manifest.go` (`Manifest` struct; `UnmarshalYAML` raw struct + assignment; `Manifest.Validate`)
- Modify: `internal/ddl/render.go` (`MarshalManifest` — emit `intent`)
- Test: `internal/ddl/manifest_intent_test.go` (extend)
- Test: `internal/ddl/render_test.go` (extend the round-trip fixture)

**Interfaces:**
- Consumes: `ddl.Intent`, `ddl.IntentCompression` (Task 1).
- Produces: `ddl.Manifest.Intent Intent` field. Parsed from top-level `intent:`, validated against the two constants, emitted by `MarshalManifest` when non-empty.

- [ ] **Step 1: Write the failing test**

Append to `internal/ddl/manifest_intent_test.go`:

```go
func TestParseManifestIntentDefault(t *testing.T) {
	src := "intent: compression\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
	m, err := ddl.ParseManifest(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}
	if m.Intent != ddl.IntentCompression {
		t.Errorf("Manifest.Intent = %q, want %q", m.Intent, ddl.IntentCompression)
	}
	// The default is NOT pushed into the operation: the op keeps its own empty intent.
	if got := m.Operations[0].(ddl.RebuildIndex).Intent; got != "" {
		t.Errorf("operation Intent = %q, want empty (default resolves at use, not at load)", got)
	}
}

func TestManifestRejectsUnknownIntent(t *testing.T) {
	_, err := ddl.ParseManifest(strings.NewReader("intent: banana\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"))
	if err == nil {
		t.Fatal("ParseManifest() error = nil, want an invalid manifest-intent error")
	}
	if !strings.Contains(err.Error(), "banana") {
		t.Errorf("error does not name the offending value: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ddl -run 'TestParseManifestIntentDefault|TestManifestRejectsUnknownIntent' -v`
Expected: FAIL — `m.Intent` is a compile error (`Manifest` has no `Intent` field).

- [ ] **Step 3: Add the field, parse it, validate it**

In `internal/ddl/manifest.go`, add to the `Manifest` struct (after `OnFailure`, so it reads naturally):

```go
	OnFailure   OnFailure // empty defaults to stop (fail-fast)
	// Intent is the default recorded on every rebuild_index that sets none of its own
	// (see Intent). It is resolved where it is used, not pushed into operations at load.
	Intent Intent
```

In `UnmarshalYAML`, add to the `raw` struct:

```go
		OnFailure              string           `yaml:"on_failure"`
		Intent                 string           `yaml:"intent"`
```

and the assignment (after `m.OnFailure = ...`):

```go
	m.Intent = Intent(strings.TrimSpace(raw.Intent))
```

In `Manifest.Validate`, add the check alongside the `OnFailure` switch (right after it):

```go
	if err := validateIntent(m.Intent); err != nil {
		return err
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ddl -run 'TestParseManifestIntentDefault|TestManifestRejectsUnknownIntent' -v`
Expected: PASS.

- [ ] **Step 5: Emit the manifest-level intent, and cover it in the round-trip**

In `internal/ddl/render.go`, in `MarshalManifest`, add after the `on_failure` block:

```go
	if m.OnFailure != "" {
		addPair(root, "on_failure", scalarNode(string(m.OnFailure)))
	}
	if m.Intent != "" {
		addPair(root, "intent", scalarNode(string(m.Intent)))
	}
```

In `internal/ddl/render_test.go`, in `TestMarshalManifestRoundTrip`, add `Intent` to the `in` manifest and an operation-level intent so both round-trip:

```go
		OnFailure:              ddl.OnFailureContinue,
		Intent:                 ddl.IntentCompression,
```

and change the first rebuild operation to carry its own intent:

```go
			ddl.RebuildIndex{Schema: "dbo", Table: "ORDERS", Index: "PK_ORDERS", DataCompression: "PAGE", Intent: ddl.IntentFragmentation},
```

- [ ] **Step 6: Run the render tests**

Run: `go test ./internal/ddl -run 'TestMarshalManifestRoundTrip|TestMarshalManifestOmitsEmpty' -v`
Expected: PASS. (`TestMarshalManifestOmitsEmpty` still passes — its manifest sets no intent, and `omitempty` + `compact` keep `intent:` out.)

- [ ] **Step 7: Full package check and commit**

Run: `go build ./... && go test -race ./internal/ddl`
Expected: PASS.

```bash
git add internal/ddl/manifest.go internal/ddl/render.go internal/ddl/manifest_intent_test.go internal/ddl/render_test.go
git commit -m "feat(ddl): manifest-level intent default

A top-level intent: is the default for every rebuild_index that sets none of its
own. It is stored on the manifest and resolved at use, not pushed into operations
at load, so the marshal/parse round-trip stays exact. MarshalManifest emits it.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: The planner stamps intent

**Files:**
- Modify: `internal/maint/decide.go` (`decideIndex`, the `RebuildIndex` construction ~line 242-251)
- Test: `internal/maint/decide_intent_test.go` (create)

**Interfaces:**
- Consumes: `ddl.RebuildIndex.Intent`, `ddl.IntentCompression`, `ddl.IntentFragmentation` (Task 1).
- Produces: every `rebuild_index` emitted by `decideIndex` carries `Intent`: `IntentCompression` when the rebuild is compression-only (`!fragRebuild`), `IntentFragmentation` otherwise (pure-frag or dual-motive).

- [ ] **Step 1: Write the failing test**

Create `internal/maint/decide_intent_test.go`:

```go
package maint_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
)

func TestDecideIndexStampsIntent(t *testing.T) {
	p := baseProfile(t)
	tests := []struct {
		name string
		m    maint.IndexMeasurement
		want ddl.Intent
	}{
		{
			name: "compression-only (low frag, PAGE gain)",
			m: maint.IndexMeasurement{Schema: "dbo", Table: "T", Index: "IX", PageCount: 5000, SizeMB: 100,
				FragmentationPercent: 2, Current: maint.CompressionNone,
				Estimate: &maint.CompressionEstimate{CurrentKB: 100, RowKB: 70, PageKB: 50}},
			want: ddl.IntentCompression,
		},
		{
			name: "pure fragmentation (high frag, no estimate)",
			m:    bigIndex(42),
			want: ddl.IntentFragmentation,
		},
		{
			name: "dual motive (high frag AND PAGE gain)",
			m: maint.IndexMeasurement{Schema: "dbo", Table: "T", Index: "IX", PageCount: 5000, SizeMB: 100,
				FragmentationPercent: 42, Current: maint.CompressionNone,
				Estimate: &maint.CompressionEstimate{CurrentKB: 100, RowKB: 70, PageKB: 50}},
			want: ddl.IntentFragmentation,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := maint.DecideIndex(tt.m, p)
			if d.Kind != "rebuild_index" {
				t.Fatalf("Kind = %q, want rebuild_index (reason: %s)", d.Kind, d.Reason)
			}
			if got := d.Op.(ddl.RebuildIndex).Intent; got != tt.want {
				t.Errorf("Intent = %q, want %q", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maint -run TestDecideIndexStampsIntent -v`
Expected: FAIL — every case reports `Intent = ""` (the planner does not set it yet).

- [ ] **Step 3: Stamp intent in `decideIndex`**

In `internal/maint/decide.go`, in `decideIndex`, fold intent into the existing `!fragRebuild` block and add it to the construction:

```go
	reason := fmt.Sprintf("fragmentation %.0f%%", frag)
	intent := ddl.IntentFragmentation
	if !fragRebuild { // rebuild is purely compression-motivated
		reason = "compression change"
		intent = ddl.IntentCompression
	}
	reason += "; " + comp.reason
	op := ddl.RebuildIndex{
		Schema: m.Schema, Table: m.Table, Index: m.Index,
		Partition: m.Partition, DataCompression: dataCompression, Intent: intent,
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/maint -run TestDecideIndexStampsIntent -v`
Expected: PASS.

- [ ] **Step 5: Full package check and commit**

Run: `go build ./... && go test -race ./internal/maint`
Expected: PASS (existing decide tests unaffected — they assert `Kind`/`DataCompression`, not `Intent`).

```bash
git add internal/maint/decide.go internal/maint/decide_intent_test.go
git commit -m "feat(maint): planner stamps rebuild intent

decideIndex sets IntentCompression when the rebuild is compression-only and
IntentFragmentation otherwise (pure-frag or dual-motive), from the fragRebuild
boolean it already computes. Generated manifests now record why each line is there.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: The engine gates on intent

**Files:**
- Modify: `internal/run/skip.go` (`skipSatisfied` signature and body; add `effectiveIntent`)
- Modify: `internal/run/engine.go` (call site ~line 480)
- Test: `internal/run/skip_engine_test.go` (migrate fixture + 3 tests; add the regression)
- Test: `internal/run/resume_test.go` (migrate `TestSkipIfSatisfiedDoesNotOrphanOwnPausedResumable`)

**Interfaces:**
- Consumes: `ddl.Manifest.Intent`, `ddl.RebuildIndex.Intent`, `ddl.IntentCompression` (Tasks 1–2).
- Produces: `skipSatisfied(ctx, manifestIntent ddl.Intent, op ddl.Operation) (string, bool)` — skips only when the operation's **effective** intent (its own, else the manifest default) is `compression`. `effectiveIntent(manifestIntent ddl.Intent, op ddl.RebuildIndex) ddl.Intent`.
- Note: after this task `Manifest.SkipIfSatisfied` still exists but is no longer read by the engine; Task 5 removes it.

- [ ] **Step 1: Migrate the fixture and write the new/failing tests**

In `internal/run/skip_engine_test.go`, replace the `skipCompressManifest` const so the manifest declares compression intent instead of the flag:

```go
const skipCompressManifest = `
description: skip test
intent: compression
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
    data_compression: PAGE
`
```

Rewrite `TestSkipIfSatisfiedOffByDefault` (the `strings.Replace` no longer matches) to prove that **no intent** runs even when satisfied, and add the regression test that a fragmentation intent runs even when satisfied. Replace the whole `TestSkipIfSatisfiedOffByDefault` function with:

```go
func TestSkipUnsetIntentRunsEvenWhenSatisfied(t *testing.T) {
	// No intent → unknown → runs, even at target. Skipping is opt-in via intent: compression.
	runner := &fakeOpRunner{}
	comp := &fakeCompression{parts: []mssql.PartitionCompression{{Partition: 1, Desc: "PAGE"}}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithCompressionReader(comp))
	manifest := strings.Replace(skipCompressManifest, "intent: compression\n", "", 1)
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("runner ran %d times, want 1 (unset intent runs)", runner.calls)
	}
}

func TestFragmentationIntentRunsEvenWhenSatisfied(t *testing.T) {
	// The regression this feature exists to prevent: a rebuild whose compression already
	// holds but whose intent is fragmentation MUST run — the defrag still needs doing.
	runner := &fakeOpRunner{}
	comp := &fakeCompression{parts: []mssql.PartitionCompression{{Partition: 1, Desc: "PAGE"}}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithCompressionReader(comp))
	manifest := strings.Replace(skipCompressManifest, "intent: compression\n", "intent: fragmentation\n", 1)
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("runner ran %d times, want 1 (fragmentation intent must run despite matching compression)", runner.calls)
	}
}

func TestOperationIntentBeatsManifestDefault(t *testing.T) {
	// Manifest default is compression, but the operation overrides to fragmentation → runs.
	runner := &fakeOpRunner{}
	comp := &fakeCompression{parts: []mssql.PartitionCompression{{Partition: 1, Desc: "PAGE"}}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithCompressionReader(comp))
	manifest := strings.Replace(skipCompressManifest,
		"    data_compression: PAGE\n",
		"    data_compression: PAGE\n    intent: fragmentation\n", 1)
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("runner ran %d times, want 1 (operation intent overrides the manifest default)", runner.calls)
	}
}
```

Rename the two kept tests so no test in the tree still names the removed flag (Task 5 relies on a clean grep). Their bodies are unchanged — the migrated fixture (`intent: compression`) makes the first skip and the second run (current NONE ≠ PAGE), exactly as before:

- `TestSkipIfSatisfiedSkipsSatisfiedRebuild` → `TestCompressionIntentSkipsSatisfiedRebuild`
- `TestSkipIfSatisfiedRunsWhenNotSatisfied` → `TestCompressionIntentRunsWhenNotSatisfied`

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/run -run 'TestSkip|TestFragmentationIntent|TestOperationIntent' -v`
Expected: FAIL — the engine still gates on `manifest.SkipIfSatisfied`, which the migrated fixtures no longer set, so `TestSkipIfSatisfiedSkipsSatisfiedRebuild` runs the op (want 0 calls) and the two new "runs" tests may pass for the wrong reason. The build still compiles.

- [ ] **Step 3: Switch `skipSatisfied` to intent and add `effectiveIntent`**

Also rename the resume test in this task (see Step 4). In `internal/run/skip.go`, replace `skipSatisfied` and add `effectiveIntent`:

```go
// effectiveIntent resolves an operation's intent against the manifest default:
// the operation's own intent if set, otherwise the manifest's.
func effectiveIntent(manifestIntent ddl.Intent, op ddl.RebuildIndex) ddl.Intent {
	if op.Intent != "" {
		return op.Intent
	}
	return manifestIntent
}

// skipSatisfied reports whether an operation can be skipped because its target state
// already holds, returning a short reason for the log. Only a rebuild_index whose
// effective intent is compression and whose data_compression every relevant partition
// already has is eligible; then the rebuild is a no-op, so a re-run after an
// interruption reuses the finished work cheaply. A read error is treated as "not
// satisfied" (do the rebuild), never a hard failure.
func (e *Engine) skipSatisfied(ctx context.Context, manifestIntent ddl.Intent, op ddl.Operation) (string, bool) {
	if e.compression == nil {
		return "", false
	}
	ri, ok := op.(ddl.RebuildIndex)
	if !ok || ri.DataCompression == "" || effectiveIntent(manifestIntent, ri) != ddl.IntentCompression {
		return "", false
	}
	parts, err := e.compression.IndexCompression(ctx, ri.Schema, ri.Table, ri.Index)
	if err != nil || !compressionSatisfied(parts, ri.DataCompression, ri.Partition) {
		return "", false
	}
	return "already " + strings.ToUpper(ri.DataCompression), true
}
```

In `internal/run/engine.go`, change the call site (~line 480). Replace the comment and call:

```go
		// intent: compression — a rebuild whose target compression already holds is a no-op,
		// unless this operation left its own paused resumable, which must be resumed/finished
		// rather than skipped (skipping would orphan it paused on the server). Emitting only
		// the finished event keeps a re-run's log to one line per skipped operation.
		if reason, skip := e.skipSatisfied(ctx, manifest.Intent, step.Operation); skip && !ownsPausedResumable(st.Paused, i, step.Operation) {
```

- [ ] **Step 4: Migrate the resume test**

In `internal/run/resume_test.go`, `TestSkipIfSatisfiedDoesNotOrphanOwnPausedResumable` uses `skipCompressManifest`, which now carries `intent: compression` — so its body is unchanged. Rename it (so no test names the removed flag) and update its comment:

```go
func TestCompressionSkipDoesNotOrphanOwnPausedResumable(t *testing.T) {
	// #6: an intent: compression rebuild already at its target compression would be skipped,
	// but this op left its own paused resumable — skipping would orphan it. It must RESUME.
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/run -run 'TestSkip|TestFragmentationIntent|TestOperationIntent' -v`
Expected: PASS, including `TestFragmentationIntentRunsEvenWhenSatisfied`.

- [ ] **Step 6: Full package check and commit**

Run: `go build ./... && go test -race ./internal/run`
Expected: PASS.

```bash
git add internal/run/skip.go internal/run/engine.go internal/run/skip_engine_test.go internal/run/resume_test.go
git commit -m "feat(run): gate the compression skip on intent, not a flag

skipSatisfied now skips only when the operation's effective intent (its own,
else the manifest default) is compression. A fragmentation rebuild whose
compression already matches runs anyway — the defrag still needs doing. The
manifest-level skip_if_satisfied flag is no longer read; Task 5 removes it.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Remove `skip_if_satisfied`

**Files:**
- Modify: `internal/ddl/manifest.go` (delete `Manifest.SkipIfSatisfied` field + doc; raw struct field; assignment)
- Modify: `internal/ddl/render.go` (delete the `skip_if_satisfied` `addPair`)
- Modify: `internal/run/skip.go` (doc comment referencing the flag — already rewritten in Task 4; verify none remain)
- Modify: `internal/run/engine.go` (comments at ~161, 209, 476, 758, 812, 817, 1038 — remove flag references)
- Modify: `internal/report/history.go:21` (`RunRecord.Skipped` doc)
- Modify: `internal/mssql/indexes.go:71` (comment)
- Test: `internal/ddl/manifest_test.go` (delete `TestParseSkipIfSatisfied`)
- Test: `internal/ddl/render_test.go` (remove `skip_if_satisfied` from the fixture and the omit-empty banned list)

**Interfaces:**
- Removes: `ddl.Manifest.SkipIfSatisfied`. No new symbols. A manifest still carrying `skip_if_satisfied:` fails to load (unknown field) — desired.

- [ ] **Step 1: Write the failing test — a stale flag must be rejected**

Append to `internal/ddl/manifest_intent_test.go`:

```go
func TestManifestRejectsRemovedSkipIfSatisfied(t *testing.T) {
	src := "skip_if_satisfied: true\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
	_, err := ddl.ParseManifest(strings.NewReader(src))
	if err == nil {
		t.Fatal("ParseManifest() error = nil, want unknown-field for the removed flag")
	}
	if !strings.Contains(err.Error(), "skip_if_satisfied") {
		t.Errorf("error does not name the removed flag: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ddl -run TestManifestRejectsRemovedSkipIfSatisfied -v`
Expected: FAIL — the flag still parses (it is a known field), so no error is returned.

- [ ] **Step 3: Delete the field everywhere it is declared, parsed, and written**

In `internal/ddl/manifest.go`:
- Delete the `SkipIfSatisfied bool` field and its doc comment from the `Manifest` struct.
- Delete `SkipIfSatisfied bool \`yaml:"skip_if_satisfied"\`` from the `raw` struct in `UnmarshalYAML`.
- Delete `m.SkipIfSatisfied = raw.SkipIfSatisfied` from the assignment block.

In `internal/ddl/render.go`, delete:

```go
	if m.SkipIfSatisfied {
		addPair(root, "skip_if_satisfied", scalarNode("true"))
	}
```

- [ ] **Step 4: Migrate the two `internal/ddl` tests**

In `internal/ddl/manifest_test.go`, delete the whole `TestParseSkipIfSatisfied` function (lines ~114-135).

In `internal/ddl/render_test.go`:
- In `TestMarshalManifestRoundTrip`, delete the `SkipIfSatisfied: true,` line from the `in` manifest.
- In `TestMarshalManifestOmitsEmpty`, remove `"skip_if_satisfied:"` from the banned-substrings slice.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/ddl -run 'TestManifestRejectsRemovedSkipIfSatisfied|TestMarshalManifest' -v`
Expected: PASS — the removed flag is now an unknown field, and the round-trip fixture no longer sets it.

- [ ] **Step 6: Clean the dead references in other packages**

These are comments only (no behavior). Update each so it no longer names the removed flag; keep the surrounding meaning.

- `internal/report/history.go:21` — the `RunRecord.Skipped` doc currently reads "(skip_if_satisfied)". Change to reflect the new source, e.g. `// Skipped counts operations skipped as already-satisfied (intent: compression at target).`
- `internal/mssql/indexes.go:71` — reword the comment mentioning `skip_if_satisfied` to reference the compression skip generically.
- `internal/run/engine.go` — search the file for `skip_if_satisfied` and reword each comment (notably the `finalizePartial` copy comment ~line 758 that lists it among carried settings): `grep -n skip_if_satisfied internal/run/engine.go`. Replace mentions with `intent` where the sentence still needs an example, or drop the example.
- `internal/run/skip.go` — verify no `skip_if_satisfied` remains: `grep -n skip_if_satisfied internal/run/skip.go` (Task 4 rewrote the doc; expect no hits).

- [ ] **Step 7: Verify the flag is gone from code and tests**

Run: `grep -rn "SkipIfSatisfied\|skip_if_satisfied" internal/ cmd/`
Expected: no matches. The field, its yaml key, and its accesses are gone (Steps 3–4), and Task 4 renamed every `TestSkipIfSatisfied*` function to a `Compression*` name, so no test identifier carries the substring either. (Docs under `docs/specs/`, `docs/`, and `README.md` are handled in Task 6.) If any match remains, it is a comment missed in Step 6 — reword it.

- [ ] **Step 8: Full build, vet, and race suite; commit**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: PASS across all packages.

```bash
git add internal/ddl/manifest.go internal/ddl/render.go internal/ddl/manifest_test.go internal/ddl/render_test.go internal/ddl/manifest_intent_test.go internal/report/history.go internal/mssql/indexes.go internal/run/engine.go internal/run/skip.go
git commit -m "refactor: remove skip_if_satisfied, superseded by intent

The manifest-level flag is deleted from the struct, the parser, and the writer.
A manifest still carrying it fails to load as an unknown field. Per-operation
intent replaces it. Comments that named the flag are reworded.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Documentation

**Files:**
- Modify: `README.md` (lines ~73, ~88-93; document `intent` in the manifest-format section ~184-227)
- Modify: `docs/specs/crash-resumable.md` (§ that defines `skip_if_satisfied`)
- Modify: `docs/specs/graceful-stop.md` (the `recordSkipped` mention)
- Modify: `docs/specs/FIXES.md` (#6 and #11 references)

**Interfaces:** none (documentation only).

- [ ] **Step 1: Rewrite the README operational advice**

`README.md:73` is a dangling forward reference ("`skip_if_satisfied` (below)") to a section that never documented the flag. `README.md:88-93` is advice built on the flag. Rewrite both around `intent`:

- Line ~73: replace "or for `skip_if_satisfied` (below) to make the replay cheap." with "or by marking the operations `intent: compression` (see the manifest format) so a replay skips those already at target."
- Lines ~88-93: replace the `skip_if_satisfied: true` recommendation with the equivalent using a manifest-level `intent: compression`. Keep the advice (a windowed `continue` re-run skips the already-done compressions cheaply); change only the mechanism.

Then, in the manifest-format section (~184-227), document `intent`: values `compression` | `fragmentation`; per-operation with a manifest-level default; compression-intent operations already at target are skipped on a re-run, fragmentation and unset always run.

- [ ] **Step 2: Update the spec cross-references**

- `docs/specs/crash-resumable.md` — the section that normatively defines `skip_if_satisfied` and its default-off rationale: replace with `intent: compression` semantics, or add a one-line note that the flag was superseded by `docs/specs/OPERATION-INTENT.md` and point there.
- `docs/specs/graceful-stop.md` — the `recordSkipped` "shared with skip_if_satisfied" note: reword to reference the intent-based skip.
- `docs/specs/FIXES.md` — #6 (defined by the flag) and #11 (`RunRecord.Skipped`): add a note that the flag is gone and the skip is now intent-gated; #11's field-population change is described in `OPERATION-INTENT.md` §6.

- [ ] **Step 3: Verify no stale flag references remain in prose**

Run: `grep -rn "skip_if_satisfied" README.md docs/ docs/specs/`
Expected: only historical mentions inside `docs/specs/OPERATION-INTENT.md` (which documents the removal) and `docs/specs/superpowers/plans/` (this plan and the maintenance-window plan). No live documentation should present the flag as a current feature.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/specs/crash-resumable.md docs/specs/graceful-stop.md docs/specs/FIXES.md
git commit -m "docs: document intent, retire skip_if_satisfied references

README documents the intent field and rewrites the windowed-continue advice
around it. The specs that defined or referenced skip_if_satisfied point to
OPERATION-INTENT instead.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final Verification

- [ ] **Run the full suite one more time**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: PASS.

- [ ] **Confirm the queue manifests still parse** (they set no intent and no flag, so they must be unaffected)

Run: `make test` (or the targeted `go test -race ./internal/ddl ./internal/run ./internal/maint`)
Expected: PASS.

- [ ] **Simplify pass** — per CLAUDE.md, run a `/simplify` over the diff before considering the feature done: collapse any duplication that accreted (e.g. the two `validateIntent` call sites, the intent-stamping block).
