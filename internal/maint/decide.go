package maint

import (
	"fmt"
	"math"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// CompressionEstimate holds sp_estimate_data_compression_savings sizes (KB) for
// one object/partition: the current size and the estimated sizes under ROW and
// PAGE. A nil *CompressionEstimate means compression was not estimated.
type CompressionEstimate struct {
	CurrentKB float64
	RowKB     float64
	PageKB    float64
}

// WriteActivity holds the access counters from sys.dm_db_index_operational_stats
// used to detect write-intensive objects. A nil *WriteActivity means it was not read.
type WriteActivity struct {
	Writes int64 // leaf insert + update + delete
	Reads  int64 // range scans + singleton lookups
}

// IndexMeasurement is one index — or one partition of one index — to evaluate for
// REORGANIZE/REBUILD and compression. Estimate and Write are optional.
type IndexMeasurement struct {
	Schema, Table, Index string
	Clustered            bool
	Partition            *int // nil = whole index; set = a single partition
	PageCount            int64
	SizeMB               int64
	FragmentationPercent float64
	Current              Compression // current data_compression_desc
	Estimate             *CompressionEstimate
	Write                *WriteActivity
}

// HeapMeasurement is one heap (a table with no clustered index) to evaluate for
// rebuild. Estimate and Write are optional.
type HeapMeasurement struct {
	Schema, Table        string
	SizeMB               int64
	ForwardedRecordCount int64
	RecordCount          int64
	FragmentationPercent float64
	PageSpaceUsedPercent float64
	Current              Compression
	Estimate             *CompressionEstimate
	Write                *WriteActivity
}

// StatMeasurement is one statistic to evaluate for UPDATE STATISTICS.
type StatMeasurement struct {
	Schema, Table, Statistic string
	Rows                     int64
	ModificationCounter      int64
}

// Decision records one analyzed object, what was decided, and why. Op is nil when
// nothing is emitted (Kind "skip"); otherwise Op is the generated ddl.Operation
// and Kind is its command type. The reason mirrors ddl.Resolve's explanation trail.
type Decision struct {
	Category string // "index" | "heap" | "statistics" | "checkdb"
	Target   string // schema.table.name (+ partition) or database
	Kind     string // ddl command type, or "skip"
	Reason   string
	Op       ddl.Operation // nil when skipped
	Metrics  Metrics       // the measured inputs behind the decision, for history/trends
}

// Metrics captures the measured inputs that drove a decision, so the run history
// can record them for trend analysis. Not every category populates every field
// (a zero WriteRatio or ForwardedPercent means "not measured / not applicable").
type Metrics struct {
	PageCount            int64
	SizeMB               int64
	FragmentationPercent float64
	ForwardedPercent     float64 // heaps
	WriteRatio           float64 // writes / total access, 0 when not measured
	Rows                 int64   // statistics
	ModificationCounter  int64   // statistics
	CurrentCompression   string  // NONE | ROW | PAGE (T-SQL keyword)
	ChosenCompression    string  // effective compression after the op ("" = unchanged/none)
}

// Input bundles the measurements for one analysis pass.
type Input struct {
	Indexes      []IndexMeasurement
	Heaps        []HeapMeasurement
	Statistics   []StatMeasurement
	ConnDatabase string // used for check_db when the profile names no databases
}

// Plan is the decision core's output: the full reasoning trail. Emitted operations
// are read via Operations / OperationsByCategory.
type Plan struct {
	Decisions []Decision
}

// Operations returns every emitted operation, in decision order.
func (pl Plan) Operations() []ddl.Operation {
	var ops []ddl.Operation
	for _, d := range pl.Decisions {
		if d.Op != nil {
			ops = append(ops, d.Op)
		}
	}
	return ops
}

// OperationsByCategory returns the emitted operations for one category.
func (pl Plan) OperationsByCategory(category string) []ddl.Operation {
	var ops []ddl.Operation
	for _, d := range pl.Decisions {
		if d.Op != nil && d.Category == category {
			ops = append(ops, d.Op)
		}
	}
	return ops
}

// Decide runs the full analysis: it decides each measurement, then applies the
// cross-cutting suppression rules — a statistic backing a whole-index rebuild is
// dropped (the rebuild refreshes it with fullscan), and an index operation on a
// table whose heap is rebuilt is dropped (the heap rebuild rebuilds all its
// nonclustered indexes). See spec §3.1.
func Decide(in Input, p *Profile) Plan {
	ds := make([]Decision, 0, len(in.Indexes)+len(in.Heaps))
	for _, m := range in.Indexes {
		ds = append(ds, DecideIndex(m, p))
	}
	for _, m := range in.Heaps {
		ds = append(ds, DecideHeap(m, p))
	}

	rebuiltIndex := map[string]bool{} // schema.table.index of whole-index rebuilds
	heapTable := map[string]bool{}    // schema.table of heap rebuilds
	for _, d := range ds {
		switch op := d.Op.(type) {
		case ddl.RebuildIndex:
			if op.Partition == nil { // a single-partition rebuild only refreshes stats with default sampling
				rebuiltIndex[key(op.Schema, op.Table, op.Index)] = true
			}
		case ddl.RebuildHeap:
			heapTable[key(op.Schema, op.Table, "")] = true
		}
	}

	// Suppress index operations on a table whose heap is being rebuilt.
	for i := range ds {
		if op, ok := ds[i].Op.(ddl.RebuildIndex); ok && heapTable[key(op.Schema, op.Table, "")] {
			ds[i] = skipDecision("index", ds[i].Target, "redundant: heap rebuild rebuilds all nonclustered indexes")
		}
		if op, ok := ds[i].Op.(ddl.ReorganizeIndex); ok && heapTable[key(op.Schema, op.Table, "")] {
			ds[i] = skipDecision("index", ds[i].Target, "redundant: heap rebuild rebuilds all nonclustered indexes")
		}
	}

	// Statistics, with suppression for stats backing a rebuilt index/heap.
	for _, m := range in.Statistics {
		d := DecideStatistic(m, p)
		if d.Op != nil {
			if rebuiltIndex[key(m.Schema, m.Table, m.Statistic)] || heapTable[key(m.Schema, m.Table, "")] {
				d = skipDecision("statistics", d.Target, "redundant: backing index rebuilt in this plan (refreshed with fullscan)")
			}
		}
		ds = append(ds, d)
	}

	ds = append(ds, decideCheckDB(p, in.ConnDatabase)...)
	return Plan{Decisions: ds}
}

// DecideIndex resolves the REORGANIZE/REBUILD and compression decision for one
// index measurement (spec §5.1, §5.2) and attaches its metrics.
func DecideIndex(m IndexMeasurement, p *Profile) Decision {
	d := decideIndex(m, p)
	d.Metrics = Metrics{
		PageCount: m.PageCount, SizeMB: m.SizeMB, FragmentationPercent: m.FragmentationPercent,
		WriteRatio: writeRatioOf(m.Write), CurrentCompression: m.Current.DataCompression(),
		ChosenCompression: chosenCompression(d, m.Current),
	}
	return d
}

func decideIndex(m IndexMeasurement, p *Profile) Decision {
	target := indexLabel(m.Schema, m.Table, m.Index, m.Partition)
	ov, _ := p.OverrideFor(m.Schema, m.Table)
	if ov.Skip {
		return skipDecision("index", target, "override: skip")
	}
	if m.PageCount < int64(p.Index.PageCountFloor) {
		return skipDecision("index", target, fmt.Sprintf("page_count %d below floor %d", m.PageCount, p.Index.PageCountFloor))
	}

	frag := m.FragmentationPercent
	fragRebuild := frag >= p.Index.RebuildFromPercent
	fragReorganize := !fragRebuild && frag >= p.Index.ReorganizeFromPercent

	comp := decideCompression(m.Schema, m.Table, m.Index, m.Current, m.Estimate, m.Write, ov, p)
	needRebuild := fragRebuild || comp.change

	if !needRebuild {
		if fragReorganize {
			return reorganizeDecision(m, p, fmt.Sprintf("fragmentation %.0f%% in [%.0f, %.0f) → reorganize",
				frag, p.Index.ReorganizeFromPercent, p.Index.RebuildFromPercent))
		}
		return skipDecision("index", target, fmt.Sprintf("fragmentation %.0f%% below %.0f%%; %s",
			frag, p.Index.ReorganizeFromPercent, comp.reason))
	}

	// A rebuild is wanted. Honor a forbidding override and the size ceiling — both
	// of which fall back to a reorganize (when fragmentation justifies one) and
	// drop any compression change, since only a rebuild can apply it.
	fallback := func(reason string) Decision {
		if fragRebuild || fragReorganize {
			note := reason
			if comp.change {
				note += "; compression change dropped (needs rebuild)"
			}
			return reorganizeDecision(m, p, note)
		}
		return skipDecision("index", target, reason+"; nothing to do without a rebuild")
	}
	if ov.Rebuild == RebuildForbid {
		return fallback("rebuild wanted but override forbids it")
	}
	if m.SizeMB > p.Index.RebuildMaxSizeMB {
		if p.Index.RebuildOverCeiling == CeilingSkip {
			return skipDecision("index", target, fmt.Sprintf("rebuild wanted but size %d MB over ceiling %d MB → skip",
				m.SizeMB, p.Index.RebuildMaxSizeMB))
		}
		return fallback(fmt.Sprintf("rebuild wanted but size %d MB over ceiling %d MB", m.SizeMB, p.Index.RebuildMaxSizeMB))
	}

	dataCompression := ""
	if comp.change {
		dataCompression = comp.target.DataCompression()
	}
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
	return Decision{Category: "index", Target: target, Kind: "rebuild_index", Reason: reason, Op: op}
}

// DecideHeap resolves the rebuild decision for one heap measurement (spec §5.3)
// and attaches its metrics.
func DecideHeap(m HeapMeasurement, p *Profile) Decision {
	d := decideHeap(m, p)
	d.Metrics = Metrics{
		SizeMB: m.SizeMB, FragmentationPercent: m.FragmentationPercent,
		ForwardedPercent: ratioPercent(m.ForwardedRecordCount, m.RecordCount),
		WriteRatio:       writeRatioOf(m.Write), CurrentCompression: m.Current.DataCompression(),
		ChosenCompression: chosenCompression(d, m.Current),
	}
	return d
}

func decideHeap(m HeapMeasurement, p *Profile) Decision {
	target := m.Schema + "." + m.Table
	if !p.Heap.Enabled {
		return skipDecision("heap", target, "heap maintenance disabled")
	}
	ov, _ := p.OverrideFor(m.Schema, m.Table)
	if ov.Skip {
		return skipDecision("heap", target, "override: skip")
	}
	if ov.Rebuild == RebuildForbid {
		return skipDecision("heap", target, "override forbids rebuild (a heap cannot be reorganized)")
	}
	if m.SizeMB < p.Heap.MinSizeMB {
		return skipDecision("heap", target, fmt.Sprintf("size %d MB below min %d MB", m.SizeMB, p.Heap.MinSizeMB))
	}
	if m.SizeMB > p.Heap.MaxSizeMB {
		return skipDecision("heap", target, fmt.Sprintf("size %d MB above max %d MB", m.SizeMB, p.Heap.MaxSizeMB))
	}

	forwardedPct := ratioPercent(m.ForwardedRecordCount, m.RecordCount)
	freeSpaceDev := 100 - m.PageSpaceUsedPercent
	trigger := forwardedPct >= p.Heap.ForwardedRecordPercent ||
		m.FragmentationPercent >= p.Heap.FragmentationPercent ||
		freeSpaceDev >= p.Heap.FreeSpaceDeviationPercent
	reason := fmt.Sprintf("forwarded %.0f%% / fragmentation %.0f%% / free-space dev %.0f%%",
		forwardedPct, m.FragmentationPercent, freeSpaceDev)
	if !trigger {
		return skipDecision("heap", target, "no trigger: "+reason)
	}

	comp := decideCompression(m.Schema, m.Table, "", m.Current, m.Estimate, m.Write, ov, p)
	dataCompression := ""
	if comp.change {
		dataCompression = comp.target.DataCompression()
	}
	op := ddl.RebuildHeap{
		Schema: m.Schema, Table: m.Table, DataCompression: dataCompression,
		Options: ddl.OptionOverrides{Online: boolCopy(p.Heap.Online)},
	}
	return Decision{Category: "heap", Target: target, Kind: "rebuild_heap", Reason: reason + "; " + comp.reason, Op: op}
}

// DecideStatistic resolves the UPDATE STATISTICS decision for one statistic
// measurement (spec §5.4) and attaches its metrics. It does not apply the
// rebuild-suppression rule; Decide does, since that needs the whole plan.
func DecideStatistic(m StatMeasurement, p *Profile) Decision {
	d := decideStatistic(m, p)
	d.Metrics = Metrics{Rows: m.Rows, ModificationCounter: m.ModificationCounter}
	return d
}

func decideStatistic(m StatMeasurement, p *Profile) Decision {
	target := m.Schema + "." + m.Table + "." + m.Statistic
	if !p.Statistics.Enabled {
		return skipDecision("statistics", target, "statistics maintenance disabled")
	}
	if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
		return skipDecision("statistics", target, "override: skip")
	}

	threshold := math.Sqrt(1000 * float64(m.Rows))
	source := "dynamic sqrt(1000*rows)"
	if mp := p.Statistics.ModificationPercent; mp != nil {
		threshold = float64(m.Rows) * *mp / 100
		source = fmt.Sprintf("%.0f%% of rows", *mp)
	}
	if float64(m.ModificationCounter) < threshold {
		return skipDecision("statistics", target, fmt.Sprintf("modifications %d below threshold %.0f (%s)",
			m.ModificationCounter, threshold, source))
	}

	op := ddl.UpdateStatistics{Schema: m.Schema, Table: m.Table, Statistic: m.Statistic}
	if p.Statistics.Sample.Mode == SamplePercent {
		pct := p.Statistics.Sample.Percent
		op.SamplePercent = &pct
	} else {
		op.FullScan = true
	}
	return Decision{
		Category: "statistics", Target: target, Kind: "update_statistics",
		Reason: fmt.Sprintf("modifications %d ≥ threshold %.0f (%s)", m.ModificationCounter, threshold, source),
		Op:     op,
	}
}

