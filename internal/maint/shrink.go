package maint

import (
	"fmt"
	"sort"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// ShrinkIndexMeasurement is one rowstore index's page density for the pre-shrink
// reorganize decision, aggregated across the index's partitions.
type ShrinkIndexMeasurement struct {
	ObjectID                int64
	Schema, Table, Index    string
	PageCount               int64
	AvgPageSpaceUsedPercent float64 // worst (lowest) density across the index's partitions
}

// ShrinkHeapMeasurement is one heap's density for the pre-shrink advisory.
type ShrinkHeapMeasurement struct {
	ObjectID                int64
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
	Confirmed              bool // observed blocking the shrink (from a .contended.yaml)
	TimesBlocked           int  // reaction snapshots that saw it, when Confirmed
}

// PreShrinkPlan is the pre-shrink pass output: the reorganizes to run before the
// shrink, and the heap advisories to surface.
type PreShrinkPlan struct {
	Reorganizes     []ddl.ReorganizeIndex
	ReorganizeNotes []string // parallel to Reorganizes; "" = no annotation
	HeapAdvisories  []HeapAdvisory
}

// DecidePreShrink selects the low-density rowstore indexes to reorganize before a
// shrink and the low-density heaps to advise on. An index qualifies when it is at or
// above the index page-count floor and below the shrink density threshold; a heap
// qualifies when at or above the heap min size and below the same threshold. A table an
// override skips is dropped from both. It never emits a REBUILD (a rebuild grows the
// file) — reorganizes only.
//
// confirmed maps an object_id observed blocking a prior shrink to its times_blocked
// count (nil = no capture). Confirmed index-bearing objects are reordered to the head
// and annotated; a confirmed-but-dense object is added anyway; confirmed heaps are
// marked CONFIRMED.
func DecidePreShrink(indexes []ShrinkIndexMeasurement, heaps []ShrinkHeapMeasurement, p *Profile, confirmed map[int64]int) PreShrinkPlan {
	var pl PreShrinkPlan
	threshold := p.Shrink.ReorganizeBelowDensityPercent

	type entry struct {
		op   ddl.ReorganizeIndex
		note string
		conf int // times_blocked when confirmed, else 0
	}
	var confirmedEntries, rest []entry

	for _, m := range indexes {
		if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
			continue
		}
		tooSmall := m.PageCount < int64(p.Index.PageCountFloor)
		dense := m.AvgPageSpaceUsedPercent >= threshold
		tb, isConfirmed := confirmed[m.ObjectID]

		if tooSmall {
			continue // never reorganize a tiny index, confirmed or not
		}
		op := ddl.ReorganizeIndex{Schema: m.Schema, Table: m.Table, Index: m.Index, LOBCompaction: p.Index.LOBCompaction}
		switch {
		case isConfirmed && dense:
			confirmedEntries = append(confirmedEntries, entry{op, "confirmed blocker — added despite density", tb})
		case isConfirmed:
			confirmedEntries = append(confirmedEntries, entry{op, fmt.Sprintf("confirmed blocker (times_blocked=%d)", tb), tb})
		case dense:
			continue // density skips it and it is not confirmed
		default:
			rest = append(rest, entry{op, "", 0})
		}
	}

	// Confirmed first, by times_blocked desc then original order (stable).
	sort.SliceStable(confirmedEntries, func(i, j int) bool {
		return confirmedEntries[i].conf > confirmedEntries[j].conf
	})
	for _, e := range append(confirmedEntries, rest...) {
		pl.Reorganizes = append(pl.Reorganizes, e.op)
		pl.ReorganizeNotes = append(pl.ReorganizeNotes, e.note)
	}

	for _, m := range heaps {
		if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
			continue
		}
		if m.SizeMB < p.Heap.MinSizeMB || m.AvgPageSpaceUsedPercent >= threshold {
			continue
		}
		tb, isConfirmed := confirmed[m.ObjectID]
		pl.HeapAdvisories = append(pl.HeapAdvisories, HeapAdvisory{
			Schema: m.Schema, Table: m.Table, SizeMB: m.SizeMB,
			ForwardedRecordPercent: m.ForwardedRecordPercent, PageDensityPercent: m.AvgPageSpaceUsedPercent,
			Confirmed: isConfirmed, TimesBlocked: tb,
		})
	}
	return pl
}
