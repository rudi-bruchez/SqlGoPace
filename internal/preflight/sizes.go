package preflight

import (
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// SizedOperation reports the table an operation's size is read from, and whether it is
// one of the operations this project measures: rebuild_index, rebuild_heap and
// reorganize_index, the three that exist to make an object smaller or denser.
//
// partition is carried through because `REBUILD PARTITION = n` rewrites one partition and
// needs room for that partition alone.
func SizedOperation(op ddl.Operation) (schema, table string, partition *int, ok bool) {
	switch o := op.(type) {
	case ddl.RebuildIndex:
		return o.Schema, o.Table, o.Partition, true
	case ddl.RebuildHeap:
		return o.Schema, o.Table, nil, true
	case ddl.ReorganizeIndex:
		return o.Schema, o.Table, o.Partition, true
	default:
		return "", "", nil, false
	}
}

// Rewritten returns the structures op rewrites, out of one table's sizes.
//
// A heap rebuild takes them all: "If the table is a heap, all nonclustered indexes are
// rebuilt" (ALTER TABLE docs), verified on a server to include a nonclustered columnstore
// and a disabled index, which it re-enables (OBJECT-SIZES-ANALYSIS.md). An index
// operation takes the one index it names; an unknown name returns nothing, which callers
// read as "size unknown".
func Rewritten(op ddl.Operation, sizes []mssql.StructureSize) []mssql.StructureSize {
	switch o := op.(type) {
	case ddl.RebuildHeap:
		return sizes
	case ddl.RebuildIndex:
		return named(sizes, o.Index)
	case ddl.ReorganizeIndex:
		return named(sizes, o.Index)
	default:
		return nil
	}
}

func named(sizes []mssql.StructureSize, index string) []mssql.StructureSize {
	for _, s := range sizes {
		if strings.EqualFold(s.Name, index) && s.Name != "" {
			return []mssql.StructureSize{s}
		}
	}
	return nil
}

// SumKB totals the used size of the given structures.
func SumKB(sizes []mssql.StructureSize) int64 {
	var kb int64
	for _, s := range sizes {
		kb += s.UsedKB
	}
	return kb
}

// DisabledNames lists the disabled indexes among the given structures, in index_id order.
func DisabledNames(sizes []mssql.StructureSize) []string {
	var out []string
	for _, s := range sizes {
		if s.Disabled {
			out = append(out, s.Name)
		}
	}
	return out
}
