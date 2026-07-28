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

func TestScopeIncludes(t *testing.T) {
	// No scope block → include everything.
	plain, err := maint.Parse([]byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !plain.ScopeIncludes("AnyDatabase") {
		t.Errorf("default (no scope) should include every database")
	}

	p, err := maint.Parse([]byte("scope:\n  databases:\n    include: ['APP_*']\n    exclude: ['*_TEST']\n"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		want bool
	}{
		{"APP_SALES", true},
		{"app_sales", true},       // case-insensitive
		{"APP_SALES_TEST", false}, // included by APP_* but excluded by *_TEST (exclude wins)
		{"OTHER", false},          // not included
	}
	for _, tt := range tests {
		if got := p.ScopeIncludes(tt.name); got != tt.want {
			t.Errorf("ScopeIncludes(%q) = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestScopeInvalidGlob(t *testing.T) {
	if _, err := maint.Parse([]byte("scope:\n  databases:\n    include: ['DB[']\n")); err == nil {
		t.Errorf("Parse(bad scope glob) error = nil, want an error")
	}
}

func TestCompressionCompressesObject(t *testing.T) {
	tests := []struct {
		name                 string
		include, exclude     []string
		schema, table, index string
		want                 bool
	}{
		{"empty filter includes all", nil, nil, "dbo", "T", "IX", true},
		{"include matches table", []string{"dbo.T"}, nil, "dbo", "T", "IX", true},
		{"include matches table glob", []string{"dbo.BIG_*"}, nil, "dbo", "BIG_FACT", "IX", true},
		{"include misses table", []string{"dbo.Other"}, nil, "dbo", "T", "IX", false},
		{"include matches one index", []string{"dbo.T.IX"}, nil, "dbo", "T", "IX", true},
		{"include misses other index", []string{"dbo.T.OTHER"}, nil, "dbo", "T", "IX", false},
		{"exclude wins over include", []string{"dbo.*"}, []string{"dbo.T"}, "dbo", "T", "IX", false},
		{"exclude one index only", nil, []string{"dbo.T.IX"}, "dbo", "T", "IX", false},
		{"exclude spares other index", nil, []string{"dbo.T.OTHER"}, "dbo", "T", "IX", true},
		{"heap matched by table glob", []string{"dbo.*"}, nil, "dbo", "H", "", true},
		{"heap not matched by index glob", []string{"dbo.H.IX"}, nil, "dbo", "H", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := maint.CompressionRules{Objects: maint.ObjectScope{Include: tt.include, Exclude: tt.exclude}}
			if got := r.CompressesObject(tt.schema, tt.table, tt.index); got != tt.want {
				t.Errorf("CompressesObject(%q,%q,%q) = %v, want %v", tt.schema, tt.table, tt.index, got, tt.want)
			}
		})
	}
}

func TestCompressionObjectsInvalidGlob(t *testing.T) {
	if _, err := maint.Parse([]byte("compression:\n  objects:\n    exclude: ['dbo.T[']\n")); err == nil {
		t.Errorf("Parse(bad compression.objects glob) error = nil, want an error")
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
