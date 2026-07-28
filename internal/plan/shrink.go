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
func AnalyzePreShrink(ctx context.Context, r Reader, profile *maint.Profile, logw io.Writer) (maint.PreShrinkPlan, error) {
	inv, err := r.ObjectInventory(ctx)
	if err != nil {
		return maint.PreShrinkPlan{}, fmt.Errorf("object inventory: %w", err)
	}
	groups, _ := groupInventory(inv)

	var indexes []maint.ShrinkIndexMeasurement
	var heaps []maint.ShrinkHeapMeasurement
	for _, g := range groups {
		head := g[0]
		switch {
		case head.IsHeap():
			if m, ok := shrinkHeapMeasurement(ctx, r, profile, head, logw); ok {
				heaps = append(heaps, m)
			}
		case head.Type == 1 || head.Type == 2: // rowstore clustered / nonclustered
			if m, ok := shrinkIndexMeasurement(ctx, r, head, logw); ok {
				indexes = append(indexes, m)
			}
		}
	}
	return maint.DecidePreShrink(indexes, heaps, profile, nil), nil
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
	minDensity := 100.0
	for _, s := range ps {
		pageCount += s.PageCount
		if s.AvgPageSpaceUsedPercent < minDensity {
			minDensity = s.AvgPageSpaceUsedPercent
		}
	}
	return maint.ShrinkIndexMeasurement{
		ObjectID: head.ObjectID,
		Schema:   head.Schema, Table: head.Table, Index: head.IndexName,
		PageCount: pageCount, AvgPageSpaceUsedPercent: minDensity,
	}, true
}

// shrinkHeapMeasurement reads SAMPLED density + forwarded records for one heap, after a
// cheap min-size pre-filter. ok is false when the heap is below the min size, the scan
// fails, or it returns no rows.
func shrinkHeapMeasurement(ctx context.Context, r Reader, p *maint.Profile, head mssql.InventoryObject, logw io.Writer) (maint.ShrinkHeapMeasurement, bool) {
	sizeMB := int64(head.SizeMB)
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
	minDensity := 100.0
	for _, s := range ps {
		forwarded += s.ForwardedRecordCount
		records += s.RecordCount
		if s.AvgPageSpaceUsedPercent < minDensity {
			minDensity = s.AvgPageSpaceUsedPercent
		}
	}
	fwdPct := 0.0
	if records > 0 {
		fwdPct = float64(forwarded) / float64(records) * 100
	}
	return maint.ShrinkHeapMeasurement{
		ObjectID: head.ObjectID,
		Schema:   head.Schema, Table: head.Table, SizeMB: sizeMB,
		ForwardedRecordPercent: fwdPct, AvgPageSpaceUsedPercent: minDensity,
	}, true
}
