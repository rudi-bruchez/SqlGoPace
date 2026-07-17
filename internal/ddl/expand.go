package ddl

import (
	"fmt"
	"sort"
)

// IndexKind is the storage kind of an index, which determines whether ONLINE,
// RESUMABLE and WAIT_AT_LOW_PRIORITY apply to its rebuild.
type IndexKind int

const (
	// KindUnknown is the zero value: the kind was not resolved (e.g. a single
	// named index from YAML). No type-based gating is applied.
	KindUnknown IndexKind = iota
	KindRowstore
	KindColumnstore
	KindXML
	KindSpatial
	KindOther
)

// String returns a human label used in --explain reasons.
func (k IndexKind) String() string {
	switch k {
	case KindRowstore:
		return "rowstore"
	case KindColumnstore:
		return "columnstore"
	case KindXML:
		return "XML"
	case KindSpatial:
		return "spatial"
	case KindOther:
		return "non-rowstore"
	default:
		return "unknown"
	}
}

// supportsOnlineRebuild reports whether an index of this kind accepts the ONLINE
// family of options (ONLINE, RESUMABLE, WAIT_AT_LOW_PRIORITY). Only rowstore
// indexes do; columnstore/XML/spatial rebuild offline. KindUnknown is treated as
// rowstore so single named indexes keep their version/edition-driven behavior.
func (k IndexKind) supportsOnlineRebuild() bool {
	return k == KindUnknown || k == KindRowstore
}

// IndexDescriptor is the minimal description of a concrete index needed to expand
// an "ALL" rebuild into a per-index rebuild.
type IndexDescriptor struct {
	Name      string
	Clustered bool
	Kind      IndexKind
}

// ExpandRebuildAll returns a manifest where every "ALTER INDEX ALL ... REBUILD"
// is replaced by one rebuild per concrete index (clustered first), so each can
// carry RESUMABLE. Every other operation is passed through unchanged. lookup
// supplies a table's indexes and is called only for ALL rebuilds; the original
// data_compression and option overrides are carried to each rowstore index
// (data_compression is dropped for non-rowstore indexes, which use a different
// compression model). An ALL rebuild over a table with no rebuildable index
// expands to zero operations.
func ExpandRebuildAll(m *Manifest, lookup func(schema, table string) ([]IndexDescriptor, error)) (*Manifest, error) {
	// Copy every manifest-level field, then rebuild only Operations. A field-by-field
	// copy silently drops any field it omits — that bug already reverted on_failure to
	// fail-fast once, and dropped the execution Window again; copying the whole struct
	// makes new manifest fields carry through expansion by default.
	out := *m
	out.Operations = make([]Operation, 0, len(m.Operations))
	for _, op := range m.Operations {
		if !IsAllIndexRebuild(op) {
			out.Operations = append(out.Operations, op)
			continue
		}
		ri := op.(RebuildIndex)
		indexes, err := lookup(ri.Schema, ri.Table)
		if err != nil {
			return nil, fmt.Errorf("expand ALL rebuild on %s.%s: %w", ri.Schema, ri.Table, err)
		}
		for _, idx := range sortClusteredFirst(indexes) {
			out.Operations = append(out.Operations, expandedRebuild(ri, idx))
		}
	}
	return &out, nil
}

// expandedRebuild builds the per-index rebuild for one concrete index. It copies the whole
// operation and overrides only what expansion decides, for the same reason ExpandRebuildAll
// copies the whole manifest: a field-by-field copy silently drops any field it omits. This
// one dropped Partition, so `index: ALL` with `partition: 3` rebuilt every partition of
// every index instead of partition 3 — more work, longer locks, reported as success.
func expandedRebuild(src RebuildIndex, idx IndexDescriptor) RebuildIndex {
	op := src
	op.Index = idx.Name
	op.Kind = idx.Kind
	// data_compression is dropped for non-rowstore indexes, which use a different
	// compression model.
	if !idx.Kind.supportsOnlineRebuild() {
		op.DataCompression = ""
	}
	return op
}

// sortClusteredFirst returns the indexes with the clustered one (if any) first,
// preserving the input order otherwise. It is stable so an already clustered-first
// list from the server is left untouched.
func sortClusteredFirst(indexes []IndexDescriptor) []IndexDescriptor {
	ordered := make([]IndexDescriptor, len(indexes))
	copy(ordered, indexes)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Clustered && !ordered[j].Clustered
	})
	return ordered
}
