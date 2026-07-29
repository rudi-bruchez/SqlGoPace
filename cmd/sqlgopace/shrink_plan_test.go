package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
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

// readFile reads path and fails the test on error.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

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
	if got[100].TimesBlocked != 3 || got[200].TimesBlocked != 1 {
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
	nm, advisories, err := planShrink(context.Background(), r, p, "DB", nil, &bytes.Buffer{})
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
	nm, _, err := planShrink(context.Background(), panicReader{}, p, "DB", nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("planShrink: %v", err)
	}
	if len(nm.manifest.Operations) != 1 {
		t.Errorf("want shrink-only, got %d ops", len(nm.manifest.Operations))
	}
}

// TestPlanShrinkConfirmedIgnoredForLogShrinkWarns covers FIX 3: a non-empty
// --confirmed set is silently discarded when the pass won't run AnalyzePreShrink
// (a log shrink, or pre_reorganize:false). planShrink must warn to logw so the
// operator knows the sidecar had no effect, rather than the set vanishing quietly.
func TestPlanShrinkConfirmedIgnoredForLogShrinkWarns(t *testing.T) {
	p, err := maint.Parse([]byte("shrink:\n  enabled: true\n  type: log\n  files: all\n  targetfreespace: 10%\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	var logbuf bytes.Buffer
	_, _, err = planShrink(context.Background(), panicReader{}, p, "DB", map[int64]maint.Confirmation{1: {TimesBlocked: 2}}, &logbuf)
	if err != nil {
		t.Fatalf("planShrink: %v", err)
	}
	if !strings.Contains(logbuf.String(), "warning") || !strings.Contains(logbuf.String(), "--confirmed") {
		t.Errorf("log = %q, want a warning that --confirmed has no effect", logbuf.String())
	}
}

// TestPlanShrinkConfirmedIgnoredForPreReorganizeOffWarns is the pre_reorganize:false
// analogue of the above.
func TestPlanShrinkConfirmedIgnoredForPreReorganizeOffWarns(t *testing.T) {
	p := dataShrinkProfile(t, "  pre_reorganize: false\n")
	var logbuf bytes.Buffer
	_, _, err := planShrink(context.Background(), panicReader{}, p, "DB", map[int64]maint.Confirmation{1: {TimesBlocked: 2}}, &logbuf)
	if err != nil {
		t.Fatalf("planShrink: %v", err)
	}
	if !strings.Contains(logbuf.String(), "warning") || !strings.Contains(logbuf.String(), "--confirmed") {
		t.Errorf("log = %q, want a warning that --confirmed has no effect", logbuf.String())
	}
}

// TestPlanShrinkConfirmedEmptyNoWarning ensures the warning is only emitted when a
// confirmed set was actually supplied — an empty/nil set is the normal case (no
// sidecar given) and must not print anything.
func TestPlanShrinkConfirmedEmptyNoWarning(t *testing.T) {
	p, err := maint.Parse([]byte("shrink:\n  enabled: true\n  type: log\n  files: all\n  targetfreespace: 10%\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	var logbuf bytes.Buffer
	_, _, err = planShrink(context.Background(), panicReader{}, p, "DB", nil, &logbuf)
	if err != nil {
		t.Fatalf("planShrink: %v", err)
	}
	if strings.Contains(logbuf.String(), "warning") {
		t.Errorf("log = %q, want no warning when confirmed is empty", logbuf.String())
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
