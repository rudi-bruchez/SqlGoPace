// Package maint is the pure decision core of SqlGoPace's maintenance mode: it
// loads the maintenance_profile.yaml rules and (in later phases) turns live
// database measurements into ddl.Operations. Like the ddl package it performs no
// I/O beyond reading the YAML it is handed, which keeps the decision logic
// trivially unit-testable.
package maint

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// ErrInvalidProfile is returned for a structurally valid YAML that violates a
// maintenance-profile rule (out-of-range threshold, unknown enum value, etc.).
var ErrInvalidProfile = errors.New("invalid maintenance profile")

// CeilingAction is what to do with a REBUILD whose object exceeds the configured
// size ceiling: downgrade to a REORGANIZE, or skip it entirely.
type CeilingAction string

const (
	CeilingReorganize CeilingAction = "reorganize"
	CeilingSkip       CeilingAction = "skip"
)

// Compression is a data-compression tier as written in the profile. It renders to
// the T-SQL keyword (NONE/ROW/PAGE) via DataCompression.
type Compression string

const (
	CompressionNone Compression = "none"
	CompressionRow  Compression = "row"
	CompressionPage Compression = "page"
)

// DataCompression returns the T-SQL keyword for the tier (e.g. "PAGE"), or "" for
// the empty (unset) value.
func (c Compression) DataCompression() string {
	if c == "" {
		return ""
	}
	return strings.ToUpper(string(c))
}

// RebuildForbid is the override value that forbids a REBUILD, forcing a REORGANIZE.
const RebuildForbid = "forbid"

// Profile is the full set of maintenance decision rules, loaded from
// maintenance_profile.yaml. It carries no SQL Server logic in code — thresholds
// and policy are data.
type Profile struct {
	Index       IndexRules       `yaml:"index"`
	Compression CompressionRules `yaml:"compression"`
	Heap        HeapRules        `yaml:"heap"`
	Shrink      ShrinkRules      `yaml:"shrink"`
	Statistics  StatisticsRules  `yaml:"statistics"`
	CheckDB     CheckDBRules     `yaml:"checkdb"`
	Scope       ScopeRules       `yaml:"scope"`
	Overrides   []Override       `yaml:"overrides"`
}

// ScopeRules selects which databases the server-wide mode (--all-databases) acts
// on (spec §17.4). It is consulted only in multi-database mode; a single-database
// run ignores it.
type ScopeRules struct {
	Databases DatabaseScope `yaml:"databases"`
}

// DatabaseScope is an include/exclude glob filter on database names. An empty
// Include means "all"; Exclude always wins.
type DatabaseScope struct {
	Include []string `yaml:"include"` // globs; empty = ["*"]
	Exclude []string `yaml:"exclude"` // globs; matched names are dropped
}

// IndexRules drives the REORGANIZE vs REBUILD decision (see spec §5.1).
type IndexRules struct {
	PageCountFloor        int           `yaml:"page_count_floor"`        // skip indexes smaller than this
	ReorganizeFromPercent float64       `yaml:"reorganize_from_percent"` // frag ≥ this → REORGANIZE
	RebuildFromPercent    float64       `yaml:"rebuild_from_percent"`    // frag ≥ this → REBUILD
	RebuildMaxSizeMB      int64         `yaml:"rebuild_max_size_mb"`     // above this, downgrade a REBUILD
	RebuildOverCeiling    CeilingAction `yaml:"rebuild_over_ceiling"`    // reorganize | skip
	LOBCompaction         bool          `yaml:"lob_compaction"`          // WITH (LOB_COMPACTION = ON) on REORGANIZE
}