// decideCheckDB emits one check_db per in-scope database (the profile's list, or
// the connection's database when the list is empty).
func decideCheckDB(p *Profile, connDatabase string) []Decision {
	if !p.CheckDB.Enabled {
		return nil
	}
	databases := p.CheckDB.Databases
	if len(databases) == 0 {
		databases = []string{connDatabase}
	}
	var ds []Decision
	for _, db := range databases {
		if strings.TrimSpace(db) == "" {
			continue
		}
		op := ddl.CheckDB{
			Database: db, PhysicalOnly: p.CheckDB.PhysicalOnly, DataPurity: p.CheckDB.DataPurity,
			Options: ddl.OptionOverrides{MaxDOP: intCopy(p.CheckDB.MaxDOP)},
		}
		ds = append(ds, Decision{Category: "checkdb", Target: db, Kind: "check_db", Reason: "scheduled integrity check", Op: op})
	}
	return ds
}

// compressionOutcome is the result of the compression sub-analysis: the chosen
// tier, whether it should be applied (differs from current and is beneficial), and
// the reasoning.
type compressionOutcome struct {
	target Compression
	change bool
	reason string
}

// decideCompression picks the compression tier for an object (spec §5.2, §5.4): an
// override pin wins; otherwise, if the object is in compression scope, the
// highest-gain tier that clears its bar, capped for write-intensive objects. A NONE
// outcome never forces a decompress — it just means "no beneficial change", so
// change is false unless an override pins it. index is "" for a heap.
func decideCompression(schema, table, index string, current Compression, est *CompressionEstimate, write *WriteActivity, ov Override, p *Profile) compressionOutcome {
	if ov.Compression != "" {
		return compressionOutcome{target: ov.Compression, change: !sameCompression(ov.Compression, current),
			reason: "override pins compression = " + string(ov.Compression)}
	}
	if !p.Compression.Enabled {
		return compressionOutcome{reason: "compression analysis disabled"}
	}
	if !p.Compression.CompressesObject(schema, table, index) {
		return compressionOutcome{reason: "out of compression scope"}
	}
	if est == nil {
		return compressionOutcome{reason: "no compression estimate"}
	}

	c := p.Compression
	gainRow := pctSaving(est.CurrentKB, est.RowKB)
	gainPage := pctSaving(est.CurrentKB, est.PageKB)
	pageExtra := pctSaving(est.RowKB, est.PageKB)

	var desired Compression
	switch {
	case pageExtra >= c.PageMinExtraGainPercent && gainPage >= c.MinGainPercent:
		desired = CompressionPage
	case gainRow >= c.MinGainPercent:
		desired = CompressionRow
	default:
		desired = CompressionNone
	}
	reason := fmt.Sprintf("gain row=%.0f%% page=%.0f%% (page beats row by %.0f%%)", gainRow, gainPage, pageExtra)

	if write != nil {
		total := write.Writes + write.Reads
		if total >= c.ActivityFloor {
			ratio := float64(write.Writes) / float64(total)
			if ratio >= c.WriteIntensiveRatio {
				if capped := minTier(desired, c.WriteIntensiveCompression); capped != desired {
					reason += fmt.Sprintf("; write-intensive %.0f%% caps at %s", ratio*100, c.WriteIntensiveCompression)
					desired = capped
				}
			}
		}
	}

	if desired == CompressionNone {
		return compressionOutcome{target: CompressionNone, reason: reason + " → keep current"}
	}
	if sameCompression(desired, current) {
		return compressionOutcome{target: desired, reason: reason + " → already " + string(desired)}
	}
	return compressionOutcome{target: desired, change: true, reason: reason + " → " + string(desired)}
}

