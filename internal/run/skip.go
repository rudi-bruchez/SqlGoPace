package run

import (
	"context"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// CompressionReader reads an index's current per-partition compression, so the engine
// can skip a rebuild whose target every relevant partition already has. *mssql.Conn
// satisfies it; tests supply a fake.
type CompressionReader interface {
	IndexCompression(ctx context.Context, schema, table, index string) ([]mssql.PartitionCompression, error)
}

var _ CompressionReader = (*mssql.Conn)(nil)

// compressionSatisfied reports whether the target compression already holds. For a
// whole-index rebuild (partition nil) every partition must match; for a partition-
// targeted rebuild only that partition must match. It is false for an empty target, an
// empty read (index unknown → do the rebuild), or a targeted partition not found.
func compressionSatisfied(parts []mssql.PartitionCompression, target string, partition *int) bool {
	if target == "" || len(parts) == 0 {
		return false
	}
	matched := 0
	for _, p := range parts {
		if partition != nil && p.Partition != *partition {
			continue
		}
		if !strings.EqualFold(p.Desc, target) {
			return false
		}
		matched++
	}
	return matched > 0
}

// skipSatisfied reports whether an operation can be skipped because its target state
// already holds, returning a short reason for the log. Only a rebuild_index carrying a
// data_compression target is eligible: when every relevant partition is already at that
// compression the rebuild is a no-op, so a re-run after an interruption reuses the
// finished work cheaply. Gated by the manifest's skip_if_satisfied; a read error is
// treated as "not satisfied" (do the rebuild), never a hard failure.
func (e *Engine) skipSatisfied(ctx context.Context, enabled bool, op ddl.Operation) (string, bool) {
	if !enabled || e.compression == nil {
		return "", false
	}
	ri, ok := op.(ddl.RebuildIndex)
	if !ok || ri.DataCompression == "" {
		return "", false
	}
	parts, err := e.compression.IndexCompression(ctx, ri.Schema, ri.Table, ri.Index)
	if err != nil || !compressionSatisfied(parts, ri.DataCompression, ri.Partition) {
		return "", false
	}
	return "already " + strings.ToUpper(ri.DataCompression), true
}