// CompressionRules drives the NONE/ROW/PAGE decision (see spec §5.2, §5.4).
type CompressionRules struct {
	Enabled                   bool        `yaml:"enabled"`
	PageMinExtraGainPercent   float64     `yaml:"page_min_extra_gain_percent"` // PAGE must beat ROW by ≥ this
	MinGainPercent            float64     `yaml:"min_gain_percent"`            // below this saving vs current → NONE
	WriteIntensiveRatio       float64     `yaml:"write_intensive_ratio"`       // writes/total above this → cap
	WriteIntensiveCompression Compression `yaml:"write_intensive_compression"` // cap value: row | none
	ActivityFloor             int64       `yaml:"activity_floor"`              // min access before the ratio is trusted
	PerPartition              bool        `yaml:"per_partition"`
	Objects                   ObjectScope `yaml:"objects"` // include/exclude filter on which objects may be compressed
}

// ObjectScope is an include/exclude glob filter on which objects the compression
// heuristic may change. Each glob matches an object's "schema.table" (the whole
// table) or "schema.table.index" (one index). An empty Include means "all"; an
// Exclude always wins. It governs only the automatic compression heuristic —
// fragmentation-driven reorganize/rebuild still runs on out-of-scope objects, and
// an explicit `compression:` override pin is honored regardless (spec §5.2).
type ObjectScope struct {
	Include []string `yaml:"include"` // globs; empty = compress any eligible object
	Exclude []string `yaml:"exclude"` // globs; matched objects are never auto-compressed
}

// CompressesObject reports whether an object is in compression scope: with a
// non-empty Include it must match one (allowlist), and it must match no Exclude
// glob (denylist; exclude wins). The index argument is "" for a heap. Each glob is
// tested against both "schema.table" and "schema.table.index", so a two-part glob
// selects a whole table and a three-part glob a single index.
func (r CompressionRules) CompressesObject(schema, table, index string) bool {
	if len(r.Objects.Include) > 0 && !matchObject(r.Objects.Include, schema, table, index) {
		return false
	}
	return !matchObject(r.Objects.Exclude, schema, table, index)
}

// HeapRules drives the heap-rebuild decision (see spec §5.3).
type HeapRules struct {
	Enabled                   bool    `yaml:"enabled"`
	MinSizeMB                 int64   `yaml:"min_size_mb"`
	MaxSizeMB                 int64   `yaml:"max_size_mb"`
	ForwardedRecordPercent    float64 `yaml:"forwarded_record_percent"`
	FragmentationPercent      float64 `yaml:"fragmentation_percent"`
	FreeSpaceDeviationPercent float64 `yaml:"free_space_deviation_percent"`
	Online                    *bool   `yaml:"online"` // nil = auto (matrix)
}

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
	IdentifyTailObject            bool    `yaml:"identify_tail_object"`             // set identify_tail_object on the generated shrink op
}

// PreReorganizeEnabled reports whether the pre-shrink reorganize pass runs. It defaults
// to true when the shrink section is enabled, unless explicitly set to false.
func (r ShrinkRules) PreReorganizeEnabled() bool { return r.PreReorganize == nil || *r.PreReorganize }

// IsLog reports whether this is a log-file shrink (no reorganize, no heap advisory).
func (r ShrinkRules) IsLog() bool { return strings.EqualFold(strings.TrimSpace(r.Type), "log") }

// StatisticsRules drives the UPDATE STATISTICS decision (see spec §5.4).
type StatisticsRules struct {
	Enabled             bool       `yaml:"enabled"`
	Sample              SampleSpec `yaml:"sample"`               // fullscan | {percent: N}
	ModificationPercent *float64   `yaml:"modification_percent"` // nil = dynamic (Ola sqrt) threshold
}

// CheckDBRules drives DBCC CHECKDB generation (see spec §4.5).
type CheckDBRules struct {
	Enabled      bool     `yaml:"enabled"`
	Databases    []string `yaml:"databases"` // empty = the connection's database only
	PhysicalOnly bool     `yaml:"physical_only"`
	DataPurity   bool     `yaml:"data_purity"`
	MaxDOP       *int     `yaml:"maxdop"` // nil = server default
}