// reorganizeDecision builds a REORGANIZE for an index measurement, carrying the
// profile's LOB-compaction setting and any partition target.
func reorganizeDecision(m IndexMeasurement, p *Profile, reason string) Decision {
	op := ddl.ReorganizeIndex{
		Schema: m.Schema, Table: m.Table, Index: m.Index,
		Partition: m.Partition, LOBCompaction: p.Index.LOBCompaction,
	}
	return Decision{
		Category: "index", Target: indexLabel(m.Schema, m.Table, m.Index, m.Partition),
		Kind: "reorganize_index", Reason: reason, Op: op,
	}
}

func skipDecision(category, target, reason string) Decision {
	return Decision{Category: category, Target: target, Kind: "skip", Reason: reason}
}

// --- small pure helpers --------------------------------------------------

func indexLabel(schema, table, index string, partition *int) string {
	label := schema + "." + table + "." + index
	if partition != nil {
		label += fmt.Sprintf(" (partition %d)", *partition)
	}
	return label
}

func key(schema, table, name string) string {
	return strings.ToLower(schema + "." + table + "." + name)
}

// writeRatioOf returns writes / total access, or 0 when activity is unknown.
func writeRatioOf(w *WriteActivity) float64 {
	if w == nil {
		return 0
	}
	total := w.Writes + w.Reads
	if total == 0 {
		return 0
	}
	return float64(w.Writes) / float64(total)
}

