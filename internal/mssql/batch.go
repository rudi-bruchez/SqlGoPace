package mssql

import (
	"context"
	"database/sql"
	"fmt"
)

const tableRowEstimateSQL = `
SELECT ISNULL(SUM(ps.row_count), 0)
FROM sys.dm_db_partition_stats ps
WHERE ps.object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table))
  AND ps.index_id IN (0, 1);`

// TableRowEstimate returns the approximate row count of [schema].[table] from
// sys.dm_db_partition_stats (heap or clustered index). It is used by the batch-DML
// driver to size the initial batch and report progress; an exact count is neither
// needed nor worth a full scan.
func (c *Conn) TableRowEstimate(ctx context.Context, schema, table string) (int64, error) {
	var rows int64
	err := c.pool.QueryRowContext(ctx, tableRowEstimateSQL,
		sql.Named("schema", schema), sql.Named("table", table)).Scan(&rows)
	if err != nil {
		return 0, fmt.Errorf("table row estimate %s.%s: %w", schema, table, err)
	}
	return rows, nil
}

// KeyColumn is one column of a table's clustered key, in key order, with whether it
// is an integer type — the only kind the key_range batch walk supports in this
// iteration.
type KeyColumn struct {
	Name      string
	IsInteger bool
	// IsUnique is the clustered index's own uniqueness, repeated on each of its key
	// columns. It is a property of the index, not of the column, but the key_range
	// walk only ever asks it of a single-column key, and carrying it here keeps
	// ClusteringKeyColumns to one return value.
	IsUnique bool
}

const clusteringKeyColumnsSQL = `
SELECT c.name,
       CASE WHEN c.system_type_id IN (48, 52, 56, 127) THEN 1 ELSE 0 END, -- tinyint/smallint/int/bigint
       i.is_unique
FROM sys.indexes i
JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE i.object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table))
  AND i.index_id = 1       -- clustered index
  AND ic.key_ordinal > 0   -- key columns only (exclude INCLUDE columns)
ORDER BY ic.key_ordinal;`

// ClusteringKeyColumns returns the clustered-index key columns of [schema].[table]
// in key order, each carrying the index's uniqueness. It is empty for a heap (no
// clustered index); the batch-DML driver uses it to infer, type-check and bound a
// key_range key. A clustered index is not required to be unique, and the walk is only
// row-bounded when it is — see resolveKeyColumn.
func (c *Conn) ClusteringKeyColumns(ctx context.Context, schema, table string) ([]KeyColumn, error) {
	rows, err := c.pool.QueryContext(ctx, clusteringKeyColumnsSQL,
		sql.Named("schema", schema), sql.Named("table", table))
	if err != nil {
		return nil, fmt.Errorf("clustering key columns %s.%s: %w", schema, table, err)
	}
	defer func() { _ = rows.Close() }()

	var cols []KeyColumn
	for rows.Next() {
		var k KeyColumn
		if err := rows.Scan(&k.Name, &k.IsInteger, &k.IsUnique); err != nil {
			return nil, fmt.Errorf("scan key column: %w", err)
		}
		cols = append(cols, k)
	}
	return cols, rows.Err()
}

// QueryInt runs a scalar query on the execution connection and returns its integer
// value, with ok=false when the scalar is NULL (or no row). The batch-DML key_range
// walk uses it to read the next watermark (NULL means the walk is exhausted).
func (c *Conn) QueryInt(ctx context.Context, statement string) (int64, bool, error) {
	var v sql.NullInt64
	if err := c.execSession().QueryRowContext(ctx, statement).Scan(&v); err != nil {
		return 0, false, fmt.Errorf("query int: %w", err)
	}
	return v.Int64, v.Valid, nil
}

const dmlPermissionSQL = `
SELECT ISNULL(HAS_PERMS_BY_NAME(QUOTENAME(@schema) + '.' + QUOTENAME(@table), 'OBJECT', @perm), 0);`

// HasDMLPermission reports whether the connected login holds the given permission
// ("UPDATE" or "DELETE") on [schema].[table]. Checked in preflight so a missing
// grant fails the manifest with an actionable message before any DML is issued.
func (c *Conn) HasDMLPermission(ctx context.Context, schema, table, perm string) (bool, error) {
	return c.existsScalar(ctx, dmlPermissionSQL,
		sql.Named("schema", schema), sql.Named("table", table), sql.Named("perm", perm))
}
