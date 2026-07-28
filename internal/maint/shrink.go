package maint

import "github.com/rudi-bruchez/SqlGoPace/internal/ddl"

// ShrinkIndexMeasurement is one rowstore index's page density for the pre-shrink
// reorganize decision, aggregated across the index's partitions.
type ShrinkIndexMeasurement struct {
	Schema, Table, Index    string
	PageCount               int64
	AvgPageSpaceUsedPercent float64 // worst (lowest) density across the index's partitions
}

// ShrinkHeapMeasurement is one heap's density for the pre-shrink advisory.
type ShrinkHeapMeasurement struct {
	Schema, Table           string
	SizeMB                  int64
	ForwardedRecordPercent  float64
	AvgPageSpaceUsedPercent float64
}

// HeapAdvisory names a low-density heap the shrink cannot benefit from: reorganize
// cannot compact a heap's in-row data, so the operator rebuilds it (rebuild_heap) in a
// window. Identified by property, not confirmed tail position (see the design spec §3).
type HeapAdvisory struct {
	Schema, Table          string
	SizeMB                 int64
	ForwardedRecordPercent float64
	PageDensityPercent     float64
}

// PreShrinkPlan is the pre-shrink pass output: the reorganizes to run before the
// shrink, and the heap advisories to surface.
type PreShrinkPlan struct {
	Reorganizes    []ddl.ReorganizeIndex
	HeapAdvisories []HeapAdvisory
}

// DecidePreShrink selects the low-density rowstore indexes to reorganize before a
// shrink and the low-density heaps to advise on. An index qualifies when it is at or
// above the index page-count floor and below the shrink density threshold; a heap
// qualifies when at or above the heap min size and below the same threshold. A table an
// override skips is dropped from both. It never emits a REBUILD (a rebuild grows the
// file) — reorganizes only.
func DecidePreShrink(indexes []ShrinkIndexMeasurement, heaps []ShrinkHeapMeasurement, p *Profile) PreShrinkPlan {
	var pl PreShrinkPlan
	threshold := p.Shrink.ReorganizeBelowDensityPercent

	for _, m := range indexes {
		if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
			continue
		}
		if m.PageCount < int64(p.Index.PageCountFloor) || m.AvgPageSpaceUsedPercent >= threshold {
			continue
		}
		pl.Reorganizes = append(pl.Reorganizes, ddl.ReorganizeIndex{
			Schema: m.Schema, Table: m.Table, Index: m.Index, LOBCompaction: p.Index.LOBCompaction,
		})
	}

	for _, m := range heaps {
		if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
			continue
		}
		if m.SizeMB < p.Heap.MinSizeMB || m.AvgPageSpaceUsedPercent >= threshold {
			continue
		}
		pl.HeapAdvisories = append(pl.HeapAdvisories, HeapAdvisory{
			Schema: m.Schema, Table: m.Table, SizeMB: m.SizeMB,
			ForwardedRecordPercent: m.ForwardedRecordPercent, PageDensityPercent: m.AvgPageSpaceUsedPercent,
		})
	}
	return pl
}
