// Package plan is the maintenance analysis layer: it reads live database state
// through a narrow Reader, turns it into maint measurements, and runs the pure
// decision core (maint.Decide) to produce a reviewable Plan. It sits above both
// internal/mssql (the DMV reads) and internal/maint (the pure decisions), so
// internal/maint stays database-free.
package plan

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// Reader is the narrow set of maintenance reads the planner needs.
// *mssql.Conn satisfies it; tests pass a fake.
type Reader interface {
	ObjectInventory(ctx context.Context) ([]mssql.InventoryObject, error)
	PhysicalStats(ctx context.Context, objectID int64, indexID int, partition *int, mode string) ([]mssql.PhysicalStats, error)
	EstimateCompression(ctx context.Context, schema, table string, indexID int, partition *int, setting string) ([]mssql.CompressionSaving, error)
	IndexOperationalStats(ctx context.Context, objectID int64, indexID int, partition *int) ([]mssql.OperationalStats, error)
	StatsProperties(ctx context.Context, objectID int64) ([]mssql.StatProperty, error)
}

// AllCategories is the set of selectable analysis categories.
var AllCategories = []string{"index", "compression", "heaps", "statistics", "checkdb"}

// Categories filters which analysis categories run. An empty set means "all".
type Categories map[string]bool

// Has reports whether a category is selected (an empty set means all are).
func (c Categories) Has(name string) bool { return len(c) == 0 || c[name] }

// ParseCategories parses a comma-separated category list, rejecting unknown names.
func ParseCategories(csv string) (Categories, error) {
	if strings.TrimSpace(csv) == "" {
		return Categories{}, nil
	}
	set := Categories{}
	for _, raw := range strings.Split(csv, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if !slices.Contains(AllCategories, name) {
			return nil, fmt.Errorf("unknown category %q (want any of %s)", name, strings.Join(AllCategories, ", "))
		}
		set[name] = true
	}
	return set, nil
}

// Analyze runs the analysis (cheap inventory → gated reads → decision core) for
// one database and returns the decision plan. It is the shared core of single-
// and multi-database planning and of --auto.
func Analyze(ctx context.Context, r Reader, profile *maint.Profile, cats Categories, db string, logw io.Writer) (maint.Plan, error) {
	in, err := buildInput(ctx, r, profile, cats, db, logw)
	if err != nil {
		return maint.Plan{}, err
	}
	return maint.Decide(in, profile), nil
}

// buildInput runs the cheap inventory sweep, selects candidates per category, and
// gathers the expensive reads only for survivors (spec §4.0/§4.2/§4.6). Per-object
// read failures are logged and skipped, not fatal (spec §4.7).
func buildInput(ctx context.Context, r Reader, p *maint.Profile, cats Categories, db string, logw io.Writer) (maint.Input, error) {
	inv, err := r.ObjectInventory(ctx)
	if err != nil {
		return maint.Input{}, fmt.Errorf("object inventory: %w", err)
	}

	in := maint.Input{ConnDatabase: db}
	wantIndex := cats.Has("index")
	wantComp := cats.Has("compression") && p.Compression.Enabled

	groups, tables := groupInventory(inv)
	for _, g := range groups {
		head := g[0]
		switch {
		case head.IsHeap():
			if cats.Has("heaps") && p.Heap.Enabled {
				if hm, ok := heapMeasurement(ctx, r, p, wantComp, head, logw); ok {
					in.Heaps = append(in.Heaps, hm)
				}
			}
		case head.Type == 1 || head.Type == 2: // rowstore clustered / nonclustered
			if wantIndex || wantComp {
				in.Indexes = append(in.Indexes, indexMeasurements(ctx, r, p, cats, g, logw)...)
			}
		}
	}

	if cats.Has("statistics") && p.Statistics.Enabled {
		for _, tbl := range tables {
			props, err := r.StatsProperties(ctx, tbl.objectID)
			if err != nil {
				fmt.Fprintf(logw, "-- skip statistics on %s.%s: %v\n", tbl.schema, tbl.table, err)
				continue
			}
			for _, sp := range props {
				in.Statistics = append(in.Statistics, maint.StatMeasurement{
					Schema: tbl.schema, Table: tbl.table, Statistic: sp.Name,
					Rows: sp.Rows, ModificationCounter: sp.ModificationCounter,
				})
			}
		}
	}
	return in, nil
}

