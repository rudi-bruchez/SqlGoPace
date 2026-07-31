package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// LogSpace is the transaction-log usage snapshot. UsedBytes vs an absolute cap
// is the meaningful pressure signal; UsedPercent (of the current file size) is
// secondary because the file autogrows.
type LogSpace struct {
	TotalBytes  int64
	UsedPercent float64
}

// UsedBytes returns the absolute used log space in bytes.
func (l LogSpace) UsedBytes() int64 {
	return int64(float64(l.TotalBytes) * l.UsedPercent / 100)
}

const logSpaceSQL = `
SELECT total_log_size_in_bytes, used_log_space_in_percent
FROM sys.dm_db_log_space_usage
OPTION (RECOMPILE);`

// LogSpace reads current transaction-log usage for the connected database.
func (c *Conn) LogSpace(ctx context.Context) (LogSpace, error) {
	var ls LogSpace
	if err := c.pool.QueryRowContext(ctx, logSpaceSQL).Scan(&ls.TotalBytes, &ls.UsedPercent); err != nil {
		return LogSpace{}, fmt.Errorf("read log space: %w", err)
	}
	return ls, nil
}

const logReuseWaitSQL = `
SELECT log_reuse_wait_desc
FROM sys.databases
WHERE database_id = DB_ID();`

// LogReuseWait returns what is preventing log truncation (e.g. LOG_BACKUP,
// ACTIVE_TRANSACTION, AVAILABILITY_REPLICA).
func (c *Conn) LogReuseWait(ctx context.Context) (string, error) {
	var desc string
	if err := c.pool.QueryRowContext(ctx, logReuseWaitSQL).Scan(&desc); err != nil {
		return "", fmt.Errorf("read log_reuse_wait_desc: %w", err)
	}
	return desc, nil
}

// Progress is a running request's completion estimate. PercentComplete is
// populated for REBUILD/ALTER and during a rollback; Command distinguishes the
// two — during a KILL/abort rollback it reads "KILLED/ROLLBACK", so the percent
// is rollback progress rather than forward progress.
type Progress struct {
	PercentComplete       float64
	EstimatedCompletionMS int64
	ElapsedMS             int64
	Command               string
}

// IsRollback reports whether the request is rolling back, in which case
// PercentComplete is the rollback completion.
func (p Progress) IsRollback() bool {
	return strings.Contains(strings.ToUpper(p.Command), "ROLLBACK")
}

const progressSQL = `
SELECT percent_complete, estimated_completion_time, total_elapsed_time, command
FROM sys.dm_exec_requests
WHERE session_id = @spid;`

// Progress reads completion estimates for the given session. found is false when
// the session has no active request (e.g. the DDL has finished).
func (c *Conn) Progress(ctx context.Context, spid int) (p Progress, found bool, err error) {
	var command sql.NullString
	row := c.pool.QueryRowContext(ctx, progressSQL, sql.Named("spid", spid))
	switch err := row.Scan(&p.PercentComplete, &p.EstimatedCompletionMS, &p.ElapsedMS, &command); {
	case errors.Is(err, sql.ErrNoRows):
		return Progress{}, false, nil
	case err != nil:
		return Progress{}, false, fmt.Errorf("read progress: %w", err)
	default:
		p.Command = command.String
		return p, true, nil
	}
}

// ResumableOp is an in-progress or paused resumable index operation, surviving
// tool and server restarts; consulted on startup to adopt orphaned operations and
// by the abort-resumable command to cancel them. Schema/Table are resolved so the
// operation can be addressed in an ALTER INDEX statement.
type ResumableOp struct {
	Schema          string
	Table           string
	ObjectID        int64
	IndexID         int
	Name            string
	StateDesc       string // "RUNNING" | "PAUSED"
	PercentComplete float64
	LastPauseTime   string // ISO 8601, empty when never paused
}

const resumableOpsSQL = `
SELECT OBJECT_SCHEMA_NAME(object_id), OBJECT_NAME(object_id),
       object_id, index_id, name, state_desc, percent_complete,
       CONVERT(varchar(30), last_pause_time, 126)
FROM sys.index_resumable_operations;`

