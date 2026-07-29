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

// Confirmation is how an object_id was confirmed as a shrink blocker, read from a
// .contended.yaml sidecar. ByTail entries (position-confirmed by the tail-object walk) are
// the definitive blocker and sort ahead of lock-confirmed ones.
type Confirmation struct {
	TimesBlocked int  // lock captures; 0 for a tail-position entry
	ByTail       bool // confirmed_by == "tail_position"
	IndexID      int  // tail entries: the allocation unit at the tail
	PageFromEnd  int  // tail entries: distance from file end (smaller = more binding)
}

// DecidePreShrink selects the low-density rowstore indexes to reorganize before a
// shrink and the low-density heaps to advise on. An index qualifies when it is at or
// above the index page-count floor and below the shrink density threshold; a heap
// qualifies when at or above the heap min size and below the same threshold. A table an
// override skips is dropped from both. It never emits a REBUILD (a rebuild grows the
// file) — reorganizes only.
//
// confirmed maps an object_id observed blocking a prior shrink to its Confirmation (nil
// = no capture). Confirmed index-bearing objects are reordered to the head and
// annotated — tail-position confirmations (the definitive blocker) lead, then
// lock-confirmed ones by times_blocked desc; a confirmed-but-dense object is added
// anyway; confirmed heaps are marked CONFIRMED.
func DecidePreShrink(indexes []ShrinkIndexMeasurement, heaps []ShrinkHeapMeasurement, p *Profile, confirmed map[int64]Confirmation) PreShrinkPlan {
	var pl PreShrinkPlan
	threshold := p.Shrink.ReorganizeBelowDensityPercent

	type entry struct {
		op   ddl.ReorganizeIndex
		note string
		conf Confirmation
	}
	var confirmedEntries, rest []entry

	for _, m := range indexes {
		if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
			continue
		}
		tooSmall := m.PageCount < int64(p.Index.PageCountFloor)
		dense := m.AvgPageSpaceUsedPercent >= threshold
		c, isConfirmed := confirmed[m.ObjectID]

		if tooSmall {
			continue // never reorganize a tiny index, confirmed or not
		}
		op := ddl.ReorganizeIndex{Schema: m.Schema, Table: m.Table, Index: m.Index, LOBCompaction: p.Index.LOBCompaction}
		switch {
		case isConfirmed && c.ByTail:
			confirmedEntries = append(confirmedEntries, entry{op,
				fmt.Sprintf("confirmed tail-position blocker (index_id=%d, %d pages from end)", c.IndexID, c.PageFromEnd),
				c})
		case isConfirmed && dense:
			confirmedEntries = append(confirmedEntries, entry{op, "confirmed blocker — added despite density", c})
		case isConfirmed:
			confirmedEntries = append(confirmedEntries, entry{op, fmt.Sprintf("confirmed blocker (times_blocked=%d)", c.TimesBlocked), c})
		case dense:
			continue // density skips it and it is not confirmed
		default:
			rest = append(rest, entry{op, "", Confirmation{}})
		}
	}

	// Tail-position blockers lead (they are the definitive, position-confirmed
	// blocker); among tail entries, closest to the file end is most binding; then
	// lock-confirmed entries by times_blocked desc; original order is stable throughout.
	sort.SliceStable(confirmedEntries, func(i, j int) bool {
		a, b := confirmedEntries[i].conf, confirmedEntries[j].conf
		if a.ByTail != b.ByTail {
			return a.ByTail // tail-position blockers lead
		}
		if a.ByTail && b.ByTail && a.PageFromEnd != b.PageFromEnd {
			return a.PageFromEnd < b.PageFromEnd // closest to the file end is most binding
		}
		return a.TimesBlocked > b.TimesBlocked
	})
	for _, e := range append(confirmedEntries, rest...) {
		pl.Reorganizes = append(pl.Reorganizes, e.op)
		pl.ReorganizeNotes = append(pl.ReorganizeNotes, e.note)
	}

	for _, m := range heaps {
		if ov, _ := p.OverrideFor(m.Schema, m.Table); ov.Skip {
			continue
		}
		if m.SizeMB < p.Heap.MinSizeMB {
			continue
		}
		c, isConfirmed := confirmed[m.ObjectID]
		if m.AvgPageSpaceUsedPercent >= threshold && !isConfirmed {
			continue // dense and not confirmed → not actionable
		}
		pl.HeapAdvisories = append(pl.HeapAdvisories, HeapAdvisory{
			Schema: m.Schema, Table: m.Table, SizeMB: m.SizeMB,
			ForwardedRecordPercent: m.ForwardedRecordPercent, PageDensityPercent: m.AvgPageSpaceUsedPercent,
			Confirmed: isConfirmed, TimesBlocked: c.TimesBlocked,
		})
	}
	return pl
}
