package plan

import (
	"context"
	"fmt"
	"io"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// AnalyzePreShrink gathers SAMPLED page density for the connected database's rowstore
// indexes and heaps and returns the pre-shrink reorganize + heap-advisory plan. The
// SAMPLED scan is heavier than the LIMITED fragmentation scan the maintenance pass uses,
// so it runs only when the shrink section is enabled and the shrink is a data shrink
// with pre_reorganize on (the caller enforces that). Per-object read failures are logged
// and skipped, never fatal.
//
// confirmed maps an object_id observed blocking a prior shrink (from a .contended.yaml
// sidecar) to its Confirmation; nil when there is no capture to prioritize. A confirmed
// id that matches no measured object (already dropped, renamed, or gone) is logged and
// skipped rather than failing the plan.
func AnalyzePreShrink(ctx context.Context, r Reader, profile *maint.Profile, confirmed map[int64]maint.Confirmation, logw io.Writer) (maint.PreShrinkPlan, error) {
	inv, err := r.ObjectInventory(ctx)
	if err != nil {
		return maint.PreShrinkPlan{}, fmt.Errorf("object inventory: %w", err)
	}
	groups, _ := groupInventory(inv)

	// seen records every object_id the scan observed, before any filter (min size,
	// page-count floor, density) drops it — so "not found" below means genuinely
	// absent from the database, not merely filtered out.
	seen := make(map[int64]bool, len(groups))

	var indexes []maint.ShrinkIndexMeasurement
	var heaps []maint.ShrinkHeapMeasurement
	for _, g := range groups {
		head := g[0]
		seen[head.ObjectID] = true
		switch {
		case head.IsHeap():
			if m, ok := shrinkHeapMeasurement(ctx, r, profile, g, logw); ok {
				heaps = append(heaps, m)
			}
		case head.Type == 1 || head.Type == 2: // rowstore clustered / nonclustered
			if m, ok := shrinkIndexMeasurement(ctx, r, head, logw); ok {
				indexes = append(indexes, m)
			}
		}
	}

	for id := range confirmed {
		if !seen[id] {
			fmt.Fprintf(logw, "-- confirmed object %d not found; skipping\n", id)
		}
	}

	return maint.DecidePreShrink(indexes, heaps, profile, confirmed), nil
}

// shrinkIndexMeasurement reads SAMPLED density for one rowstore index, aggregating
// across partitions (total page count, worst density). ok is false on a read error or
// when the scan returns no rows.
func shrinkIndexMeasurement(ctx context.Context, r Reader, head mssql.InventoryObject, logw io.Writer) (maint.ShrinkIndexMeasurement, bool) {
	ps, err := r.PhysicalStats(ctx, head.ObjectID, head.IndexID, nil, mssql.PhysicalSampled)
	if err != nil {
		fmt.Fprintf(logw, "-- skip index %s.%s.%s: sampled scan: %v\n", head.Schema, head.Table, head.IndexName, err)
		return maint.ShrinkIndexMeasurement{}, false
	}
	if len(ps) == 0 {
		return maint.ShrinkIndexMeasurement{}, false
	}
	var pageCount int64
	for _, s := range ps {
		pageCount += s.PageCount
	}
	return maint.ShrinkIndexMeasurement{
		ObjectID: head.ObjectID,
		Schema:   head.Schema, Table: head.Table, Index: head.IndexName,
		PageCount: pageCount, AvgPageSpaceUsedPercent: worstDensity(ps),
	}, true
}

// shrinkHeapMeasurement reads SAMPLED density + forwarded records for one heap, after a
// cheap min-size pre-filter. g is the heap's whole partition group: InventoryObject.SizeMB
// is per-partition, so the pre-filter and the returned SizeMB sum it across g rather than
// reading only g[0]'s (which would undercount a multi-partition heap). ok is false when
// the heap is below the min size, the scan fails, or it returns no rows.
func shrinkHeapMeasurement(ctx context.Context, r Reader, p *maint.Profile, g []mssql.InventoryObject, logw io.Writer) (maint.ShrinkHeapMeasurement, bool) {
	head := g[0]
	var sizeMB int64
	for _, o := range g {
		sizeMB += int64(o.SizeMB)
	}
	if sizeMB < p.Heap.MinSizeMB {
		return maint.ShrinkHeapMeasurement{}, false
	}
	ps, err := r.PhysicalStats(ctx, head.ObjectID, 0, nil, mssql.PhysicalSampled)
	if err != nil {
		fmt.Fprintf(logw, "-- skip heap %s.%s: sampled scan: %v\n", head.Schema, head.Table, err)
		return maint.ShrinkHeapMeasurement{}, false
	}
	if len(ps) == 0 {
		return maint.ShrinkHeapMeasurement{}, false
	}
	var forwarded, records int64
	for _, s := range ps {
		forwarded += s.ForwardedRecordCount
		records += s.RecordCount
	}
	fwdPct := 0.0
	if records > 0 {
		fwdPct = float64(forwarded) / float64(records) * 100
	}
	return maint.ShrinkHeapMeasurement{
		ObjectID: head.ObjectID,
		Schema:   head.Schema, Table: head.Table, SizeMB: sizeMB,
		ForwardedRecordPercent: fwdPct, AvgPageSpaceUsedPercent: worstDensity(ps),
	}, true
}

// worstDensity is the lowest SAMPLED page density across an object's partitions,
// ignoring empty partitions (PageCount 0, whose density reads 0 and would falsely
// dominate). 100 when every partition is empty. Shared by the index and heap
// measurements, which select on the worst-packed partition.
func worstDensity(ps []mssql.PhysicalStats) float64 {
	d := 100.0
	for _, s := range ps {
		if s.PageCount > 0 {
			d = min(d, s.AvgPageSpaceUsedPercent)
		}
	}
	return d
}
