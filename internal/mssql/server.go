// Package mssql owns all SQL Server connectivity: dedicated execution and
// monitoring connections, target detection (version/edition/ADR/recovery), and
// the DMV reads and control commands (KILL, PAUSE, RESUME) the orchestrator
// drives. It exports concrete types; consumers declare the narrow interfaces
// they need.
package mssql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// ServerInfo is the detected state of the target server and database.
type ServerInfo struct {
	ServerName    string // SERVERPROPERTY('ServerName'), for the console banner
	EngineEdition int
	MajorVersion  int
	Database      string
	RecoveryModel string // "FULL" | "SIMPLE" | "BULK_LOGGED"
	ADREnabled    bool

	// RCSIEnabled is READ_COMMITTED_SNAPSHOT and SnapshotIsolation is
	// ALLOW_SNAPSHOT_ISOLATION. Both are informational for DDL (which conflicts on
	// schema locks, not data locks) but matter for batched DML: with RCSI off, lock
	// escalation to a table X lock blocks readers; with it on, the cost shifts to
	// the tempdb version store.
	RCSIEnabled       bool
	SnapshotIsolation bool
}

// Tier maps the engine edition to a capability tier.
func (s ServerInfo) Tier() ddl.Tier { return ddl.TierFromEngineEdition(s.EngineEdition) }

// Supported reports whether the engine edition is one SqlGoPace handles.
func (s ServerInfo) Supported() bool { return s.Tier() != ddl.TierUnknown }

// Target returns the option-resolution target for this server.
func (s ServerInfo) Target() ddl.Target {
	return ddl.Target{MajorVersion: s.MajorVersion, Tier: s.Tier()}
}

// ContextInfoLiteral renders a 16-byte run marker as a T-SQL varbinary literal
// (0x...) for use in SET CONTEXT_INFO. The marker is generated and persisted by
// the caller and lets crash recovery correlate an orphaned session to its run.
func ContextInfoLiteral(marker [16]byte) string {
	return "0x" + hex.EncodeToString(marker[:])
}

// detectBaseSQL reads the facts available on every supported version.
const detectBaseSQL = `
SELECT
    CAST(SERVERPROPERTY('ServerName') AS nvarchar(256)),
    CAST(SERVERPROPERTY('EngineEdition') AS int),
    CAST(SERVERPROPERTY('ProductMajorVersion') AS int),
    DB_NAME(),
    CAST(d.recovery_model_desc AS nvarchar(60)),
    CONVERT(bit, d.is_read_committed_snapshot_on),
    CONVERT(bit, CASE WHEN d.snapshot_isolation_state = 1 THEN 1 ELSE 0 END)
FROM sys.databases d
WHERE d.database_id = DB_ID();`

// detectADRSQL reads Accelerated Database Recovery, available on SQL Server 2019+
// and Azure (the column does not exist before 2019, so it is queried separately).
const detectADRSQL = `
SELECT CONVERT(bit, is_accelerated_database_recovery_on)
FROM sys.databases
WHERE database_id = DB_ID();`

// DetectServer queries the connection for version, edition, recovery model, and
// (where available) ADR state.
func DetectServer(ctx context.Context, conn *sql.Conn) (ServerInfo, error) {
	var (
		info    ServerInfo
		major   sql.NullInt64
		srvName sql.NullString
	)
	err := conn.QueryRowContext(ctx, detectBaseSQL).
		Scan(&srvName, &info.EngineEdition, &major, &info.Database, &info.RecoveryModel,
			&info.RCSIEnabled, &info.SnapshotIsolation)
	if err != nil {
		return ServerInfo{}, fmt.Errorf("detect server: %w", err)
	}
	info.MajorVersion = int(major.Int64)
	info.ServerName = srvName.String

	// ADR is 2019+ (major 15) and Azure (evergreen).
	if info.MajorVersion >= 15 || info.Tier() == ddl.TierAzure {
		if err := conn.QueryRowContext(ctx, detectADRSQL).Scan(&info.ADREnabled); err != nil {
			return ServerInfo{}, fmt.Errorf("detect ADR: %w", err)
		}
	}
	return info, nil
}

const asyncStatsWALPSQL = `
SELECT CONVERT(bit, value)
FROM sys.database_scoped_configurations
WHERE name = 'ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY';`

// AsyncStatsWaitAtLowPriority reads the ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY
// database-scoped configuration. present is false on a server where the setting does
// not exist (before SQL Server 2022), which is not an error — the caller emits no
// advisory in that case.
func (c *Conn) AsyncStatsWaitAtLowPriority(ctx context.Context) (on bool, present bool, err error) {
	switch err := c.pool.QueryRowContext(ctx, asyncStatsWALPSQL).Scan(&on); {
	case errors.Is(err, sql.ErrNoRows):
		return false, false, nil
	case err != nil:
		return false, false, fmt.Errorf("read ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY: %w", err)
	default:
		return on, true, nil
	}
}
