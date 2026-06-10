package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// populated for REBUILD/ALTER and during a KILL rollback.
type Progress struct {
	PercentComplete       float64
	EstimatedCompletionMS int64
	ElapsedMS             int64
}

const progressSQL = `
SELECT percent_complete, estimated_completion_time, total_elapsed_time
FROM sys.dm_exec_requests
WHERE session_id = @spid;`

// Progress reads completion estimates for the given session. found is false when
// the session has no active request (e.g. the DDL has finished).
func (c *Conn) Progress(ctx context.Context, spid int) (p Progress, found bool, err error) {
	row := c.pool.QueryRowContext(ctx, progressSQL, sql.Named("spid", spid))
	switch err := row.Scan(&p.PercentComplete, &p.EstimatedCompletionMS, &p.ElapsedMS); {
	case errors.Is(err, sql.ErrNoRows):
		return Progress{}, false, nil
	case err != nil:
		return Progress{}, false, fmt.Errorf("read progress: %w", err)
	default:
		return p, true, nil
	}
}

// ResumableOp is an in-progress or paused resumable index operation, surviving
// tool and server restarts; consulted on startup to adopt orphaned operations.
type ResumableOp struct {
	ObjectID        int64
	IndexID         int
	Name            string
	StateDesc       string // "RUNNING" | "PAUSED"
	PercentComplete float64
}

const resumableOpsSQL = `
SELECT object_id, index_id, name, state_desc, percent_complete
FROM sys.index_resumable_operations;`

// ResumableOps lists resumable index operations known to the engine.
func (c *Conn) ResumableOps(ctx context.Context) ([]ResumableOp, error) {
	rows, err := c.pool.QueryContext(ctx, resumableOpsSQL)
	if err != nil {
		return nil, fmt.Errorf("query resumable operations: %w", err)
	}
	defer rows.Close()

	var ops []ResumableOp
	for rows.Next() {
		var op ResumableOp
		if err := rows.Scan(&op.ObjectID, &op.IndexID, &op.Name, &op.StateDesc, &op.PercentComplete); err != nil {
			return nil, fmt.Errorf("scan resumable operation: %w", err)
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
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

// ActiveSessions returns all active user requests. The orchestrator filters and
// walks the blocking chain (blocking_session_id) to find head blockers.
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