// ResumableOps lists resumable index operations in the connected database, with
// their object schema/table resolved.
func (c *Conn) ResumableOps(ctx context.Context) ([]ResumableOp, error) {
	rows, err := c.pool.QueryContext(ctx, resumableOpsSQL)
	if err != nil {
		return nil, fmt.Errorf("query resumable operations: %w", err)
	}
	defer rows.Close()

	var ops []ResumableOp
	for rows.Next() {
		var (
			op                           ResumableOp
			schema, table, lastPauseTime sql.NullString
		)
		if err := rows.Scan(&schema, &table, &op.ObjectID, &op.IndexID, &op.Name, &op.StateDesc, &op.PercentComplete, &lastPauseTime); err != nil {
			return nil, fmt.Errorf("scan resumable operation: %w", err)
		}
		op.Schema, op.Table, op.LastPauseTime = schema.String, table.String, lastPauseTime.String
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

const pausedResumableSQL = `
SELECT COUNT(*) FROM sys.index_resumable_operations
WHERE object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table))
  AND name = @index AND state_desc = 'PAUSED';`

// PausedResumable reports whether a paused resumable rebuild exists for the named
// index on [schema].[table]. It tells an interrupted-but-recoverable operation
// (the session was killed / the connection dropped, leaving the work paused) from
// a genuine failure. It runs on the monitoring pool, so it survives the loss of
// the execution session.
func (c *Conn) PausedResumable(ctx context.Context, schema, table, index string) (bool, error) {
	return c.existsScalar(ctx, pausedResumableSQL,
		sql.Named("schema", schema), sql.Named("table", table), sql.Named("index", index))
}

// CurrentDatabase returns the name of the connected database (DB_NAME()), used as
// the default check_db target when the maintenance profile names no databases.
func (c *Conn) CurrentDatabase(ctx context.Context) (string, error) {
	var name string
	if err := c.pool.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&name); err != nil {
		return "", fmt.Errorf("read current database: %w", err)
	}
	return name, nil
}

// Session is one active user request, as seen by the monitoring connection.
type Session struct {
	SPID             int
	Status           string
	Command          string
	Login            string
	Host             string
	Program          string
	Database         string
	ElapsedMS        int64
	WaitType         string
	WaitMS           int64
	BlockingSPID     int
	OpenTransactions int
	ActiveQuery      string
	ParentQuery      string
}

// BlockedBy reports whether this session is blocked directly by ddlSPID (our DDL's
// session). A blocking_session_id of 0 means the session is not blocked by anyone, so
// it never counts — and a zero ddlSPID (our SPID unknown) never matches, so an idle
// session can never be mistaken for one our DDL is blocking.
func (s Session) BlockedBy(ddlSPID int) bool {
	return ddlSPID != 0 && s.BlockingSPID != 0 && s.BlockingSPID == ddlSPID
}

// SelfBlock describes our own DDL session being blocked by another session (the mirror
// of BlockedBy: here our DDL is the victim, waiting on a lock someone else holds). It is
// built from an ActiveSessions snapshot. Blocked is false when our session is not waiting
// on anyone. SPID/WaitType/WaitMS come from our own row; Login/Program/Query are the
// blocker's identity, populated only when the blocking session is present in the snapshot.
type SelfBlock struct {
	Blocked  bool
	SPID     int // the direct blocking session (blocking_session_id)
	WaitType string
	WaitMS   int64
	Login    string
	Program  string
	Command  string // the blocker's dm_exec_requests.command verb (for maintenance classification)
	Host     string
	Query    string
}

// FindSelfBlock reports whether the ddlSPID session is blocked in the snapshot and, if
// so, identifies its direct blocker. A zero ddlSPID (our SPID unknown) is never blocked.
// The blocker's identity is filled from the same snapshot when present; if the blocker
// is not in it (e.g. filtered out), Blocked/SPID/WaitType/WaitMS are still reported so
// the operator at least sees which session and wait is holding the operation up.
func FindSelfBlock(sessions []Session, ddlSPID int) SelfBlock {
	if ddlSPID == 0 {
		return SelfBlock{}
	}
	self, found := Session{}, false
	for _, s := range sessions {
		if s.SPID == ddlSPID {
			self, found = s, true
			break
		}
	}
	if !found || self.BlockingSPID == 0 {
		return SelfBlock{}
	}
	sb := SelfBlock{Blocked: true, SPID: self.BlockingSPID, WaitType: self.WaitType, WaitMS: self.WaitMS}
	for _, s := range sessions {
		if s.SPID != self.BlockingSPID {
			continue
		}
		sb.Login, sb.Program, sb.Host = s.Login, s.Program, s.Host
		sb.Command = s.Command
		if sb.Query = s.ActiveQuery; sb.Query == "" {
			sb.Query = s.ParentQuery // an idle-in-transaction blocker has no active statement
		}
		break
	}
	return sb
}

const activeSessionsSQL = `
SELECT
    r.session_id, r.status, r.command,
    s.login_name, s.host_name, s.program_name,
    DB_NAME(r.database_id), r.total_elapsed_time,
    r.wait_type, r.wait_time, r.blocking_session_id, r.open_transaction_count,
    SUBSTRING(qt.text, (r.statement_start_offset/2)+1,
        ((CASE r.statement_end_offset WHEN -1 THEN DATALENGTH(qt.text)
          ELSE r.statement_end_offset END - r.statement_start_offset)/2)+1),
    qt.text
FROM sys.dm_exec_requests r
INNER JOIN sys.dm_exec_sessions s ON r.session_id = s.session_id
OUTER APPLY sys.dm_exec_sql_text(r.sql_handle) qt
WHERE s.is_user_process = 1
  AND (s.status <> 'sleeping' OR r.open_transaction_count > 0)
ORDER BY r.cpu_time DESC
OPTION (RECOMPILE, MAXDOP 1);`

// ActiveSessions returns all active user requests. BlockingSPID is the DIRECT blocker
// (r.blocking_session_id), not a chain head; consumers attribute a block to our DDL with
// Session.BlockedBy (a direct comparison), so a transitively-blocked victim is not counted.
func (c *Conn) ActiveSessions(ctx context.Context) ([]Session, error) {
	rows, err := c.pool.QueryContext(ctx, activeSessionsSQL)
	if err != nil {
		return nil, fmt.Errorf("query active sessions: %w", err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var (
			sn                                 Session
			login, host, program, db           sql.NullString
			waitType, activeQuery, parentQuery sql.NullString
		)
		if err := rows.Scan(
			&sn.SPID, &sn.Status, &sn.Command,
			&login, &host, &program, &db, &sn.ElapsedMS,
			&waitType, &sn.WaitMS, &sn.BlockingSPID, &sn.OpenTransactions,
			&activeQuery, &parentQuery,
		); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sn.Login = login.String
		sn.Host = host.String
		sn.Program = program.String
		sn.Database = db.String
		sn.WaitType = waitType.String
		sn.ActiveQuery = activeQuery.String
		sn.ParentQuery = parentQuery.String
		sessions = append(sessions, sn)
	}
	return sessions, rows.Err()
}

// LockedObject is one user object a session holds a granted Sch-M lock on — for a
// shrink, the object it is relocating and holding other sessions up on. Schema/Table
// are empty when the object could not be name-resolved (dropped between lock and read);
// ObjectID always identifies it.
type LockedObject struct {
	ObjectID int64
	Schema   string
	Table    string
	Mode     string
}

const heldObjectLocksSQL = `
SELECT l.resource_associated_entity_id,
       OBJECT_SCHEMA_NAME(l.resource_associated_entity_id, l.resource_database_id),
       OBJECT_NAME(l.resource_associated_entity_id, l.resource_database_id),
       l.request_mode
FROM sys.dm_tran_locks l
WHERE l.request_session_id = @spid
  AND l.resource_type   = 'OBJECT'
  AND l.request_status  = 'GRANT'
  AND l.request_mode LIKE 'Sch-M%';`

// scanLockedObject maps one dm_tran_locks row to a LockedObject, keeping only the
// object_id when the name did not resolve.
func scanLockedObject(objectID int64, schema, table sql.NullString, mode string) LockedObject {
	return LockedObject{ObjectID: objectID, Schema: schema.String, Table: table.String, Mode: mode}
}

// HeldObjectLocks returns the user objects spid currently holds a granted Sch-M lock on.
// Best-effort: the caller treats an error as "nothing captured this snapshot".
func (c *Conn) HeldObjectLocks(ctx context.Context, spid int) ([]LockedObject, error) {
	rows, err := c.pool.QueryContext(ctx, heldObjectLocksSQL, sql.Named("spid", spid))
	if err != nil {
		return nil, fmt.Errorf("query held object locks: %w", err)
	}
	defer rows.Close()

	var out []LockedObject
	for rows.Next() {
		var (
			objectID      int64
			schema, table sql.NullString
			mode          string
		)
		if err := rows.Scan(&objectID, &schema, &table, &mode); err != nil {
			return nil, fmt.Errorf("scan held object lock: %w", err)
		}
		out = append(out, scanLockedObject(objectID, schema, table, mode))
	}
	return out, rows.Err()
}
