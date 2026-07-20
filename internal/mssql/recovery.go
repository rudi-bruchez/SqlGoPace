package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SessionIdentity is the signature of a live session used to recognize an
// orphaned DDL after a crash. The SPID alone is unreliable (reused), so it is
// correlated with login_time and the CONTEXT_INFO marker. Active reports whether
// the session has a request in flight: an existing-but-idle session is a dangling
// connection the crashed process left behind, not a running operation.
type SessionIdentity struct {
	Exists      bool
	Active      bool   // the session has a live request in sys.dm_exec_requests
	LoginTime   string // ISO 8601
	ContextInfo string // 0x... hex
}

const sessionIdentitySQL = `
SELECT CONVERT(varchar(30), s.login_time, 126),
       CONVERT(varchar(300), s.context_info, 1),
       CASE WHEN EXISTS (SELECT 1 FROM sys.dm_exec_requests r
                         WHERE r.session_id = s.session_id) THEN 1 ELSE 0 END
FROM sys.dm_exec_sessions s
WHERE s.session_id = @spid;`

// SessionIdentity reads the signature of the session with the given SPID.
func (c *Conn) SessionIdentity(ctx context.Context, spid int) (SessionIdentity, error) {
	var login, ctxInfo sql.NullString
	var active bool
	err := c.pool.QueryRowContext(ctx, sessionIdentitySQL, sql.Named("spid", spid)).Scan(&login, &ctxInfo, &active)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SessionIdentity{Exists: false}, nil
	case err != nil:
		return SessionIdentity{}, fmt.Errorf("read session identity: %w", err)
	default:
		return SessionIdentity{Exists: true, Active: active, LoginTime: login.String, ContextInfo: ctxInfo.String}, nil
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
