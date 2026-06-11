package mssql

import (
	"context"
	"database/sql"
	"fmt"
)

// IndexInfo describes one concrete index on a table, enough to expand an
// "ALTER INDEX ALL ... REBUILD" into per-index rebuilds.
type IndexInfo struct {
	Name        string
	IndexID     int
	Type        int    // sys.indexes.type (1=clustered, 2=nonclustered, 3=XML, 4=spatial, 5/6=columnstore)
	TypeDesc    string // sys.indexes.type_desc, for logging
	IsClustered bool
}

const rebuildableIndexesSQL = `
SELECT i.name, i.index_id, i.type, i.type_desc
FROM sys.indexes i
WHERE i.object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table))
  AND i.index_id > 0      -- exclude the heap (index_id = 0)
  AND i.is_disabled = 0   -- skip disabled indexes
  AND i.name IS NOT NULL
ORDER BY CASE WHEN i.index_id = 1 THEN 0 ELSE 1 END, i.index_id;`

// RebuildableIndexes returns the rebuildable indexes of [schema].[table],
// clustered first, then by index_id. Disabled indexes and the heap are excluded.
func (c *Conn) RebuildableIndexes(ctx context.Context, schema, table string) ([]IndexInfo, error) {
	rows, err := c.pool.QueryContext(ctx, rebuildableIndexesSQL,
		sql.Named("schema", schema), sql.Named("table", table))
	if err != nil {
		return nil, fmt.Errorf("list indexes on %s.%s: %w", schema, table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []IndexInfo
	for rows.Next() {
		var idx IndexInfo
		if err := rows.Scan(&idx.Name, &idx.IndexID, &idx.Type, &idx.TypeDesc); err != nil {
			return nil, fmt.Errorf("scan index row: %w", err)
		}
		idx.IsClustered = idx.IndexID == 1
		out = append(out, idx)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate index rows: %w", err)
	}
	return out, nil
}
