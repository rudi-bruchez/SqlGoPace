package ddl_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// TestMarshalManifestRoundTrip is the safety net for the manifest writer: every
// generated operation must survive Marshal → Parse unchanged.
func TestMarshalManifestRoundTrip(t *testing.T) {
	in := &ddl.Manifest{
		Description:            "Maintenance plan for MYDB",
		Database:               "MYDB",
		OnFailure:              ddl.OnFailureContinue,
		Intent:                 ddl.IntentCompression,
		AbortBlockingResumable: true,
		IgnoreBlockedSessions: []ddl.IgnoredSession{
			{AppName: "^SQLAgent", LoginName: "svc_reporting"},
			{SessionID: intPtr(57)},
			{Statement: `dbo\.Report`},
		},
		Window: &ddl.Window{Start: "22:00", End: "05:00", Days: []string{"Sat", "Sun"}},
		Operations: []ddl.Operation{
			ddl.RebuildIndex{Schema: "dbo", Table: "ORDERS", Index: "PK_ORDERS", DataCompression: "PAGE", Intent: ddl.IntentFragmentation},
			// A forced-off *bool override must survive the rewrite: nil=auto, &false=force off.
			ddl.RebuildIndex{Schema: "dbo", Table: "ORDERS", Index: "IX_OFF", Options: ddl.OptionOverrides{Online: boolPtr(false), Resumable: boolPtr(false)}},
			ddl.RebuildIndex{Schema: "dbo", Table: "ORDERS", Index: "IX_PART", Partition: intPtr(3), DataCompression: "ROW"},
			ddl.ReorganizeIndex{Schema: "dbo", Table: "T", Index: "IX", LOBCompaction: true},
			ddl.ReorganizeIndex{Schema: "dbo", Table: "T", Index: "IX2", Partition: intPtr(2)},
			ddl.RebuildHeap{Schema: "dbo", Table: "H", DataCompression: "PAGE"},
			ddl.UpdateStatistics{Schema: "dbo", Table: "T", Statistic: "ST", FullScan: true},
			ddl.UpdateStatistics{Schema: "dbo", Table: "T", SamplePercent: intPtr(30)},
			ddl.CheckDB{Database: "MYDB", PhysicalOnly: true},
			// shrink and batch fold a sub-type into CommandType (shrink_data/shrink_log,
			// batch_update/batch_delete): the manifest discriminator is "shrink" / "batch_*",
			// so these guard MarshalManifest against writing an unparseable "operation:".
			ddl.Shrink{Type: "data", Files: "all", TargetFreeSpace: "10%"},
			ddl.Shrink{Type: "log", Files: "MYDB_log", TargetFreeSpace: "100MB"},
			ddl.BatchDML{Verb: "update", Schema: "dbo", Table: "T", SetRaw: "status = 'X'", WhereRaw: "status IS NULL"},
			ddl.BatchDML{Verb: "delete", Schema: "dbo", Table: "Events", WhereRaw: "created_at < '2020-01-01'"},
		},
	}

	data, err := ddl.MarshalManifest(in)
	if err != nil {
		t.Fatalf("MarshalManifest() error = %v", err)
	}

	got, err := ddl.ParseManifest(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseManifest(marshaled) error = %v\n---\n%s", err, data)
	}
	if diff := cmp.Diff(in, got); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s\n--- marshaled ---\n%s", diff, data)
	}
}

// TestMarshalManifestCompactsSequenceElements checks that a mapping inside a sequence
// (a batch WHERE condition) is compacted too: an IS NULL condition has a nil Value, which
// must be omitted (no `value: null` noise) while still round-tripping.
func TestMarshalManifestCompactsSequenceElements(t *testing.T) {
	in := &ddl.Manifest{
		Database: "DB",
		Operations: []ddl.Operation{
			ddl.BatchDML{Verb: "delete", Schema: "dbo", Table: "T",
				Where: []ddl.Condition{{Column: "archived_at", Op: "is null"}}},
		},
	}
	data, err := ddl.MarshalManifest(in)
	if err != nil {
		t.Fatalf("MarshalManifest() error = %v", err)
	}
	if strings.Contains(string(data), "value:") {
		t.Errorf("a nil condition value must be omitted, got:\n%s", data)
	}
	got, err := ddl.ParseManifest(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v\n%s", err, data)
	}
	if diff := cmp.Diff(in, got); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s\n--- marshaled ---\n%s", diff, data)
	}
}

// TestMarshalManifestOmitsEmpty checks the output is clean: no null/false/empty noise.
func TestMarshalManifestOmitsEmpty(t *testing.T) {
	data, err := ddl.MarshalManifest(&ddl.Manifest{
		Database:   "MYDB",
		Operations: []ddl.Operation{ddl.ReorganizeIndex{Schema: "dbo", Table: "T", Index: "IX"}},
	})
	if err != nil {
		t.Fatalf("MarshalManifest() error = %v", err)
	}
	out := string(data)
	for _, banned := range []string{"null", "partition:", "lob_compaction:", "options:", "description:", "on_failure:", "ignore_blocked_sessions:", "abort_blocking_resumable:"} {
		if strings.Contains(out, banned) {
			t.Errorf("output contains %q, want it omitted:\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "operation: reorganize_index") {
		t.Errorf("output missing the operation discriminator:\n%s", out)
	}
}

// TestMarshalManifestAnnotatedEmitsComment checks that a per-op comment is emitted as a
// YAML head comment above the annotated operation, and that it does not break round-trip.
func TestMarshalManifestAnnotatedEmitsComment(t *testing.T) {
	m := &ddl.Manifest{
		Database: "PRODDB",
		Operations: []ddl.Operation{
			ddl.ReorganizeIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "PK"},
		},
	}
	out, err := ddl.MarshalManifestAnnotated(m, map[int]string{0: "confirmed blocker (times_blocked=3)"})
	if err != nil {
		t.Fatalf("MarshalManifestAnnotated: %v", err)
	}
	if !strings.Contains(string(out), "# confirmed blocker (times_blocked=3)") {
		t.Errorf("comment not emitted:\n%s", out)
	}
	// The comment must not break round-trip.
	if _, err := ddl.ParseManifest(bytes.NewReader(out)); err != nil {
		t.Errorf("annotated manifest does not parse: %v", err)
	}
}

// TestMarshalManifestUnannotatedUnchanged checks that MarshalManifest is a pure
// nil-annotated call: its output must stay byte-identical to before the refactor.
func TestMarshalManifestUnannotatedUnchanged(t *testing.T) {
	m := &ddl.Manifest{Operations: []ddl.Operation{ddl.ReorganizeIndex{Schema: "dbo", Table: "A", Index: "IX"}}}
	a, _ := ddl.MarshalManifest(m)
	b, _ := ddl.MarshalManifestAnnotated(m, nil)
	if string(a) != string(b) {
		t.Errorf("nil-annotated output differs from MarshalManifest")
	}
}