// Override pins or forbids the decision for objects whose "schema.table" matches a
// glob (see spec §5, §5.1). The first matching override in declaration order wins.
type Override struct {
	Match       string      `yaml:"match"`       // glob on "schema.table" (e.g. "dbo.AUDIT_*")
	Skip        bool        `yaml:"skip"`        // exclude the object entirely
	Rebuild     string      `yaml:"rebuild"`     // "" | "forbid" (force REORGANIZE)
	Compression Compression `yaml:"compression"` // "" | none | row | page (pin, bypassing the heuristic)
}

// SampleMode selects how UPDATE STATISTICS samples.
type SampleMode int

const (
	// SampleFullScan is the default: WITH FULLSCAN.
	SampleFullScan SampleMode = iota
	// SamplePercent is WITH SAMPLE n PERCENT.
	SamplePercent
)

// SampleSpec is the polymorphic statistics.sample value: the scalar "fullscan", or
// a mapping {percent: N}. Its zero value is FullScan, matching the documented default.
type SampleSpec struct {
	Mode    SampleMode
	Percent int
}

// UnmarshalYAML accepts either the scalar "fullscan" or a {percent: N} mapping.
func (s *SampleSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if !strings.EqualFold(strings.TrimSpace(value.Value), "fullscan") {
			return fmt.Errorf("statistics.sample: %q: want %q or {percent: N}: %w", value.Value, "fullscan", ErrInvalidProfile)
		}
		s.Mode, s.Percent = SampleFullScan, 0
		return nil
	case yaml.MappingNode:
		var m struct {
			Percent int `yaml:"percent"`
		}
		if err := value.Decode(&m); err != nil {
			return err
		}
		s.Mode, s.Percent = SamplePercent, m.Percent
		return nil
	default:
		return fmt.Errorf("statistics.sample: want %q or {percent: N}: %w", "fullscan", ErrInvalidProfile)
	}
}

// Parse decodes a Profile from YAML (strict: unknown fields rejected), applies
// defaults for omitted values, and validates.
func Parse(data []byte) (*Profile, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)

	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode maintenance profile: %w", err)
	}
	p.applyDefaults()
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Load reads a profile from r.
func Load(r io.Reader) (*Profile, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read maintenance profile: %w", err)
	}
	return Parse(data)
}

// LoadFile loads a profile from a file path.
func LoadFile(path string) (*Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open maintenance profile: %w", err)
	}
	defer func() { _ = f.Close() }()

	return Load(f)
}

// OverrideFor returns the first override whose glob matches "schema.table"
// (case-insensitive), and whether one matched. It is the pure override-resolution
// step the decision core builds on.
func (p *Profile) OverrideFor(schema, table string) (Override, bool) {
	target := schema + "." + table
	for _, o := range p.Overrides {
		if matchGlob(o.Match, target) {
			return o, true
		}
	}
	return Override{}, false
}

// ScopeIncludes reports whether the database name passes the scope filter: it must
// match an include glob (default "*") and no exclude glob. Case-insensitive.
func (p *Profile) ScopeIncludes(database string) bool {
	include := p.Scope.Databases.Include
	if len(include) == 0 {
		include = []string{"*"}
	}
	if !matchAnyGlob(include, database) {
		return false
	}
	return !matchAnyGlob(p.Scope.Databases.Exclude, database)
}

// matchGlob reports whether target matches a single glob pattern, case-insensitively.
func matchGlob(pattern, target string) bool {
	matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(target))
	return err == nil && matched
}

// matchAnyGlob reports whether target matches any of the glob patterns.
func matchAnyGlob(patterns []string, target string) bool {
	for _, p := range patterns {
		if matchGlob(p, target) {
			return true
		}
	}
	return false
}

// matchObject reports whether any glob matches the object at table granularity
// ("schema.table") or index granularity ("schema.table.index"). index is "" for a
// heap, leaving only the table identity to match.
func matchObject(patterns []string, schema, table, index string) bool {
	tableID := schema + "." + table
	if matchAnyGlob(patterns, tableID) {
		return true
	}
	return index != "" && matchAnyGlob(patterns, tableID+"."+index)
}

