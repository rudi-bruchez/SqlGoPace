# Pre-shrink Reorganize + Heap Advisory Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Teach the `plan` subcommand to emit a `reorganize → shrink` manifest for the connected database — selecting low-density rowstore indexes to compact before a data shrink — and to print/write a heap advisory for the objects reorganize cannot fix.

**Architecture:** A new `shrink:` section in the maintenance profile drives an optional pre-shrink pass. The density decision is a pure function in `internal/maint` (unit-testable, no DB); the SAMPLED density gathering reuses the existing `PhysicalStats` reader in `internal/plan`; `cmd/sqlgopace` assembles the dedicated shrink manifest and the `.heaps.yaml` advisory sidecar and wires them into `runPlan`. The `shrink`/`reorganize_index`/`rebuild_heap` operations and the shrink driver already exist and are unchanged.

**Tech Stack:** Go, `gopkg.in/yaml.v3`, SQL Server DMVs (`sys.dm_db_index_physical_stats` SAMPLED), the existing `internal/{ddl,maint,plan,mssql}` packages.

## Global Constraints

- **English only** — all code, comments, identifiers, committed docs.
- **Idiomatic Go, KISS** — match surrounding code; no new layers/abstractions beyond what the tasks define. US spelling in comments/identifiers.
- **Tests:** `make test` runs `go test -race ./...`, no database needed. Every task's tests are pure/fakeable.
- **Lint:** golangci-lint v2 (`.golangci.yml`). On this Windows tree, gofmt reports pre-existing CRLF/version noise on files you did not touch — ignore it; ensure only that files you create/edit are gofmt-clean once CRLF-normalized (`tr -d '\r' < f | gofmt -l` prints nothing).
- **Connected database only.** Multi-database shrink (`planMulti`) and the `--auto` run path are **out of scope** for this iteration — wire the pre-shrink pass into single-database `runPlan` only.
- **Signal is page density, not fragmentation:** `avg_page_space_used_in_percent < reorganize_below_density_percent` (default 65), from a SAMPLED scan.
- **Never emit REBUILD** in the pre-shrink pass (a rebuild grows the file — `docs/specs/SHRINK.md` §400); emit `reorganize_index` only.
- **`type: log`** skips reorganize and the advisory entirely (a log shrink reclaims VLFs, not page density).
- Design source of truth: `docs/specs/superpowers/specs/2026-07-28-pre-shrink-reorganize-design.md`.

---

## File Structure

- `internal/maint/profile.go` — **modify**: add `ShrinkRules` struct, `Profile.Shrink` field, defaults, validation.
- `internal/maint/shrink.go` — **create**: pre-shrink measurement types + `DecidePreShrink` (pure decision).
- `internal/maint/shrink_test.go` — **create**: `DecidePreShrink` tests.
- `internal/plan/shrink.go` — **create**: `AnalyzePreShrink` (SAMPLED density gathering over `Reader`).
- `internal/plan/shrink_test.go` — **create**: `AnalyzePreShrink` tests with a fake `Reader`.
- `cmd/sqlgopace/shrink_plan.go` — **create**: `shrinkManifest`, `planShrink`, heap-advisory print/write.
- `cmd/sqlgopace/shrink_plan_test.go` — **create**: manifest-assembly + advisory + `planShrink` tests.
- `cmd/sqlgopace/plan.go` — **modify**: call `planShrink` in `runPlan`, append the manifest, print/write the advisory.
- `README.md` — **modify**: document the `shrink:` profile section.

---

### Task 1: `ShrinkRules` in the maintenance profile

**Files:**
- Modify: `internal/maint/profile.go`
- Test: `internal/maint/profile_test.go`

