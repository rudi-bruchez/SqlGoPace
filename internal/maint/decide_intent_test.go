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