// indexMeasurements turns one (object, index) partition group into measurements,
// per-partition when the profile asks and the index is partitioned, else whole-index.
func indexMeasurements(ctx context.Context, r Reader, p *maint.Profile, cats Categories, group []mssql.InventoryObject, logw io.Writer) []maint.IndexMeasurement {
	head := group[0]
	clustered := head.IndexID == 1

	// Fragmentation (cheap LIMITED scan), mapped by partition.
	fragByPart := map[int]float64{}
	pageByPart := map[int]int64{}
	if cats.Has("index") {
		ps, err := r.PhysicalStats(ctx, head.ObjectID, head.IndexID, nil, mssql.PhysicalLimited)
		if err != nil {
			fmt.Fprintf(logw, "-- skip index %s.%s.%s: physical stats: %v\n", head.Schema, head.Table, head.IndexName, err)
			return nil
		}
		for _, s := range ps {
			fragByPart[s.PartitionNumber] = s.AvgFragmentationPercent
			pageByPart[s.PartitionNumber] = s.PageCount
		}
	}

	wantComp := cats.Has("compression") && p.Compression.Enabled
	granular := wantComp && p.Compression.PerPartition && len(group) > 1

	if granular {
		out := make([]maint.IndexMeasurement, 0, len(group))
		for _, row := range group {
			part := row.PartitionNumber
			m := maint.IndexMeasurement{
				Schema: row.Schema, Table: row.Table, Index: row.IndexName, Clustered: clustered,
				Partition: &part, PageCount: pageByPart[part], SizeMB: int64(row.SizeMB),
				FragmentationPercent: fragByPart[part], Current: parseCompression(row.Compression),
			}
			if estimable(row) && p.Compression.CompressesObject(row.Schema, row.Table, row.IndexName) {
				m.Estimate = estimateFor(ctx, r, row.Schema, row.Table, row.IndexID, &part, logw)
				m.Write = writeFor(ctx, r, row.ObjectID, row.IndexID, &part)
			}
			out = append(out, m)
		}
		return out
	}

	// Whole-index: aggregate size, take the worst fragmentation.
	var sizeMB, pageCount int64
	var maxFrag float64
	for _, row := range group {
		sizeMB += int64(row.SizeMB)
		pageCount += pageByPart[row.PartitionNumber]
		if f := fragByPart[row.PartitionNumber]; f > maxFrag {
			maxFrag = f
		}
	}
	m := maint.IndexMeasurement{
		Schema: head.Schema, Table: head.Table, Index: head.IndexName, Clustered: clustered,
		PageCount: pageCount, SizeMB: sizeMB, FragmentationPercent: maxFrag, Current: parseCompression(head.Compression),
	}
	if wantComp && estimable(head) && p.Compression.CompressesObject(head.Schema, head.Table, head.IndexName) {
		m.Estimate = estimateFor(ctx, r, head.Schema, head.Table, head.IndexID, nil, logw)
		m.Write = writeFor(ctx, r, head.ObjectID, head.IndexID, nil)
	}
	return []maint.IndexMeasurement{m}
}

// heapMeasurement gathers the SAMPLED scan (forwarded records) for one heap, after
// a cheap size pre-filter. ok is false when the heap is out of size bounds or the
// scan fails.
func heapMeasurement(ctx context.Context, r Reader, p *maint.Profile, wantComp bool, head mssql.InventoryObject, logw io.Writer) (maint.HeapMeasurement, bool) {
	sizeMB := int64(head.SizeMB)
	if sizeMB < p.Heap.MinSizeMB || sizeMB > p.Heap.MaxSizeMB {
		return maint.HeapMeasurement{}, false
	}
	ps, err := r.PhysicalStats(ctx, head.ObjectID, 0, nil, mssql.PhysicalSampled)
	if err != nil {
		fmt.Fprintf(logw, "-- skip heap %s.%s: sampled scan: %v\n", head.Schema, head.Table, err)
		return maint.HeapMeasurement{}, false
	}
	if len(ps) == 0 {
		return maint.HeapMeasurement{}, false
	}
	// Aggregate across partitions (heaps are usually single-partition).
	var forwarded, records int64
	var maxFrag, minPageSpace float64 = 0, 100
	for _, s := range ps {
		forwarded += s.ForwardedRecordCount
		records += s.RecordCount
		if s.AvgFragmentationPercent > maxFrag {
			maxFrag = s.AvgFragmentationPercent
		}
		if s.AvgPageSpaceUsedPercent < minPageSpace {
			minPageSpace = s.AvgPageSpaceUsedPercent
		}
	}
	m := maint.HeapMeasurement{
		Schema: head.Schema, Table: head.Table, SizeMB: sizeMB,
		ForwardedRecordCount: forwarded, RecordCount: records,
		FragmentationPercent: maxFrag, PageSpaceUsedPercent: minPageSpace,
		Current: parseCompression(head.Compression),
	}
	if wantComp && p.Compression.CompressesObject(head.Schema, head.Table, "") {
		m.Estimate = estimateFor(ctx, r, head.Schema, head.Table, 0, nil, logw)
		m.Write = writeFor(ctx, r, head.ObjectID, 0, nil)
	}
	return m, true
}

