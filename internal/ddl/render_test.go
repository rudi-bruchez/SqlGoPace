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