// chosenCompression returns the effective compression after the decision's
// operation (the rebuild's DATA_COMPRESSION), or the current setting when the
// operation leaves compression unchanged.
func chosenCompression(d Decision, current Compression) string {
	switch op := d.Op.(type) {
	case ddl.RebuildIndex:
		if op.DataCompression != "" {
			return op.DataCompression
		}
	case ddl.RebuildHeap:
		if op.DataCompression != "" {
			return op.DataCompression
		}
	}
	return current.DataCompression()
}

// pctSaving returns the percentage saved going from current to candidate sizes.
func pctSaving(current, candidate float64) float64 {
	if current <= 0 {
		return 0
	}
	return (current - candidate) / current * 100
}

// ratioPercent returns num/den as a percentage, guarding a zero denominator.
func ratioPercent(num, den int64) float64 {
	if den <= 0 {
		return 0
	}
	return float64(num) / float64(den) * 100
}

// tierRank orders compression tiers NONE < ROW < PAGE; "" is treated as NONE.
func tierRank(c Compression) int {
	switch c {
	case CompressionRow:
		return 1
	case CompressionPage:
		return 2
	default:
		return 0
	}
}

func sameCompression(a, b Compression) bool { return tierRank(a) == tierRank(b) }

// minTier returns the lower tier of a and ceiling (NONE < ROW < PAGE).
func minTier(a, ceiling Compression) Compression {
	if tierRank(ceiling) < tierRank(a) {
		return ceiling
	}
	return a
}

func boolCopy(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

func intCopy(i *int) *int {
	if i == nil {
		return nil
	}
	v := *i
	return &v
}
