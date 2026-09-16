package run

import (
	"context"
	"fmt"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// SizeReader reads the used size of every structure of a table. *mssql.Conn satisfies it.
type SizeReader interface {
	TableStructureSizes(ctx context.Context, schema, table string, partition *int) ([]mssql.StructureSize, error)
}

// readSizes returns the structures op rewrites, or nil with the error when the read
// failed. A nil reader (tests, and any engine built without one) reads as "not measured".
func readSizes(ctx context.Context, r SizeReader, op ddl.Operation) ([]mssql.StructureSize, error) {
	if r == nil {
		return nil, nil
	}
	schema, table, partition, ok := preflight.SizedOperation(op)
	if !ok {
		return nil, nil
	}
	sizes, err := r.TableStructureSizes(ctx, schema, table, partition)
	if err != nil {
		return nil, err
	}
	return preflight.Rewritten(op, sizes), nil
}

// cachedSizes returns preflight's already-paid read for this operation when it has one,
// and reads otherwise. Preflight ran seconds earlier over the same expanded operation
// list, so a hit saves an unmonitored DMV round trip per heap; a miss means preflight
// could not read it either, or did not need to, and the caller still gets a real answer.
func cachedSizes(ctx context.Context, r SizeReader, op ddl.Operation, cache map[string][]mssql.StructureSize) ([]mssql.StructureSize, error) {
	if schema, table, partition, ok := preflight.SizedOperation(op); ok {
		if sizes, hit := cache[preflight.SizeKey(schema, table, partition)]; hit {
			return preflight.Rewritten(op, sizes), nil
		}
	}
	return readSizes(ctx, r, op)
}

// sizeLines pairs a before and an after read into the report's lines. A structure present
// on one side only still gets a line, with SizeUnknown on the missing side; a structure
// that had no pages before (a disabled index the rebuild re-enabled) is marked.
func sizeLines(before, after []mssql.StructureSize) []report.SizeLine {
	if len(before) == 0 && len(after) == 0 {
		return nil
	}
	byID := map[int]report.SizeLine{}
	var order []int
	for _, s := range before {
		byID[s.IndexID] = report.SizeLine{
			Name: structureName(s), Type: s.TypeDesc, WasDisabled: s.Disabled,
			BeforeKB: s.UsedKB, AfterKB: report.SizeUnknown,
		}
		order = append(order, s.IndexID)
	}
	for _, s := range after {
		line, seen := byID[s.IndexID]
		if !seen {
			line = report.SizeLine{Name: structureName(s), Type: s.TypeDesc, BeforeKB: report.SizeUnknown}
			order = append(order, s.IndexID)
		}
		line.AfterKB = s.UsedKB
		byID[s.IndexID] = line
	}
	out := make([]report.SizeLine, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

func structureName(s mssql.StructureSize) string {
	if s.IndexID == 0 {
		return "heap"
	}
	return s.Name
}

// sizeTotals accumulates the manifest figure per distinct structure: the first measured
// "before" and the last measured "after". A manifest that rebuilds a heap and then one of
// its indexes touches that index twice, and counting it twice would inflate the total.
type sizeTotals struct {
	first map[string]int64
	last  map[string]int64
	order []string
}

func (t *sizeTotals) add(schema, table string, lines []report.SizeLine) {
	for _, l := range lines {
		if l.BeforeKB == report.SizeUnknown || l.AfterKB == report.SizeUnknown {
			continue
		}
		key := fmt.Sprintf("%s.%s.%s", schema, table, l.Name)
		if t.first == nil {
			t.first, t.last = map[string]int64{}, map[string]int64{}
		}
		if _, seen := t.first[key]; !seen {
			t.first[key] = l.BeforeKB
			t.order = append(t.order, key)
		}
		t.last[key] = l.AfterKB
	}
}

func (t *sizeTotals) totals() (beforeKB, afterKB int64, structures int) {
	for _, key := range t.order {
		beforeKB += t.first[key]
		afterKB += t.last[key]
		structures++
	}
	return beforeKB, afterKB, len(t.order)
}

// heapScopeNotice is the manifest-start line for one heap rebuild: what else it rewrites,
// and how much, in one transaction (OBJECT-SIZES.md §5.1).
func heapScopeNotice(index int, op ddl.RebuildHeap, sizes []mssql.StructureSize) string {
	names, count := nonclusteredNames(sizes)
	notice := fmt.Sprintf("operation %d rebuild_heap %s.%s also rebuilds %d nonclustered index(es) (%s): %s rewritten in one transaction",
		index, op.Schema, op.Table, count, strings.Join(names, ", "), report.HumanizeKB(preflight.SumKB(sizes)))
	if disabled := preflight.DisabledNames(sizes); len(disabled) > 0 {
		notice += fmt.Sprintf("; it re-enables disabled index(es) %s, rebuilt without their compression", strings.Join(disabled, ", "))
	}
	return notice
}

// heapScopeUnreadableNotice is the manifest-start line for a heap rebuild whose structure
// sizes could not be established: the read failed, or it succeeded with zero rows, which is
// the same permission-gap case (metadata visibility filters rows rather than raising when VIEW
// DEFINITION is missing — H1, both 2026-09-16 harm reviews). Either way the scope is unknown,
// not empty, and staying silent read as "nothing to rewrite" — say so instead.
func heapScopeUnreadableNotice(index int, op ddl.RebuildHeap, err error) string {
	cause := "no structure rows returned (VIEW DEFINITION may be missing)"
	if err != nil {
		cause = err.Error()
	}
	return fmt.Sprintf(
		"operation %d rebuild_heap %s.%s: structure sizes could not be read (%s); its rebuild scope — "+
			"what else it rewrites, and whether it re-enables a disabled index — is unknown",
		index, op.Schema, op.Table, cause)
}

// heapScopeDetail is the same fact, short enough for a console row.
func heapScopeDetail(sizes []mssql.StructureSize) string {
	_, count := nonclusteredNames(sizes)
	detail := fmt.Sprintf("+%d nonclustered, %s rewritten", count, report.HumanizeKB(preflight.SumKB(sizes)))
	if n := len(preflight.DisabledNames(sizes)); n > 0 {
		detail += fmt.Sprintf(", re-enables %d disabled", n)
	}
	return detail
}

// opDetail joins the manifest-start notes for one operation's console row.
func opDetail(step ddl.PlannedOperation, scope string) string {
	var parts []string
	if RollbackOnCancel(step) {
		parts = append(parts, "cancel only")
	}
	if scope != "" {
		parts = append(parts, scope)
	}
	return strings.Join(parts, " · ")
}

// nonclusteredNames returns the names and count of every structure with IndexID != 0
// (the heap itself, index_id 0, is excluded — this is what "also rebuilds" is naming).
func nonclusteredNames(sizes []mssql.StructureSize) ([]string, int) {
	var names []string
	for _, s := range sizes {
		if s.IndexID == 0 {
			continue
		}
		names = append(names, s.Name)
	}
	return names, len(names)
}
