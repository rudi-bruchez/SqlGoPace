package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SessionIdentity is the signature of a live session used to recognize an
// orphaned DDL after a crash. The SPID alone is unreliable (reused), so it is
// correlated with login_time and the CONTEXT_INFO marker.
type SessionIdentity struct {
	Exists      bool
	LoginTime   string // ISO 8601
	ContextInfo string // 0x... hex
}

const sessionIdentitySQL = `
SELECT CONVERT(varchar(30), login_time, 126),
       CONVERT(varchar(300), context_info, 1)
FROM sys.dm_exec_sessions
WHERE session_id = @spid;`

// SessionIdentity reads the signature of the session with the given SPID.
func (c *Conn) SessionIdentity(ctx context.Context, spid int) (SessionIdentity, error) {
	var login, ctxInfo sql.NullString
	err := c.pool.QueryRowContext(ctx, sessionIdentitySQL, sql.Named("spid", spid)).Scan(&login, &ctxInfo)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SessionIdentity{Exists: false}, nil
	case err != nil:
		return SessionIdentity{}, fmt.Errorf("read session identity: %w", err)
	default:
		return SessionIdentity{Exists: true, LoginTime: login.String, ContextInfo: ctxInfo.String}, nil
	}
}

const loginTimeSQL = `SELECT CONVERT(varchar(30), login_time, 126) FROM sys.dm_exec_sessions WHERE session_id = @@SPID;`

// LoginTime reads the login_time of the execution session, recorded in the run
// state so recovery can disambiguate a reused SPID.
func (c *Conn) LoginTime(ctx context.Context) (string, error) {
	var login string
	if err := c.exec.QueryRowContext(ctx, loginTimeSQL).Scan(&login); err != nil {
		return "", fmt.Errorf("read login_time: %w", err)
	}
	return login, nil
}
