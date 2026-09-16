package run

import (
	"context"
	"fmt"

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