// applyDefaults fills the documented defaults for omitted values. Note: a field
// left at its Go zero value is treated as "unset" and takes the default, so to
// disable a non-zero-defaulted floor (e.g. page_count_floor) set it to a small
// explicit value like 1 rather than 0.
func (p *Profile) applyDefaults() {
	i := &p.Index
	if i.PageCountFloor == 0 {
		i.PageCountFloor = 1000
	}
	if i.ReorganizeFromPercent == 0 {
		i.ReorganizeFromPercent = 5
	}
	if i.RebuildFromPercent == 0 {
		i.RebuildFromPercent = 30
	}
	if i.RebuildMaxSizeMB == 0 {
		i.RebuildMaxSizeMB = 51200
	}
	i.RebuildOverCeiling = CeilingAction(strings.ToLower(string(i.RebuildOverCeiling)))
	if i.RebuildOverCeiling == "" {
		i.RebuildOverCeiling = CeilingReorganize
	}

	c := &p.Compression
	if c.PageMinExtraGainPercent == 0 {
		c.PageMinExtraGainPercent = 10
	}
	if c.MinGainPercent == 0 {
		c.MinGainPercent = 5
	}
	if c.WriteIntensiveRatio == 0 {
		c.WriteIntensiveRatio = 0.30
	}
	c.WriteIntensiveCompression = Compression(strings.ToLower(string(c.WriteIntensiveCompression)))
	if c.WriteIntensiveCompression == "" {
		c.WriteIntensiveCompression = CompressionRow
	}
	if c.ActivityFloor == 0 {
		c.ActivityFloor = 1000
	}

	h := &p.Heap
	if h.MinSizeMB == 0 {
		h.MinSizeMB = 10
	}
	if h.MaxSizeMB == 0 {
		h.MaxSizeMB = 10000
	}
	if h.ForwardedRecordPercent == 0 {
		h.ForwardedRecordPercent = 10
	}
	if h.FragmentationPercent == 0 {
		h.FragmentationPercent = 15
	}
	if h.FreeSpaceDeviationPercent == 0 {
		h.FreeSpaceDeviationPercent = 30
	}

	if p.Shrink.Enabled && p.Shrink.ReorganizeBelowDensityPercent == 0 {
		p.Shrink.ReorganizeBelowDensityPercent = 65
	}

	for idx := range p.Overrides {
		o := &p.Overrides[idx]
		o.Rebuild = strings.ToLower(strings.TrimSpace(o.Rebuild))
		o.Compression = Compression(strings.ToLower(string(o.Compression)))
	}
}

