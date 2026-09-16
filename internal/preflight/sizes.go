package preflight

import (
	"fmt"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
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

// CheckReenabledIndexes guards the one irreversible side effect of a heap rebuild. The
// rebuild recreates every nonclustered index of the table, so a disabled one comes back
// live and uncompressed (verified on a server: docs/specs/OBJECT-SIZES.md "Verified
// behavior"). That undoes a deliberate operator decision, so it fails unless the operation
// opted in. A read error warns rather than fails: sys.dm_db_partition_stats wants VIEW
// DEFINITION, which the documented VIEW SERVER STATE does not imply, and a login missing it
// must not be blocked by a guard that cannot run.
func CheckReenabledIndexes(target string, disabled []string, allowed bool, readErr error) Check {
	const name = "heap rebuild re-enables indexes"
	switch {
	case readErr != nil:
		return Check{name, Warn, fmt.Sprintf(
			"%s: index state could not be read: %v; a disabled index would be re-enabled by the rebuild", target, readErr)}
	case len(disabled) == 0:
		return Check{name, Pass, target + ": no disabled index on the table"}
	case allowed:
		return Check{name, Warn, fmt.Sprintf(
			"%s: rebuild re-enables disabled index(es) %s and rebuilds it live and without its compression (compression metadata is dropped when an index is disabled) — allowed by allow_reenable_disabled_indexes",
			target, strings.Join(disabled, ", "))}
	default:
		return Check{name, Fail, fmt.Sprintf(
			"%s: ALTER TABLE REBUILD re-enables disabled index(es) %s and rebuilds it live and without its compression (compression metadata is dropped when an index is disabled); drop the index, or set allow_reenable_disabled_indexes: true on this operation",
			target, strings.Join(disabled, ", "))}
	}
}

// CheckHeapRebuildScope records, in the .log, what else a heap rebuild rewrites. It is a
// record, not the warning: only FAIL lines reach the console, so the operator is told before
// the fact by the engine's manifest-start line and the dry run, not by this check.
func CheckHeapRebuildScope(target string, rewritten []mssql.StructureSize) Check {
	const name = "heap rebuild scope"
	var (
		parts []string
		count int
	)
	for _, s := range rewritten {
		if s.IndexID == 0 {
			continue
		}
		count++
		parts = append(parts, fmt.Sprintf("%s %s", s.Name, report.HumanizeKB(s.UsedKB)))
	}
	if count == 0 {
		return Check{name, Pass, target + ": no nonclustered index; the rebuild rewrites the heap alone"}
	}
	return Check{name, Warn, fmt.Sprintf(
		"%s: ALTER TABLE REBUILD also rebuilds %d nonclustered index(es): %s; %s rewritten in one transaction",
		target, count, strings.Join(parts, ", "), report.HumanizeKB(SumKB(rewritten)))}
}
