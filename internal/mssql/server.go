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
	"fmt"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// ServerInfo is the detected state of the target server and database.
type ServerInfo struct {
	EngineEdition int
	MajorVersion  int
	Database      string
	RecoveryModel string // "FULL" | "SIMPLE" | "BULK_LOGGED"
	ADREnabled    bool
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
    CAST(SERVERPROPERTY('EngineEdition') AS int),
    CAST(SERVERPROPERTY('ProductMajorVersion') AS int),
    DB_NAME(),
    CAST(d.recovery_model_desc AS nvarchar(60))
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
		info  ServerInfo
		major sql.NullInt64
	)
	err := conn.QueryRowContext(ctx, detectBaseSQL).
		Scan(&info.EngineEdition, &major, &info.Database, &info.RecoveryModel)
	if err != nil {
		return ServerInfo{}, fmt.Errorf("detect server: %w", err)
	}
	info.MajorVersion = int(major.Int64)

	// ADR is 2019+ (major 15) and Azure (evergreen).
	if info.MajorVersion >= 15 || info.Tier() == ddl.TierAzure {
		if err := conn.QueryRowContext(ctx, detectADRSQL).Scan(&info.ADREnabled); err != nil {
			return ServerInfo{}, fmt.Errorf("detect ADR: %w", err)
		}
	}
	return info, nil
}