func (p *Profile) validate() error {
	for _, c := range []struct {
		name string
		val  float64
	}{
		{"index.reorganize_from_percent", p.Index.ReorganizeFromPercent},
		{"index.rebuild_from_percent", p.Index.RebuildFromPercent},
		{"compression.page_min_extra_gain_percent", p.Compression.PageMinExtraGainPercent},
		{"compression.min_gain_percent", p.Compression.MinGainPercent},
		{"heap.forwarded_record_percent", p.Heap.ForwardedRecordPercent},
		{"heap.fragmentation_percent", p.Heap.FragmentationPercent},
		{"heap.free_space_deviation_percent", p.Heap.FreeSpaceDeviationPercent},
	} {
		if err := checkPercent(c.name, c.val); err != nil {
			return err
		}
	}

	if p.Index.ReorganizeFromPercent > p.Index.RebuildFromPercent {
		return fmt.Errorf("index.reorganize_from_percent (%g) must be ≤ rebuild_from_percent (%g): %w",
			p.Index.ReorganizeFromPercent, p.Index.RebuildFromPercent, ErrInvalidProfile)
	}
	if p.Index.PageCountFloor < 0 {
		return fmt.Errorf("index.page_count_floor must be ≥ 0: %w", ErrInvalidProfile)
	}
	if p.Index.RebuildMaxSizeMB < 0 {
		return fmt.Errorf("index.rebuild_max_size_mb must be ≥ 0: %w", ErrInvalidProfile)
	}
	if p.Index.RebuildOverCeiling != CeilingReorganize && p.Index.RebuildOverCeiling != CeilingSkip {
		return fmt.Errorf("index.rebuild_over_ceiling = %q, want %q or %q: %w",
			p.Index.RebuildOverCeiling, CeilingReorganize, CeilingSkip, ErrInvalidProfile)
	}

	if p.Compression.WriteIntensiveRatio < 0 || p.Compression.WriteIntensiveRatio > 1 {
		return fmt.Errorf("compression.write_intensive_ratio must be in 0..1: %w", ErrInvalidProfile)
	}
	if wic := p.Compression.WriteIntensiveCompression; wic != CompressionRow && wic != CompressionNone {
		return fmt.Errorf("compression.write_intensive_compression = %q, want %q or %q: %w",
			wic, CompressionRow, CompressionNone, ErrInvalidProfile)
	}
	if p.Compression.ActivityFloor < 0 {
		return fmt.Errorf("compression.activity_floor must be ≥ 0: %w", ErrInvalidProfile)
	}
	if err := validateGlobs("compression.objects", p.Compression.Objects.Include, p.Compression.Objects.Exclude); err != nil {
		return err
	}

	if p.Heap.MinSizeMB < 0 {
		return fmt.Errorf("heap.min_size_mb must be ≥ 0: %w", ErrInvalidProfile)
	}
	if p.Heap.MaxSizeMB < p.Heap.MinSizeMB {
		return fmt.Errorf("heap.max_size_mb (%d) must be ≥ min_size_mb (%d): %w",
			p.Heap.MaxSizeMB, p.Heap.MinSizeMB, ErrInvalidProfile)
	}

	if p.Statistics.Sample.Mode == SamplePercent {
		if pct := p.Statistics.Sample.Percent; pct < 1 || pct > 100 {
			return fmt.Errorf("statistics.sample percent must be in 1..100: %w", ErrInvalidProfile)
		}
	}
	if mp := p.Statistics.ModificationPercent; mp != nil && (*mp < 1 || *mp > 100) {
		return fmt.Errorf("statistics.modification_percent must be in 1..100: %w", ErrInvalidProfile)
	}

	if p.CheckDB.MaxDOP != nil && *p.CheckDB.MaxDOP < 0 {
		return fmt.Errorf("checkdb.maxdop must be ≥ 0: %w", ErrInvalidProfile)
	}

	if err := validateGlobs("scope.databases", p.Scope.Databases.Include, p.Scope.Databases.Exclude); err != nil {
		return err
	}

	for idx, o := range p.Overrides {
		if strings.TrimSpace(o.Match) == "" {
			return fmt.Errorf("overrides[%d]: match is required: %w", idx, ErrInvalidProfile)
		}
		if _, err := path.Match(o.Match, ""); err != nil {
			return fmt.Errorf("overrides[%d]: invalid match glob %q: %w", idx, o.Match, ErrInvalidProfile)
		}
		if o.Rebuild != "" && o.Rebuild != RebuildForbid {
			return fmt.Errorf("overrides[%d]: rebuild = %q, want %q or empty: %w", idx, o.Rebuild, RebuildForbid, ErrInvalidProfile)
		}
		switch o.Compression {
		case "", CompressionNone, CompressionRow, CompressionPage:
		default:
			return fmt.Errorf("overrides[%d]: compression = %q, want none/row/page or empty: %w", idx, o.Compression, ErrInvalidProfile)
		}
	}

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
	return nil
}

// validateGlobs reports the first syntactically invalid glob across the given
// pattern lists, attributing it to the named profile field.
func validateGlobs(name string, lists ...[]string) error {
	for _, list := range lists {
		for _, g := range list {
			if _, err := path.Match(g, ""); err != nil {
				return fmt.Errorf("%s: invalid glob %q: %w", name, g, ErrInvalidProfile)
			}
		}
	}
	return nil
}

// checkPercent validates that a percentage value is in 0..100.
func checkPercent(name string, val float64) error {
	if val < 0 || val > 100 {
		return fmt.Errorf("%s must be in 0..100: %w", name, ErrInvalidProfile)
	}
	return nil
}