**Interfaces:**
- Produces: `maint.ShrinkRules` struct; `Profile.Shrink ShrinkRules`; methods `(ShrinkRules).PreReorganizeEnabled() bool`, `(ShrinkRules).IsLog() bool`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/maint/profile_test.go`:

```go
func TestShrinkRulesParseAndDefaults(t *testing.T) {
	p, err := maint.Parse([]byte("shrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  max_block_minutes: 10\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !p.Shrink.Enabled || p.Shrink.Type != "data" || p.Shrink.Files != "all" {
		t.Errorf("shrink parsed wrong: %+v", p.Shrink)
	}
	if p.Shrink.ReorganizeBelowDensityPercent != 65 {
		t.Errorf("density default = %v, want 65", p.Shrink.ReorganizeBelowDensityPercent)
	}
	if !p.Shrink.PreReorganizeEnabled() {
		t.Error("PreReorganizeEnabled() = false, want true (default when enabled)")
	}
	if p.Shrink.MaxBlockMinutes != 10 {
		t.Errorf("max_block_minutes = %d, want 10", p.Shrink.MaxBlockMinutes)
	}
}

func TestShrinkRulesPreReorganizeExplicitFalse(t *testing.T) {
	p, err := maint.Parse([]byte("shrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  pre_reorganize: false\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Shrink.PreReorganizeEnabled() {
		t.Error("PreReorganizeEnabled() = true, want false (explicit)")
	}
}

func TestShrinkRulesValidation(t *testing.T) {
	for name, body := range map[string]string{
		"bad type":      "shrink:\n  enabled: true\n  type: index\n  files: all\n  targetfreespace: 10%\n",
		"empty files":   "shrink:\n  enabled: true\n  type: data\n  files: \"\"\n  targetfreespace: 10%\n",
		"bad target":    "shrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: nonsense\n",
		"density > 100": "shrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 150\n",
		"neg maxblock":  "shrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  max_block_minutes: -1\n",
	} {
		if _, err := maint.Parse([]byte(body)); err == nil {
			t.Errorf("%s: expected a validation error, got nil", name)
		}
	}
}

func TestShrinkRulesDisabledIsInert(t *testing.T) {
	// An absent shrink section (or enabled:false) must not error even with no other fields.
	if _, err := maint.Parse([]byte("index:\n  reorganize_from_percent: 5\n")); err != nil {
		t.Fatalf("parse without shrink section: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/maint -run TestShrinkRules -v`
Expected: FAIL (compile error — `Shrink` field / methods undefined).

- [ ] **Step 3: Add the struct, field, methods, defaults, and validation**

In `internal/maint/profile.go`, add the ddl import to the import block:

```go
	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
```

Add `Shrink` to the `Profile` struct (after `Heap`):

```go
	Shrink      ShrinkRules      `yaml:"shrink"`
```

Add the type + methods (place after `HeapRules`):

```go
// ShrinkRules drives the plan subcommand's optional pre-shrink pass: it emits a
// reorganize→shrink manifest for the connected database and a heap advisory. The
// section is inert unless Enabled. See docs/specs/superpowers/specs/2026-07-28-pre-shrink-
// reorganize-design.md.
type ShrinkRules struct {
	Enabled                       bool    `yaml:"enabled"`
	Type                          string  `yaml:"type"`                             // data | log
	Files                         string  `yaml:"files"`                            // all | logical file name
	TargetFreeSpace               string  `yaml:"targetfreespace"`                  // percent or absolute MB
	PreReorganize                 *bool   `yaml:"pre_reorganize"`                   // nil = true when Enabled
	ReorganizeBelowDensityPercent float64 `yaml:"reorganize_below_density_percent"` // default 65
	MaxBlockMinutes               int     `yaml:"max_block_minutes"`                // carried into the shrink op; 0 = omit
}

// PreReorganizeEnabled reports whether the pre-shrink reorganize pass runs. It defaults
// to true when the shrink section is enabled, unless explicitly set to false.
func (r ShrinkRules) PreReorganizeEnabled() bool { return r.PreReorganize == nil || *r.PreReorganize }

// IsLog reports whether this is a log-file shrink (no reorganize, no heap advisory).
func (r ShrinkRules) IsLog() bool { return strings.EqualFold(strings.TrimSpace(r.Type), "log") }
```

In `applyDefaults`, add (after the heap block):

```go
	if p.Shrink.Enabled && p.Shrink.ReorganizeBelowDensityPercent == 0 {
		p.Shrink.ReorganizeBelowDensityPercent = 65
	}
```

In `validate`, add (before `return nil`):

```go
	if p.Shrink.Enabled {
		switch strings.ToLower(strings.TrimSpace(p.Shrink.Type)) {
		case "data", "log":
		default:
			return fmt.Errorf("shrink.type = %q, want \"data\" or \"log\": %w", p.Shrink.Type, ErrInvalidProfile)
		}
		if strings.TrimSpace(p.Shrink.Files) == "" {
			return fmt.Errorf("shrink.files is required (\"all\" or a logical file name): %w", ErrInvalidProfile)
		}
		if _, err := ddl.ParseTargetFreeSpace(p.Shrink.TargetFreeSpace); err != nil {
			return fmt.Errorf("shrink.targetfreespace: %w", err)
		}
		if d := p.Shrink.ReorganizeBelowDensityPercent; d < 1 || d > 100 {
			return fmt.Errorf("shrink.reorganize_below_density_percent must be in 1..100: %w", ErrInvalidProfile)
		}
		if p.Shrink.MaxBlockMinutes < 0 {
			return fmt.Errorf("shrink.max_block_minutes must be ≥ 0: %w", ErrInvalidProfile)
		}
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/maint -run TestShrinkRules -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add internal/maint/profile.go internal/maint/profile_test.go
git commit -m "feat(maint): add shrink rules to the maintenance profile"
```

---

### Task 2: `DecidePreShrink` pure decision

**Files:**
- Create: `internal/maint/shrink.go`
- Test: `internal/maint/shrink_test.go`

**Interfaces:**
- Consumes: `maint.Profile` (Task 1), `ddl.ReorganizeIndex`.
- Produces:
  - `maint.ShrinkIndexMeasurement{Schema, Table, Index string; PageCount int64; AvgPageSpaceUsedPercent float64}`
  - `maint.ShrinkHeapMeasurement{Schema, Table string; SizeMB int64; ForwardedRecordPercent, AvgPageSpaceUsedPercent float64}`
  - `maint.HeapAdvisory{Schema, Table string; SizeMB int64; ForwardedRecordPercent, PageDensityPercent float64}`
  - `maint.PreShrinkPlan{Reorganizes []ddl.ReorganizeIndex; HeapAdvisories []HeapAdvisory}`
  - `func DecidePreShrink(indexes []ShrinkIndexMeasurement, heaps []ShrinkHeapMeasurement, p *Profile) PreShrinkPlan`

- [ ] **Step 1: Write the failing tests**

Create `internal/maint/shrink_test.go`:

```go
package maint_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
)

func shrinkProfile(t *testing.T) *maint.Profile {
	t.Helper()
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\nheap:\n  min_size_mb: 10\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 65\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	return p
}

func TestDecidePreShrinkSelectsLowDensityIndexes(t *testing.T) {
	p := shrinkProfile(t)
	indexes := []maint.ShrinkIndexMeasurement{
		{Schema: "dbo", Table: "A", Index: "PK_A", PageCount: 5000, AvgPageSpaceUsedPercent: 40}, // below 65 → reorganize
		{Schema: "dbo", Table: "B", Index: "PK_B", PageCount: 5000, AvgPageSpaceUsedPercent: 90}, // dense → skip
		{Schema: "dbo", Table: "C", Index: "PK_C", PageCount: 100, AvgPageSpaceUsedPercent: 10},  // below floor → skip
	}
	pl := maint.DecidePreShrink(indexes, nil, p)
	if len(pl.Reorganizes) != 1 {
		t.Fatalf("got %d reorganizes, want 1: %+v", len(pl.Reorganizes), pl.Reorganizes)
	}
	ro := pl.Reorganizes[0]
	if ro.Schema != "dbo" || ro.Table != "A" || ro.Index != "PK_A" {
		t.Errorf("reorganize target = %+v, want dbo.A.PK_A", ro)
	}
}

func TestDecidePreShrinkHeapAdvisory(t *testing.T) {
	p := shrinkProfile(t)
	heaps := []maint.ShrinkHeapMeasurement{
		{Schema: "dbo", Table: "H", SizeMB: 3000, ForwardedRecordPercent: 12, AvgPageSpaceUsedPercent: 55}, // advise
		{Schema: "dbo", Table: "D", SizeMB: 3000, AvgPageSpaceUsedPercent: 80},                             // dense → skip
		{Schema: "dbo", Table: "S", SizeMB: 5, AvgPageSpaceUsedPercent: 10},                                // below min size → skip
	}
	pl := maint.DecidePreShrink(nil, heaps, p)
	if len(pl.HeapAdvisories) != 1 || pl.HeapAdvisories[0].Table != "H" {
		t.Fatalf("got %+v, want one advisory for dbo.H", pl.HeapAdvisories)
	}
	// A heap is never emitted as a reorganize.
	if len(pl.Reorganizes) != 0 {
		t.Errorf("heaps must not produce reorganizes, got %+v", pl.Reorganizes)
	}
}

func TestDecidePreShrinkHonorsOverrideSkip(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\noverrides:\n  - match: dbo.A\n    skip: true\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	indexes := []maint.ShrinkIndexMeasurement{{Schema: "dbo", Table: "A", Index: "PK_A", PageCount: 5000, AvgPageSpaceUsedPercent: 40}}
	if pl := maint.DecidePreShrink(indexes, nil, p); len(pl.Reorganizes) != 0 {
		t.Errorf("override skip must drop the reorganize, got %+v", pl.Reorganizes)
	}
}

func TestDecidePreShrinkReorganizeCarriesLOBCompaction(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\n  lob_compaction: true\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	indexes := []maint.ShrinkIndexMeasurement{{Schema: "dbo", Table: "A", Index: "PK_A", PageCount: 5000, AvgPageSpaceUsedPercent: 40}}
	pl := maint.DecidePreShrink(indexes, nil, p)
	if len(pl.Reorganizes) != 1 || !pl.Reorganizes[0].LOBCompaction {
		t.Errorf("reorganize should carry LOBCompaction=true, got %+v", pl.Reorganizes)
	}
	_ = ddl.ReorganizeIndex{} // ensure ddl import is used
}

func TestDecidePreShrinkDensityBoundary(t *testing.T) {
	p := shrinkProfile(t) // threshold 65
	indexes := []maint.ShrinkIndexMeasurement{
		{Schema: "dbo", Table: "AtThreshold", Index: "IX", PageCount: 5000, AvgPageSpaceUsedPercent: 65},  // == threshold → skip (only below qualifies)
		{Schema: "dbo", Table: "Below", Index: "IX", PageCount: 5000, AvgPageSpaceUsedPercent: 64.9},      // below → reorganize
	}
	pl := maint.DecidePreShrink(indexes, nil, p)
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Table != "Below" {
		t.Errorf("boundary: density == threshold must be skipped; got %+v", pl.Reorganizes)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/maint -run TestDecidePreShrink -v`
Expected: FAIL (compile error — `DecidePreShrink` and types undefined).

- [ ] **Step 3: Implement the decision**

Create `internal/maint/shrink.go`:

```go
package maint

import "github.com/rudi-bruchez/SqlGoPace/internal/ddl"

// ShrinkIndexMeasurement is one rowstore index's page density for the pre-shrink
// reorganize decision, aggregated across the index's partitions.
type ShrinkIndexMeasurement struct {
	Schema, Table, Index    string
	PageCount               int64
	AvgPageSpaceUsedPercent float64 // worst (lowest) density across the index's partitions
}

// ShrinkHeapMeasurement is one heap's density for the pre-shrink advisory.
type ShrinkHeapMeasurement struct {
	Schema, Table           string
	SizeMB                  int64
	ForwardedRecordPercent  float64
	AvgPageSpaceUsedPercent float64
}

// HeapAdvisory names a low-density heap the shrink cannot benefit from: reorganize
// cannot compact a heap's in-row data, so the operator rebuilds it (rebuild_heap) in a
// window. Identified by property, not confirmed tail position (see the design spec §3).
type HeapAdvisory struct {
	Schema, Table          string
	SizeMB                 int64
	ForwardedRecordPercent float64
	PageDensityPercent     float64
}

// PreShrinkPlan is the pre-shrink pass output: the reorganizes to run before the
// shrink, and the heap advisories to surface.
type PreShrinkPlan struct {
	Reorganizes    []ddl.ReorganizeIndex
	HeapAdvisories []HeapAdvisory
}

// DecidePreShrink selects the low-density rowstore indexes to reorganize before a
// shrink and the low-density heaps to advise on. An index qualifies when it is at or
// above the index page-count floor and below the shrink density threshold; a heap
// qualifies when at or above the heap min size and below the same threshold. A table an
// override skips is dropped from both. It never emits a REBUILD (a rebuild grows the
// file) — reorganizes only.
func DecidePreShrink(indexes []ShrinkIndexMeasurement, heaps []ShrinkHeapMeasurement, p *Profile) PreShrinkPlan {
	var pl PreShrinkPlan
	threshold := p.Shrink.ReorganizeBelowDensityPercent

	for _, m := range indexes {
		if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
			continue
		}
		if m.PageCount < int64(p.Index.PageCountFloor) || m.AvgPageSpaceUsedPercent >= threshold {
			continue
		}
		pl.Reorganizes = append(pl.Reorganizes, ddl.ReorganizeIndex{
			Schema: m.Schema, Table: m.Table, Index: m.Index, LOBCompaction: p.Index.LOBCompaction,
		})
	}

	for _, m := range heaps {
		if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
			continue
		}
		if m.SizeMB < p.Heap.MinSizeMB || m.AvgPageSpaceUsedPercent >= threshold {
			continue
		}
		pl.HeapAdvisories = append(pl.HeapAdvisories, HeapAdvisory{
			Schema: m.Schema, Table: m.Table, SizeMB: m.SizeMB,
			ForwardedRecordPercent: m.ForwardedRecordPercent, PageDensityPercent: m.AvgPageSpaceUsedPercent,
		})
	}
	return pl
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/maint -run TestDecidePreShrink -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add internal/maint/shrink.go internal/maint/shrink_test.go
git commit -m "feat(maint): DecidePreShrink selects low-density indexes + heap advisories"
```

---

### Task 3: `AnalyzePreShrink` density gathering

**Files:**
- Create: `internal/plan/shrink.go`
- Test: `internal/plan/shrink_test.go`

**Interfaces:**
- Consumes: `plan.Reader` (`ObjectInventory`, `PhysicalStats`), `maint.DecidePreShrink` and its measurement types (Task 2), `mssql.PhysicalSampled`, `groupInventory` (same package, from `plan.go`).
- Produces: `func AnalyzePreShrink(ctx context.Context, r Reader, profile *maint.Profile, logw io.Writer) (maint.PreShrinkPlan, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/plan/shrink_test.go`:

```go
package plan_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/plan"
)

// fakeShrinkReader returns canned inventory + sampled physical stats; the other Reader
// methods are unused by AnalyzePreShrink and return nil.
type fakeShrinkReader struct {
	inv        []mssql.InventoryObject
	density    map[int64]float64 // objectID → avg_page_space_used_in_percent
	pages      map[int64]int64   // objectID → page_count
	errObjects map[int64]bool    // objectID → PhysicalStats returns an error (per-object read failure)
}

func (f *fakeShrinkReader) ObjectInventory(context.Context) ([]mssql.InventoryObject, error) {
	return f.inv, nil
}
func (f *fakeShrinkReader) PhysicalStats(_ context.Context, objectID int64, _ int, _ *int, mode string) ([]mssql.PhysicalStats, error) {
	if f.errObjects[objectID] {
		return nil, fmt.Errorf("sampled scan failed for object %d", objectID)
	}
	return []mssql.PhysicalStats{{
		PartitionNumber: 1, PageCount: f.pages[objectID],
		AvgPageSpaceUsedPercent: f.density[objectID], RecordCount: 100,
	}}, nil
}
func (f *fakeShrinkReader) EstimateCompression(context.Context, string, string, int, *int, string) ([]mssql.CompressionSaving, error) {
	return nil, nil
}
func (f *fakeShrinkReader) IndexOperationalStats(context.Context, int64, int, *int) ([]mssql.OperationalStats, error) {
	return nil, nil
}
func (f *fakeShrinkReader) StatsProperties(context.Context, int64) ([]mssql.StatProperty, error) {
	return nil, nil
}

func TestAnalyzePreShrink(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\nheap:\n  min_size_mb: 10\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 65\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	r := &fakeShrinkReader{
		inv: []mssql.InventoryObject{
			{Schema: "dbo", Table: "A", ObjectID: 1, IndexID: 1, IndexName: "PK_A", Type: 1, PartitionNumber: 1, SizeMB: 100},
			{Schema: "dbo", Table: "H", ObjectID: 2, IndexID: 0, IndexName: "", Type: 0, PartitionNumber: 1, SizeMB: 3000},
		},
		density: map[int64]float64{1: 40, 2: 50},
		pages:   map[int64]int64{1: 5000, 2: 400000},
	}
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AnalyzePreShrink: %v", err)
	}
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Index != "PK_A" {
		t.Errorf("reorganizes = %+v, want one for PK_A", pl.Reorganizes)
	}
	if len(pl.HeapAdvisories) != 1 || pl.HeapAdvisories[0].Table != "H" {
		t.Errorf("advisories = %+v, want one for dbo.H", pl.HeapAdvisories)
	}
}

func TestAnalyzePreShrinkSkipsFailedObjectNotFatal(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 65\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	r := &fakeShrinkReader{
		inv: []mssql.InventoryObject{
			{Schema: "dbo", Table: "Bad", ObjectID: 1, IndexID: 1, IndexName: "IX_Bad", Type: 1, PartitionNumber: 1, SizeMB: 100},
			{Schema: "dbo", Table: "Good", ObjectID: 2, IndexID: 1, IndexName: "IX_Good", Type: 1, PartitionNumber: 1, SizeMB: 100},
		},
		density:    map[int64]float64{2: 40},
		pages:      map[int64]int64{2: 5000},
		errObjects: map[int64]bool{1: true}, // object 1's sampled scan errors; must be skipped, not fatal
	}
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("a per-object read error must not be fatal, got %v", err)
	}
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Table != "Good" {
		t.Errorf("failed object should be skipped, survivor kept; got %+v", pl.Reorganizes)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/plan -run TestAnalyzePreShrink -v`
Expected: FAIL (compile error — `AnalyzePreShrink` undefined).

- [ ] **Step 3: Implement the gathering**

Create `internal/plan/shrink.go`:

```go
package plan

import (
	"context"
	"fmt"
	"io"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// AnalyzePreShrink gathers SAMPLED page density for the connected database's rowstore
// indexes and heaps and returns the pre-shrink reorganize + heap-advisory plan. The
// SAMPLED scan is heavier than the LIMITED fragmentation scan the maintenance pass uses,
// so it runs only when the shrink section is enabled and the shrink is a data shrink
// with pre_reorganize on (the caller enforces that). Per-object read failures are logged
// and skipped, never fatal.
func AnalyzePreShrink(ctx context.Context, r Reader, profile *maint.Profile, logw io.Writer) (maint.PreShrinkPlan, error) {
	inv, err := r.ObjectInventory(ctx)
	if err != nil {
		return maint.PreShrinkPlan{}, fmt.Errorf("object inventory: %w", err)
	}
	groups, _ := groupInventory(inv)

	var indexes []maint.ShrinkIndexMeasurement
	var heaps []maint.ShrinkHeapMeasurement
	for _, g := range groups {
		head := g[0]
		switch {
		case head.IsHeap():
			if m, ok := shrinkHeapMeasurement(ctx, r, profile, head, logw); ok {
				heaps = append(heaps, m)
			}
		case head.Type == 1 || head.Type == 2: // rowstore clustered / nonclustered
			if m, ok := shrinkIndexMeasurement(ctx, r, head, logw); ok {
				indexes = append(indexes, m)
			}
		}
	}
	return maint.DecidePreShrink(indexes, heaps, profile), nil
}

// shrinkIndexMeasurement reads SAMPLED density for one rowstore index, aggregating
// across partitions (total page count, worst density). ok is false on a read error or
// when the scan returns no rows.
func shrinkIndexMeasurement(ctx context.Context, r Reader, head mssql.InventoryObject, logw io.Writer) (maint.ShrinkIndexMeasurement, bool) {
	ps, err := r.PhysicalStats(ctx, head.ObjectID, head.IndexID, nil, mssql.PhysicalSampled)
	if err != nil {
		fmt.Fprintf(logw, "-- skip index %s.%s.%s: sampled scan: %v\n", head.Schema, head.Table, head.IndexName, err)
		return maint.ShrinkIndexMeasurement{}, false
	}
	if len(ps) == 0 {
		return maint.ShrinkIndexMeasurement{}, false
	}
	var pageCount int64
	minDensity := 100.0
	for _, s := range ps {
		pageCount += s.PageCount
		if s.AvgPageSpaceUsedPercent < minDensity {
			minDensity = s.AvgPageSpaceUsedPercent
		}
	}
	return maint.ShrinkIndexMeasurement{
		Schema: head.Schema, Table: head.Table, Index: head.IndexName,
		PageCount: pageCount, AvgPageSpaceUsedPercent: minDensity,
	}, true
}

// shrinkHeapMeasurement reads SAMPLED density + forwarded records for one heap, after a
// cheap min-size pre-filter. ok is false when the heap is below the min size, the scan
// fails, or it returns no rows.
func shrinkHeapMeasurement(ctx context.Context, r Reader, p *maint.Profile, head mssql.InventoryObject, logw io.Writer) (maint.ShrinkHeapMeasurement, bool) {
	sizeMB := int64(head.SizeMB)
	if sizeMB < p.Heap.MinSizeMB {
		return maint.ShrinkHeapMeasurement{}, false
	}
	ps, err := r.PhysicalStats(ctx, head.ObjectID, 0, nil, mssql.PhysicalSampled)
	if err != nil {
		fmt.Fprintf(logw, "-- skip heap %s.%s: sampled scan: %v\n", head.Schema, head.Table, err)
		return maint.ShrinkHeapMeasurement{}, false
	}
	if len(ps) == 0 {
		return maint.ShrinkHeapMeasurement{}, false
	}
	var forwarded, records int64
	minDensity := 100.0
	for _, s := range ps {
		forwarded += s.ForwardedRecordCount
		records += s.RecordCount
		if s.AvgPageSpaceUsedPercent < minDensity {
			minDensity = s.AvgPageSpaceUsedPercent
		}
	}
	fwdPct := 0.0
	if records > 0 {
		fwdPct = float64(forwarded) / float64(records) * 100
	}
	return maint.ShrinkHeapMeasurement{
		Schema: head.Schema, Table: head.Table, SizeMB: sizeMB,
		ForwardedRecordPercent: fwdPct, AvgPageSpaceUsedPercent: minDensity,
	}, true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/plan -run TestAnalyzePreShrink -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/plan/shrink.go internal/plan/shrink_test.go
git commit -m "feat(plan): AnalyzePreShrink gathers SAMPLED density for the pre-shrink pass"
```

---

### Task 4: Shrink manifest assembly + heap advisory output

**Files:**
- Create: `cmd/sqlgopace/shrink_plan.go`
- Test: `cmd/sqlgopace/shrink_plan_test.go`

**Interfaces:**
- Consumes: `maint.Profile`, `maint.PreShrinkPlan`, `maint.HeapAdvisory` (Tasks 1–2); `ddl.Shrink`, `ddl.Manifest`, `ddl.Operation`, `ddl.OptionOverrides`; `namedManifest` (from `plan.go`).
- Produces:
  - `func shrinkManifest(profile *maint.Profile, db string, pre maint.PreShrinkPlan) *namedManifest`
  - `func printHeapAdvisory(w io.Writer, advisories []maint.HeapAdvisory)`
  - `func writeHeapAdvisorySidecar(dir, shrinkFilename, db string, advisories []maint.HeapAdvisory) (string, error)`

- [ ] **Step 1: Write the failing tests**

Create `cmd/sqlgopace/shrink_plan_test.go`:

```go
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
)

func dataShrinkProfile(t *testing.T, extra string) *maint.Profile {
	t.Helper()
	p, err := maint.Parse([]byte("shrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n" + extra))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	return p
}

func TestShrinkManifestReorganizesPrecedeShrink(t *testing.T) {
	p := dataShrinkProfile(t, "  max_block_minutes: 10\n")
	pre := maint.PreShrinkPlan{Reorganizes: []ddl.ReorganizeIndex{
		{Schema: "dbo", Table: "A", Index: "PK_A"},
	}}
	nm := shrinkManifest(p, "PRODDB", pre)
	if nm == nil {
		t.Fatal("shrinkManifest returned nil for an enabled profile")
	}
	if nm.filename != "050_shrink_PRODDB_data.yaml" {
		t.Errorf("filename = %q", nm.filename)
	}
	ops := nm.manifest.Operations
	if len(ops) != 2 {
		t.Fatalf("got %d ops, want reorganize+shrink", len(ops))
	}
	if _, ok := ops[0].(ddl.ReorganizeIndex); !ok {
		t.Errorf("op[0] = %T, want ReorganizeIndex", ops[0])
	}
	sh, ok := ops[1].(ddl.Shrink)
	if !ok {
		t.Fatalf("op[1] = %T, want Shrink", ops[1])
	}
	if sh.Options.MaxBlockMinutes == nil || *sh.Options.MaxBlockMinutes != 10 {
		t.Errorf("max_block_minutes not carried: %+v", sh.Options)
	}
}

func TestShrinkManifestNoPreReorganizeIsShrinkOnly(t *testing.T) {
	p := dataShrinkProfile(t, "")
	// Caller passes an empty PreShrinkPlan when pre_reorganize is off.
	nm := shrinkManifest(p, "DB", maint.PreShrinkPlan{})
	if len(nm.manifest.Operations) != 1 {
		t.Fatalf("want shrink op only, got %d", len(nm.manifest.Operations))
	}
	if _, ok := nm.manifest.Operations[0].(ddl.Shrink); !ok {
		t.Errorf("op = %T, want Shrink", nm.manifest.Operations[0])
	}
}

func TestShrinkManifestLogType(t *testing.T) {
	p, err := maint.Parse([]byte("shrink:\n  enabled: true\n  type: log\n  files: all\n  targetfreespace: 10%\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	nm := shrinkManifest(p, "DB", maint.PreShrinkPlan{})
	if nm.filename != "050_shrink_DB_log.yaml" {
		t.Errorf("filename = %q, want log", nm.filename)
	}
	sh := nm.manifest.Operations[0].(ddl.Shrink)
	if sh.Options.MaxBlockMinutes != nil {
		t.Errorf("max_block_minutes should be nil when unset, got %v", *sh.Options.MaxBlockMinutes)
	}
}

func TestShrinkManifestDisabledIsNil(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if nm := shrinkManifest(p, "DB", maint.PreShrinkPlan{}); nm != nil {
		t.Errorf("shrinkManifest = %+v, want nil when disabled", nm)
	}
}

func TestHeapAdvisorySidecar(t *testing.T) {
	dir := t.TempDir()
	advisories := []maint.HeapAdvisory{{Schema: "dbo", Table: "H", SizeMB: 3000, ForwardedRecordPercent: 12, PageDensityPercent: 55}}
	path, err := writeHeapAdvisorySidecar(dir, "050_shrink_DB_data.yaml", "DB", advisories)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if filepath.Base(path) != "050_shrink_DB_data.yaml.heaps.yaml" {
		t.Errorf("sidecar path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "dbo") || !strings.Contains(body, "H") || !strings.Contains(body, "rebuild_heap") {
		t.Errorf("advisory body missing expected content:\n%s", body)
	}
}

func TestHeapAdvisoryEmptyNoFile(t *testing.T) {
	dir := t.TempDir()
	path, err := writeHeapAdvisorySidecar(dir, "050_shrink_DB_data.yaml", "DB", nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if path != "" {
		t.Errorf("expected no sidecar for empty advisories, got %q", path)
	}
}

func TestPrintHeapAdvisory(t *testing.T) {
	var buf bytes.Buffer
	printHeapAdvisory(&buf, []maint.HeapAdvisory{{Schema: "dbo", Table: "H", SizeMB: 3000, PageDensityPercent: 55, ForwardedRecordPercent: 12}})
	if !strings.Contains(buf.String(), "dbo.H") {
		t.Errorf("advisory print missing table: %q", buf.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/sqlgopace -run 'TestShrinkManifest|TestHeapAdvisory|TestPrintHeapAdvisory' -v`
Expected: FAIL (compile error — functions undefined).

- [ ] **Step 3: Implement the assembly + advisory**

Create `cmd/sqlgopace/shrink_plan.go`:

```go
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"gopkg.in/yaml.v3"
)

// shrinkManifest assembles the dedicated shrink manifest for the connected database:
// the density-selected reorganizes (in pre, empty when pre_reorganize is off or the
// shrink is a log shrink) followed by the shrink operation. Returns nil when the shrink
// section is disabled. Filename sorts after the maintenance manifests (010–040).
func shrinkManifest(profile *maint.Profile, db string, pre maint.PreShrinkPlan) *namedManifest {
	s := profile.Shrink
	if !s.Enabled {
		return nil
	}
	ops := make([]ddl.Operation, 0, len(pre.Reorganizes)+1)
	for _, ro := range pre.Reorganizes {
		ops = append(ops, ro)
	}
	shrink := ddl.Shrink{Type: s.Type, Files: s.Files, TargetFreeSpace: s.TargetFreeSpace}
	if s.MaxBlockMinutes > 0 {
		mb := s.MaxBlockMinutes
		shrink.Options.MaxBlockMinutes = &mb
	}
	ops = append(ops, shrink)

	typ := "data"
	if s.IsLog() {
		typ = "log"
	}
	return &namedManifest{
		filename: fmt.Sprintf("050_shrink_%s_%s.yaml", db, typ),
		category: "shrink",
		manifest: &ddl.Manifest{
			Description: fmt.Sprintf("Pre-shrink reorganize + reclaim for %s — %s (generated by sqlgopace)", db, typ),
			Database:    db,
			Operations:  ops,
		},
	}
}

// heapAdvisoryDoc is the .heaps.yaml advisory sidecar. Advisory only — SqlGoPace never
// reads it back (mirrors the .blocked.yaml convention).
type heapAdvisoryDoc struct {
	Advisory string             `yaml:"advisory"`
	Database string             `yaml:"database"`
	Heaps    []heapAdvisoryItem `yaml:"heaps"`
}

type heapAdvisoryItem struct {
	Schema                 string  `yaml:"schema"`
	Table                  string  `yaml:"table"`
	SizeMB                 int64   `yaml:"size_mb"`
	ForwardedRecordPercent float64 `yaml:"forwarded_record_percent"`
	PageDensityPercent     float64 `yaml:"page_density_percent"`
}

const heapAdvisoryText = "Heaps cannot be reorganized in-row (reorganize only compacts their LOB pages). " +
	"These low-density heaps may keep the shrink from reaching target; rebuild them with a rebuild_heap " +
	"manifest in a maintenance window (offline/blocking on Standard edition), or give them a clustered index. " +
	"Candidates by property (size/density), not confirmed tail position."

// printHeapAdvisory prints the advisory summary to w (both dry-run and real runs).
func printHeapAdvisory(w io.Writer, advisories []maint.HeapAdvisory) {
	if len(advisories) == 0 {
		return
	}
	fmt.Fprintf(w, "-- heap advisory: %d heap(s) reorganize cannot help before the shrink:\n", len(advisories))
	for _, a := range advisories {
		fmt.Fprintf(w, "--   %s.%s  %d MB  density %.0f%%  forwarded %.0f%%\n",
			a.Schema, a.Table, a.SizeMB, a.PageDensityPercent, a.ForwardedRecordPercent)
	}
}

// writeHeapAdvisorySidecar writes the .heaps.yaml sidecar next to the shrink manifest
// and returns its path. It returns ("", nil) when there are no advisories.
func writeHeapAdvisorySidecar(dir, shrinkFilename, db string, advisories []maint.HeapAdvisory) (string, error) {
	if len(advisories) == 0 {
		return "", nil
	}
	doc := heapAdvisoryDoc{Advisory: heapAdvisoryText, Database: db}
	for _, a := range advisories {
		doc.Heaps = append(doc.Heaps, heapAdvisoryItem{
			Schema: a.Schema, Table: a.Table, SizeMB: a.SizeMB,
			ForwardedRecordPercent: a.ForwardedRecordPercent, PageDensityPercent: a.PageDensityPercent,
		})
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal heap advisory: %w", err)
	}
	path := filepath.Join(dir, shrinkFilename+".heaps.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write heap advisory: %w", err)
	}
	return path, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/sqlgopace -run 'TestShrinkManifest|TestHeapAdvisory|TestPrintHeapAdvisory' -v`
Expected: PASS (all cases).

- [ ] **Step 5: Commit**

```bash
git add cmd/sqlgopace/shrink_plan.go cmd/sqlgopace/shrink_plan_test.go
git commit -m "feat(cmd): assemble shrink manifest + heap advisory sidecar"
```

---

### Task 5: `planShrink` glue + wire into `runPlan`

**Files:**
- Modify: `cmd/sqlgopace/shrink_plan.go` (add `planShrink`)
- Modify: `cmd/sqlgopace/plan.go` (`runPlan`)
- Test: `cmd/sqlgopace/shrink_plan_test.go` (add `planShrink` end-to-end test)

**Interfaces:**
- Consumes: `plan.Reader`, `plan.AnalyzePreShrink` (Task 3), `shrinkManifest` (Task 4).
- Produces: `func planShrink(ctx context.Context, r plan.Reader, profile *maint.Profile, db string, logw io.Writer) (*namedManifest, []maint.HeapAdvisory, error)`

- [ ] **Step 1: Write the failing test**

Add to `cmd/sqlgopace/shrink_plan_test.go` (reuse `fakeShrinkReader` shape — define a local one here since the one in Task 3 lives in package `plan_test`):

```go
// fakePlanReader implements plan.Reader for planShrink tests.
type fakePlanReader struct {
	inv     []mssql.InventoryObject
	density map[int64]float64
	pages   map[int64]int64
}

func (f *fakePlanReader) ObjectInventory(context.Context) ([]mssql.InventoryObject, error) {
	return f.inv, nil
}
func (f *fakePlanReader) PhysicalStats(_ context.Context, objectID int64, _ int, _ *int, _ string) ([]mssql.PhysicalStats, error) {
	return []mssql.PhysicalStats{{PartitionNumber: 1, PageCount: f.pages[objectID], AvgPageSpaceUsedPercent: f.density[objectID], RecordCount: 100}}, nil
}
func (f *fakePlanReader) EstimateCompression(context.Context, string, string, int, *int, string) ([]mssql.CompressionSaving, error) {
	return nil, nil
}
func (f *fakePlanReader) IndexOperationalStats(context.Context, int64, int, *int) ([]mssql.OperationalStats, error) {
	return nil, nil
}
func (f *fakePlanReader) StatsProperties(context.Context, int64) ([]mssql.StatProperty, error) {
	return nil, nil
}

func TestPlanShrinkEndToEnd(t *testing.T) {
	p := dataShrinkProfile(t, "  reorganize_below_density_percent: 65\nindex:\n  page_count_floor: 1000\n")
	r := &fakePlanReader{
		inv:     []mssql.InventoryObject{{Schema: "dbo", Table: "A", ObjectID: 1, IndexID: 1, IndexName: "PK_A", Type: 1, PartitionNumber: 1, SizeMB: 100}},
		density: map[int64]float64{1: 40},
		pages:   map[int64]int64{1: 5000},
	}
	nm, advisories, err := planShrink(context.Background(), r, p, "DB", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("planShrink: %v", err)
	}
	if nm == nil || len(nm.manifest.Operations) != 2 {
		t.Fatalf("want reorganize+shrink manifest, got %+v", nm)
	}
	if len(advisories) != 0 {
		t.Errorf("no heaps in fixture, want 0 advisories, got %+v", advisories)
	}
}

func TestPlanShrinkPreReorganizeOff(t *testing.T) {
	p := dataShrinkProfile(t, "  pre_reorganize: false\n")
	// Even with a low-density index available, pre_reorganize:false yields shrink-only
	// and never calls AnalyzePreShrink. A reader that panics proves it is not called.
	nm, _, err := planShrink(context.Background(), panicReader{}, p, "DB", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("planShrink: %v", err)
	}
	if len(nm.manifest.Operations) != 1 {
		t.Errorf("want shrink-only, got %d ops", len(nm.manifest.Operations))
	}
}

// panicReader fails the test if any Reader method is called.
type panicReader struct{}

func (panicReader) ObjectInventory(context.Context) ([]mssql.InventoryObject, error) {
	panic("AnalyzePreShrink must not run when pre_reorganize is off")
}
func (panicReader) PhysicalStats(context.Context, int64, int, *int, string) ([]mssql.PhysicalStats, error) {
	panic("unexpected")
}
func (panicReader) EstimateCompression(context.Context, string, string, int, *int, string) ([]mssql.CompressionSaving, error) {
	panic("unexpected")
}
func (panicReader) IndexOperationalStats(context.Context, int64, int, *int) ([]mssql.OperationalStats, error) {
	panic("unexpected")
}
func (panicReader) StatsProperties(context.Context, int64) ([]mssql.StatProperty, error) {
	panic("unexpected")
}
```

Add these imports to the test file's import block if not present: `"context"`, `"github.com/rudi-bruchez/SqlGoPace/internal/mssql"`, `"github.com/rudi-bruchez/SqlGoPace/internal/plan"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sqlgopace -run TestPlanShrink -v`
Expected: FAIL (compile error — `planShrink` undefined).

- [ ] **Step 3: Implement `planShrink`**

Add to `cmd/sqlgopace/shrink_plan.go` (and add `"context"` and the `plan` import to its import block):

```go
// planShrink gathers the pre-shrink density (only for an enabled data shrink with
// pre_reorganize on), assembles the shrink manifest, and returns it with the heap
// advisories. Returns (nil, nil, nil) when the shrink section is disabled.
func planShrink(ctx context.Context, r plan.Reader, profile *maint.Profile, db string, logw io.Writer) (*namedManifest, []maint.HeapAdvisory, error) {
	if !profile.Shrink.Enabled {
		return nil, nil, nil
	}
	var pre maint.PreShrinkPlan
	if !profile.Shrink.IsLog() && profile.Shrink.PreReorganizeEnabled() {
		var err error
		if pre, err = plan.AnalyzePreShrink(ctx, r, profile, logw); err != nil {
			return nil, nil, err
		}
	}
	return shrinkManifest(profile, db, pre), pre.HeapAdvisories, nil
}
```

- [ ] **Step 4: Run the planShrink test to verify it passes**

Run: `go test ./cmd/sqlgopace -run TestPlanShrink -v`
Expected: PASS.

- [ ] **Step 5: Wire into `runPlan`**

In `cmd/sqlgopace/plan.go`, in `runPlan`, replace the block:

```go
	pl, err := plan.Analyze(ctx, conn, profile, cats, db, stdout)
	if err != nil {
		return err
	}
	manifests := manifestsFromPlan(pl, db)

	if *explain {
		renderDecisions(stdout, pl)
	}
	if *dryRun {
		return renderManifests(stdout, manifests)
	}
	if err := writeManifests(stdout, out, manifests); err != nil {
		return err
	}
	recordPlanHistory(ctx, stdout, cfg, db, pl, manifests)
	return nil
```

with:

```go
	pl, err := plan.Analyze(ctx, conn, profile, cats, db, stdout)
	if err != nil {
		return err
	}
	manifests := manifestsFromPlan(pl, db)

	// Pre-shrink pass (connected database only): a reorganize→shrink manifest and a
	// heap advisory, when the profile's shrink section is enabled.
	shrinkNM, heapAdvisories, err := planShrink(ctx, conn, profile, db, stdout)
	if err != nil {
		return err
	}
	if shrinkNM != nil {
		manifests = append(manifests, *shrinkNM)
	}

	if *explain {
		renderDecisions(stdout, pl)
	}
	printHeapAdvisory(stdout, heapAdvisories)
	if *dryRun {
		return renderManifests(stdout, manifests)
	}
	if err := writeManifests(stdout, out, manifests); err != nil {
		return err
	}
	if shrinkNM != nil {
		if path, err := writeHeapAdvisorySidecar(out, shrinkNM.filename, db, heapAdvisories); err != nil {
			return err
		} else if path != "" {
			fmt.Fprintf(stdout, "wrote %s\n", path)
		}
	}
	recordPlanHistory(ctx, stdout, cfg, db, pl, manifests)
	return nil
```

- [ ] **Step 6: Run full package tests + build**

Run: `go build ./... && go test -race ./cmd/sqlgopace ./internal/plan ./internal/maint`
Expected: build OK; all tests PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/sqlgopace/shrink_plan.go cmd/sqlgopace/shrink_plan_test.go cmd/sqlgopace/plan.go
git commit -m "feat(cmd): emit pre-shrink reorganize manifest + heap advisory from plan"
```

---

### Task 6: Document the `shrink:` profile section

**Files:**
- Modify: `README.md`

**Interfaces:** none (docs only).

- [ ] **Step 1: Add the section to README**

Find the maintenance-profile documentation in `README.md` (search for `maintenance_profile.yaml` or the `heap:` / `index:` section docs) and add, alongside the other sections:

````markdown
### `shrink:` (pre-shrink reorganize + reclaim)

Optional. When enabled, `sqlgopace plan` emits an extra manifest for the connected
database that reorganizes the low-density rowstore indexes (the tables large deletes
left half-empty), then shrinks the data file. It also prints and writes a `.heaps.yaml`
advisory listing the heaps a shrink cannot benefit from (reorganize cannot compact a
heap's in-row data — rebuild them in a window). Applies to the connected database only.

```yaml
shrink:
  enabled: true          # off/absent = no shrink manifest (default)
  type: data             # data | log  (log skips reorganize + the advisory)
  files: all             # all | a logical file name
  targetfreespace: 10%   # percent or absolute MB (e.g. 100MB)
  pre_reorganize: true   # false = emit the shrink op alone (default true)
  reorganize_below_density_percent: 65  # reorganize rowstore indexes below this SAMPLED page density
  max_block_minutes: 10  # optional; carried into the shrink op's options
```

Notes:
- The index size floor reuses `index.page_count_floor`.
- Session policy (`ignore_blocked_sessions` / `kill_blocking_sessions`) is not generated —
  add it by editing the generated manifest.
- The reorganize selection runs a SAMPLED `sys.dm_db_index_physical_stats` scan of the
  database's indexes at plan time (heavier than the maintenance pass's LIMITED scan).
````

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: document the shrink profile section"
```

---

### Task 7: Full verification

**Files:** none (verification only).

- [ ] **Step 1: Build, vet, race-test the whole tree**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: build OK; vet clean; all tests PASS.

- [ ] **Step 2: gofmt-check only the files this plan created/edited**

Run (Git Bash): for each new/edited `.go` file `f`, `tr -d '\r' < "$f" | gofmt -l` must print nothing. (Ignore golangci-lint's pre-existing CRLF gofmt noise on untouched files.)

- [ ] **Step 3: Bump the version**

Edit `internal/version/VERSION` to the next minor (`0.8.0` — this is a new feature). Preserve the CRLF line ending: `printf '0.8.0\r\n' > internal/version/VERSION`.

- [ ] **Step 4: Commit the bump**

```bash
git add internal/version/VERSION
git commit -m "chore: bump version to 0.8.0 for pre-shrink reorganize"
```

---

## Self-Review

**Spec coverage:**
- §1 profile `shrink:` section + validation + `type: log` handling → Task 1. ✓
- §2 density-selected reorganizes, never REBUILD, generator-side (no decide.go change) → Tasks 2 (decision), 3 (gather), 4 (assemble). ✓
- §2 self-contained manifest, `pre_reorganize: false` → shrink-only → Tasks 4/5. ✓
- §3 heap advisory (density-filtered, sidecar + stdout, advisory only, candidates-not-tail wording) → Tasks 2 (`HeapAdvisory`), 4 (sidecar/print). ✓
- §4 `rebuild_heap` already exists → no task (verified in the spec). ✓
- Connected-database-only, no `--auto`, no session-policy generation, no multi-db → enforced by wiring in `runPlan` single-db path only (Task 5) + not carrying session rules (Task 4). ✓
- Testing all in pure/fakeable code → Tasks 1–5 tests. ✓

**Placeholder scan:** No TBD/TODO; every code and test step has concrete content. ✓

**Type consistency:** `ShrinkRules`, `PreShrinkPlan`, `ShrinkIndexMeasurement`, `ShrinkHeapMeasurement`, `HeapAdvisory`, `DecidePreShrink`, `AnalyzePreShrink`, `shrinkManifest`, `planShrink`, `writeHeapAdvisorySidecar`, `printHeapAdvisory` — names and signatures match across the tasks that define and consume them. `ddl.Shrink.Options.MaxBlockMinutes` is `*int` (verified in `manifest.go`). `ddl.ReorganizeIndex` fields `Schema/Table/Index/LOBCompaction` match `manifest.go`. ✓
