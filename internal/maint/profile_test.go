package maint_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
)

// profilePath points at the canonical default profile shipped at the repo root.
const profilePath = "../../maintenance_profile.yaml"

func TestShippedProfileParses(t *testing.T) {
	p, err := maint.LoadFile(filepath.FromSlash(profilePath))
	if err != nil {
		t.Fatalf("LoadFile(%q) error = %v, want nil", profilePath, err)
	}
	if got, want := p.Index.RebuildFromPercent, 30.0; got != want {
		t.Errorf("Index.RebuildFromPercent = %g, want %g", got, want)
	}
	if got, want := p.Index.RebuildOverCeiling, maint.CeilingReorganize; got != want {
		t.Errorf("Index.RebuildOverCeiling = %q, want %q", got, want)
	}
	if !p.Compression.Enabled || !p.Heap.Enabled || !p.Statistics.Enabled || !p.CheckDB.Enabled {
		t.Errorf("expected all sections enabled in the shipped profile")
	}
	if got, want := p.Statistics.Sample.Mode, maint.SampleFullScan; got != want {
		t.Errorf("Statistics.Sample.Mode = %v, want SampleFullScan", got)
	}
	if got, want := len(p.Overrides), 3; got != want {
		t.Fatalf("len(Overrides) = %d, want %d", got, want)
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	// An (almost) empty profile must come back fully populated with the documented
	// defaults so partial profiles are ergonomic.
	p, err := maint.Parse([]byte("{}\n"))
	if err != nil {
		t.Fatalf("Parse({}) error = %v, want nil", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"index.page_count_floor", p.Index.PageCountFloor, 1000},
		{"index.reorganize_from_percent", p.Index.ReorganizeFromPercent, 5.0},
		{"index.rebuild_from_percent", p.Index.RebuildFromPercent, 30.0},
		{"index.rebuild_max_size_mb", p.Index.RebuildMaxSizeMB, int64(51200)},
		{"index.rebuild_over_ceiling", p.Index.RebuildOverCeiling, maint.CeilingReorganize},
		{"compression.page_min_extra_gain_percent", p.Compression.PageMinExtraGainPercent, 10.0},
		{"compression.min_gain_percent", p.Compression.MinGainPercent, 5.0},
		{"compression.write_intensive_ratio", p.Compression.WriteIntensiveRatio, 0.30},
		{"compression.write_intensive_compression", p.Compression.WriteIntensiveCompression, maint.CompressionRow},
		{"compression.activity_floor", p.Compression.ActivityFloor, int64(1000)},
		{"heap.min_size_mb", p.Heap.MinSizeMB, int64(10)},
		{"heap.max_size_mb", p.Heap.MaxSizeMB, int64(10000)},
		{"heap.forwarded_record_percent", p.Heap.ForwardedRecordPercent, 10.0},
		{"statistics.sample.mode", p.Statistics.Sample.Mode, maint.SampleFullScan},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParseNormalizesEnumCase(t *testing.T) {
	src := "index:\n  rebuild_over_ceiling: SKIP\ncompression:\n  write_intensive_compression: NONE\noverrides:\n  - match: dbo.T\n    compression: PAGE\n"
	p, err := maint.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if p.Index.RebuildOverCeiling != maint.CeilingSkip {
		t.Errorf("RebuildOverCeiling = %q, want %q (case-normalized)", p.Index.RebuildOverCeiling, maint.CeilingSkip)
	}
	if p.Compression.WriteIntensiveCompression != maint.CompressionNone {
		t.Errorf("WriteIntensiveCompression = %q, want %q", p.Compression.WriteIntensiveCompression, maint.CompressionNone)
	}
	if p.Overrides[0].Compression != maint.CompressionPage {
		t.Errorf("override compression = %q, want %q", p.Overrides[0].Compression, maint.CompressionPage)
	}
}

func TestParseSampleSpec(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantMode maint.SampleMode
		wantPct  int
		wantErr  bool
	}{
		{"fullscan scalar", "statistics:\n  sample: fullscan\n", maint.SampleFullScan, 0, false},
		{"percent mapping", "statistics:\n  sample:\n    percent: 30\n", maint.SamplePercent, 30, false},
		{"invalid scalar", "statistics:\n  sample: half\n", 0, 0, true},
		{"percent out of range", "statistics:\n  sample:\n    percent: 0\n", 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := maint.Parse([]byte(tt.yaml))
			if tt.wantErr {
				if !errors.Is(err, maint.ErrInvalidProfile) {
					t.Fatalf("Parse() error = %v, want ErrInvalidProfile", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			if p.Statistics.Sample.Mode != tt.wantMode || p.Statistics.Sample.Percent != tt.wantPct {
				t.Errorf("Sample = {%v, %d}, want {%v, %d}",
					p.Statistics.Sample.Mode, p.Statistics.Sample.Percent, tt.wantMode, tt.wantPct)
			}
		})
	}
}

func TestParseValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"reorganize above rebuild", "index:\n  reorganize_from_percent: 40\n  rebuild_from_percent: 30\n"},
		{"frag percent over 100", "index:\n  rebuild_from_percent: 150\n"},
		{"bad ceiling action", "index:\n  rebuild_over_ceiling: drop\n"},
		{"ratio out of range", "compression:\n  write_intensive_ratio: 1.5\n"},
		{"write intensive page rejected", "compression:\n  write_intensive_compression: page\n"},
		{"heap max below min", "heap:\n  min_size_mb: 100\n  max_size_mb: 50\n"},
		{"modification percent out of range", "statistics:\n  modification_percent: 0\n"},
		{"checkdb negative maxdop", "checkdb:\n  maxdop: -1\n"},
		{"override missing match", "overrides:\n  - rebuild: forbid\n"},
		{"override bad rebuild", "overrides:\n  - match: dbo.T\n    rebuild: drop\n"},
		{"override bad compression", "overrides:\n  - match: dbo.T\n    compression: gzip\n"},
		{"override bad glob", "overrides:\n  - match: \"dbo.[\"\n"},
		{"unknown field", "index:\n  bogus: 1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := maint.Parse([]byte(tt.yaml)); err == nil {
				t.Errorf("Parse() error = nil, want an error for %q", tt.name)
			}
		})
	}
}

