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
// partition already has.
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

// indexSizeMBSQL sums the used pages of one index (or the heap, when @index is empty)
// across all its partitions. used_page_count is in 8-KB pages, so /128 = MB. A missing
// object yields no rows, which the caller reads as "size unknown".
const indexSizeMBSQL = `
SELECT CAST(CEILING(SUM(ps.used_page_count) / 128.0) AS INT) AS used_mb
FROM sys.dm_db_partition_stats ps
JOIN sys.indexes i
  ON i.object_id = ps.object_id AND i.index_id = ps.index_id
WHERE ps.object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table))
  AND ((@index = N'' AND i.index_id = 0) OR i.name = @index)
  AND (@partition = 0 OR ps.partition_number = @partition);`

// IndexSizeMB returns the used size in MB of [schema].[table]'s index, or of the heap
// when index is empty, summed across partitions. It returns 0 when the object cannot be
// measured (no such object, or no allocated pages); callers treat 0 as "size unknown"
// and must not fail a run on it.
func (c *Conn) IndexSizeMB(ctx context.Context, schema, table, index string, partition *int) (int, error) {
	// 0 means "every partition": partition numbers start at 1, so it cannot collide.
	part := 0
	if partition != nil {
		part = *partition
	}
	var mb sql.NullInt64
	err := c.pool.QueryRowContext(ctx, indexSizeMBSQL,
		sql.Named("schema", schema), sql.Named("table", table), sql.Named("index", index),
		sql.Named("partition", part)).Scan(&mb)
	if err != nil {
		return 0, fmt.Errorf("index size %s.%s.%s: %w", schema, table, index, err)
	}
	return int(mb.Int64), nil
}
