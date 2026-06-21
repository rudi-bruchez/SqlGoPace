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

const dmlPermissionSQL = `
SELECT ISNULL(HAS_PERMS_BY_NAME(QUOTENAME(@schema) + '.' + QUOTENAME(@table), 'OBJECT', @perm), 0);`

// HasDMLPermission reports whether the connected login holds the given permission
// ("UPDATE" or "DELETE") on [schema].[table]. Checked in preflight so a missing
// grant fails the manifest with an actionable message before any DML is issued.
func (c *Conn) HasDMLPermission(ctx context.Context, schema, table, perm string) (bool, error) {
	return c.existsScalar(ctx, dmlPermissionSQL,
		sql.Named("schema", schema), sql.Named("table", table), sql.Named("perm", perm))
}