func TestOverrideFor(t *testing.T) {
	src := strings.Join([]string{
		"overrides:",
		"  - match: dbo.AUDIT_*",
		"    rebuild: forbid",
		"  - match: dbo.STAGING",
		"    skip: true",
		"  - match: \"*.LEGACY\"",
		"    compression: none",
	}, "\n") + "\n"
	p, err := maint.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}

	tests := []struct {
		name        string
		schema      string
		table       string
		wantMatched bool
		check       func(maint.Override) bool
	}{
		{"glob prefix", "dbo", "AUDIT_2024", true, func(o maint.Override) bool { return o.Rebuild == maint.RebuildForbid }},
		{"exact", "dbo", "STAGING", true, func(o maint.Override) bool { return o.Skip }},
		{"case insensitive", "DBO", "audit_x", true, func(o maint.Override) bool { return o.Rebuild == maint.RebuildForbid }},
		{"schema wildcard", "sales", "LEGACY", true, func(o maint.Override) bool { return o.Compression == maint.CompressionNone }},
		{"no match", "dbo", "ORDERS", false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, matched := p.OverrideFor(tt.schema, tt.table)
			if matched != tt.wantMatched {
				t.Fatalf("OverrideFor(%s.%s) matched = %t, want %t", tt.schema, tt.table, matched, tt.wantMatched)
			}
			if matched && !tt.check(got) {
				t.Errorf("OverrideFor(%s.%s) = %+v, failed its check", tt.schema, tt.table, got)
			}
		})
	}
}

func TestOverrideForFirstMatchWins(t *testing.T) {
	src := "overrides:\n  - match: dbo.*\n    skip: true\n  - match: dbo.ORDERS\n    rebuild: forbid\n"
	p, err := maint.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	got, matched := p.OverrideFor("dbo", "ORDERS")
	if !matched || !got.Skip {
		t.Errorf("OverrideFor(dbo.ORDERS) = %+v (matched=%t), want the first (skip) override", got, matched)
	}
}

func TestCompressionDataCompression(t *testing.T) {
	tests := []struct {
		in   maint.Compression
		want string
	}{
		{maint.CompressionPage, "PAGE"},
		{maint.CompressionRow, "ROW"},
		{maint.CompressionNone, "NONE"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := tt.in.DataCompression(); got != tt.want {
			t.Errorf("Compression(%q).DataCompression() = %q, want %q", tt.in, got, tt.want)
		}
	}
}
