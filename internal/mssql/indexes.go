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

// PartitionCompression is one partition's current data_compression_desc
// (NONE | ROW | PAGE | COLUMNSTORE | COLUMNSTORE_ARCHIVE).
type PartitionCompression struct {
	Partition int
	Desc      string
}

const indexCompressionSQL = `
SELECT p.partition_number, p.data_compression_desc
FROM sys.indexes i
JOIN sys.partitions p ON p.object_id = i.object_id AND p.index_id = i.index_id
WHERE i.object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table))
  AND i.name = @index
ORDER BY p.partition_number;`

// IndexCompression returns the current compression of each partition of
// [schema].[table].[index], in partition order. It is empty when the index does not
// exist. The engine uses it to skip a rebuild whose target compression every relevant
// partition already has (skip_if_satisfied).
func (c *Conn) IndexCompression(ctx context.Context, schema, table, index string) ([]PartitionCompression, error) {
	rows, err := c.pool.QueryContext(ctx, indexCompressionSQL,
		sql.Named("schema", schema), sql.Named("table", table), sql.Named("index", index))
	if err != nil {
		return nil, fmt.Errorf("index compression %s.%s.%s: %w", schema, table, index, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PartitionCompression
	for rows.Next() {
		var (
			p    PartitionCompression
			desc sql.NullString
		)
		if err := rows.Scan(&p.Partition, &desc); err != nil {
			return nil, fmt.Errorf("scan compression row: %w", err)
		}
		p.Desc = desc.String
		out = append(out, p)
	}
	return out, rows.Err()
}