// estimateFor runs sp_estimate for ROW and PAGE and folds them into one estimate,
// summing across partitions. Best-effort: returns nil on error (the decision then
// proceeds without a compression opinion).
func estimateFor(ctx context.Context, r Reader, schema, table string, indexID int, partition *int, logw io.Writer) *maint.CompressionEstimate {
	rowRes, err := r.EstimateCompression(ctx, schema, table, indexID, partition, "ROW")
	if err != nil {
		fmt.Fprintf(logw, "-- skip compression estimate on %s.%s: %v\n", schema, table, err)
		return nil
	}
	pageRes, err := r.EstimateCompression(ctx, schema, table, indexID, partition, "PAGE")
	if err != nil {
		fmt.Fprintf(logw, "-- skip compression estimate on %s.%s: %v\n", schema, table, err)
		return nil
	}
	var current, row, page float64
	for _, s := range rowRes {
		current += float64(s.CurrentKB)
		row += float64(s.RequestedKB)
	}
	for _, s := range pageRes {
		page += float64(s.RequestedKB)
	}
	return &maint.CompressionEstimate{CurrentKB: current, RowKB: row, PageKB: page}
}

// writeFor reads operational stats and folds them into a write/read split.
// Best-effort: returns nil on error (the decision then proceeds without a cap).
func writeFor(ctx context.Context, r Reader, objectID int64, indexID int, partition *int) *maint.WriteActivity {
	os, err := r.IndexOperationalStats(ctx, objectID, indexID, partition)
	if err != nil {
		return nil
	}
	var w maint.WriteActivity
	for _, s := range os {
		w.Writes += s.LeafInsert + s.LeafUpdate + s.LeafDelete
		w.Reads += s.RangeScan + s.SingletonLookup
	}
	return &w
}

// estimable reports whether an object is worth estimating compression for: a
// rowstore index/heap that is not already PAGE-compressed (nothing higher to try).
func estimable(o mssql.InventoryObject) bool {
	if o.Type != 0 && o.Type != 1 && o.Type != 2 {
		return false // columnstore / XML / spatial use a different model
	}
	c := parseCompression(o.Compression)
	return c == maint.CompressionNone || c == maint.CompressionRow
}

func parseCompression(desc string) maint.Compression {
	switch strings.ToUpper(strings.TrimSpace(desc)) {
	case "ROW":
		return maint.CompressionRow
	case "PAGE":
		return maint.CompressionPage
	case "NONE":
		return maint.CompressionNone
	default:
		return "" // columnstore etc.
	}
}

// tableRef identifies a distinct table for statistics analysis.
type tableRef struct {
	schema, table string
	objectID      int64
}

// groupInventory splits the inventory into contiguous (object, index) partition
// groups (the query is ordered so they are adjacent) and the distinct table list.
func groupInventory(inv []mssql.InventoryObject) ([][]mssql.InventoryObject, []tableRef) {
	var groups [][]mssql.InventoryObject
	var tables []tableRef
	seenTable := map[int64]bool{}

	// Each (object, index) group is a contiguous run in inv (the query orders them
	// adjacent), so groups are sub-slices of inv rather than freshly accumulated.
	start := 0
	for i, o := range inv {
		if i > start && (inv[start].ObjectID != o.ObjectID || inv[start].IndexID != o.IndexID) {
			groups = append(groups, inv[start:i])
			start = i
		}
		if !seenTable[o.ObjectID] {
			seenTable[o.ObjectID] = true
			tables = append(tables, tableRef{schema: o.Schema, table: o.Table, objectID: o.ObjectID})
		}
	}
	if len(inv) > start {
		groups = append(groups, inv[start:])
	}
	return groups, tables
}
