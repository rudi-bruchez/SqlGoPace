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

// effectiveIntent resolves an operation's intent against the manifest default:
// the operation's own intent if set, otherwise the manifest's.
func effectiveIntent(manifestIntent ddl.Intent, op ddl.RebuildIndex) ddl.Intent {
	if op.Intent != "" {
		return op.Intent
	}
	return manifestIntent
}

// skipSatisfied reports whether an operation can be skipped because its target state
// already holds, returning a short reason for the log. Only a rebuild_index whose
// effective intent is compression and whose data_compression every relevant partition
// already has is eligible; then the rebuild is a no-op, so a re-run after an
// interruption reuses the finished work cheaply. A read error is treated as "not
// satisfied" (do the rebuild), never a hard failure.
func (e *Engine) skipSatisfied(ctx context.Context, manifestIntent ddl.Intent, op ddl.Operation) (string, bool) {
	if e.compression == nil {
		return "", false
	}
	ri, ok := op.(ddl.RebuildIndex)
	if !ok || ri.DataCompression == "" || effectiveIntent(manifestIntent, ri) != ddl.IntentCompression {
		return "", false
	}
	parts, err := e.compression.IndexCompression(ctx, ri.Schema, ri.Table, ri.Index)
	if err != nil || !compressionSatisfied(parts, ri.DataCompression, ri.Partition) {
		return "", false
	}
	return "already " + strings.ToUpper(ri.DataCompression), true
}
