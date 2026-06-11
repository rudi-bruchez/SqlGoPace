package mssql

import (
	"context"
	"database/sql"
	"fmt"
)

// DatabaseInfo describes one user database for multi-database maintenance: whether
// it is eligible to be maintained from this connection, and — when not — why. The
// orchestrator maintains the eligible ones and logs the reason for any it skips
// (spec §17.3).
type DatabaseInfo struct {
	Name             string
	Eligible         bool
	IneligibleReason string // empty when Eligible
}

// userDatabasesSQL classifies every user database (database_id > 4) by eligibility,
// in name order. A database is eligible when it is online, writable, not a snapshot,
// the writable replica here (AG primary / mirroring principal), and accessible to
// the login. The reason is the first failing condition, for a clear skip log.
const userDatabasesSQL = `
SELECT d.name,
    CASE
        WHEN d.state_desc <> 'ONLINE'        THEN 'not online (' + d.state_desc + ')'
        WHEN d.is_read_only = 1              THEN 'read-only'
        WHEN d.source_database_id IS NOT NULL THEN 'database snapshot'
        WHEN ars.role_desc IS NOT NULL AND ars.role_desc <> 'PRIMARY'         THEN 'availability-group secondary'
        WHEN dm.mirroring_role_desc IS NOT NULL AND dm.mirroring_role_desc <> 'PRINCIPAL' THEN 'mirroring ' + dm.mirroring_role_desc
        WHEN ISNULL(HAS_DBACCESS(d.name), 0) <> 1 THEN 'no access for the login'
        ELSE ''
    END AS ineligible_reason
FROM sys.databases d
LEFT JOIN sys.dm_hadr_database_replica_states    rs  ON rs.database_id = d.database_id AND rs.is_local = 1
LEFT JOIN sys.dm_hadr_availability_replica_states ars ON ars.replica_id = rs.replica_id
LEFT JOIN sys.database_mirroring                  dm  ON dm.database_id = d.database_id
WHERE d.database_id > 4
ORDER BY d.name
OPTION (RECOMPILE, MAXDOP 1);`

// UserDatabases lists every user database with its maintenance eligibility, in name
// order. System databases (master/tempdb/model/msdb) are excluded entirely. It runs
// from any connection context (it only reads sys.databases and the HADR/mirroring
// views), so a single connection can survey the whole server before maintenance.
func (c *Conn) UserDatabases(ctx context.Context) ([]DatabaseInfo, error) {
	rows, err := c.pool.QueryContext(ctx, userDatabasesSQL)
	if err != nil {
		return nil, fmt.Errorf("list user databases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DatabaseInfo
	for rows.Next() {
		var (
			name   string
			reason sql.NullString
		)
		if err := rows.Scan(&name, &reason); err != nil {
			return nil, fmt.Errorf("scan user database: %w", err)
		}
		out = append(out, DatabaseInfo{
			Name:             name,
			Eligible:         reason.String == "",
			IneligibleReason: reason.String,
		})
	}
	return out, rows.Err()
}
