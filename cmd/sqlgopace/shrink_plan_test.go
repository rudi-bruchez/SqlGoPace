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
