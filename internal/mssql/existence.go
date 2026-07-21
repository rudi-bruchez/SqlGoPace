package mssql

import (
	"context"
	"database/sql"
	"fmt"
)

// existsScalar runs a query expected to return a single non-zero count or flag
// and reports whether it is positive.
func (c *Conn) existsScalar(ctx context.Context, query string, args ...any) (bool, error) {
	var n int
	if err := c.pool.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return false, fmt.Errorf("existence check: %w", err)
	}
	return n > 0, nil
}

const elevatedAccessSQL = `
SELECT CASE WHEN IS_ROLEMEMBER('db_owner') = 1 OR IS_SRVROLEMEMBER('sysadmin') = 1
            THEN 1 ELSE 0 END;`

// HasElevatedDBAccess reports whether the current login is a member of db_owner
// in the connected database or of the sysadmin server role — the privilege that
// DBCC SHRINKFILE and DBCC CHECKDB both require. Checked in preflight so a missing
// grant fails the manifest with an actionable message before any DBCC is issued.
func (c *Conn) HasElevatedDBAccess(ctx context.Context) (bool, error) {
	return c.existsScalar(ctx, elevatedAccessSQL)
}

const alterAnyConnectionSQL = `
SELECT CASE WHEN IS_SRVROLEMEMBER('sysadmin') = 1
              OR IS_SRVROLEMEMBER('processadmin') = 1
              OR HAS_PERMS_BY_NAME(NULL, NULL, 'ALTER ANY CONNECTION') = 1
            THEN 1 ELSE 0 END;`

// HasAlterAnyConnection reports whether the current login can KILL another session:
// sysadmin/processadmin membership, or the server-level ALTER ANY CONNECTION permission.
// The selective blocker-kill policy (and ABORT_AFTER_WAIT = BLOCKERS) needs it; preflight
// warns — but does not fail — when it is missing so the operator learns kills will no-op.
func (c *Conn) HasAlterAnyConnection(ctx context.Context) (bool, error) {
	return c.existsScalar(ctx, alterAnyConnectionSQL)
}

const tableExistsSQL = `
SELECT CASE WHEN OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table), 'U') IS NOT NULL
            THEN 1 ELSE 0 END;`

// TableExists reports whether the user table [schema].[table] exists.
func (c *Conn) TableExists(ctx context.Context, schema, table string) (bool, error) {
	return c.existsScalar(ctx, tableExistsSQL, sql.Named("schema", schema), sql.Named("table", table))
}

const indexExistsSQL = `
SELECT COUNT(*) FROM sys.indexes
WHERE name = @index
  AND object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table));`

// IndexExists reports whether the named index exists on [schema].[table].
func (c *Conn) IndexExists(ctx context.Context, schema, table, index string) (bool, error) {
	return c.existsScalar(ctx, indexExistsSQL,
		sql.Named("schema", schema), sql.Named("table", table), sql.Named("index", index))
}

const columnExistsSQL = `
SELECT CASE WHEN COL_LENGTH(QUOTENAME(@schema) + '.' + QUOTENAME(@table), @column) IS NOT NULL
            THEN 1 ELSE 0 END;`

// ColumnExists reports whether the named column exists on [schema].[table].
func (c *Conn) ColumnExists(ctx context.Context, schema, table, column string) (bool, error) {
	return c.existsScalar(ctx, columnExistsSQL,
		sql.Named("schema", schema), sql.Named("table", table), sql.Named("column", column))
}

const constraintExistsSQL = `
SELECT COUNT(*) FROM sys.objects
WHERE name = @constraint
  AND parent_object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table));`

// ConstraintExists reports whether the named constraint exists on [schema].[table].
func (c *Conn) ConstraintExists(ctx context.Context, schema, table, constraint string) (bool, error) {
	return c.existsScalar(ctx, constraintExistsSQL,
		sql.Named("schema", schema), sql.Named("table", table), sql.Named("constraint", constraint))
}
