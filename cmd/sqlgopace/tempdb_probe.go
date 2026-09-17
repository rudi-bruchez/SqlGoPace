package main

import (
	"context"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// logProbe is the database-scoped half of what a sampler reads: both statements report the
// connection's *current* database, so which connection asks decides which database answers.
type logProbe interface {
	LogSpace(ctx context.Context) (mssql.LogSpace, error)
	LogReuseWait(ctx context.Context) (string, error)
}

// sessionProbe is the instance-wide half: sys.dm_exec_requests joined to
// sys.dm_exec_sessions sees every session on the instance, whatever database the
// connection sits in.
type sessionProbe interface {
	ActiveSessions(ctx context.Context) ([]mssql.Session, error)
}

// tempdbProbe routes each read of the tempdb shrink's sampler to the connection that can
// answer it correctly. The distinction is not decorative: sys.dm_db_log_space_usage reports
// the connected database and the reuse-wait read filters on DB_ID(), so a tempdb shrink
// probed through the primary connection watched the *user* database's log — reacting to
// pressure that had nothing to do with the operation, and blind to the pressure it was
// itself producing. The in-code justification for that wiring ("DMV reads are
// instance-wide, so probing stays on conn") was right about the session read it was written
// for and was never revisited when the log reads joined the same sampler.
//
// Sessions deliberately stay on the other connection: the read is instance-wide, so either
// would answer correctly, and keeping it off the connection running the DBCC keeps the
// monitoring load away from the statement being monitored.
type tempdbProbe struct {
	db       logProbe     // the connection whose database context is tempdb
	sessions sessionProbe // any connection on the instance, other than the executing one
}

func (p tempdbProbe) LogSpace(ctx context.Context) (mssql.LogSpace, error) {
	return p.db.LogSpace(ctx)
}

func (p tempdbProbe) LogReuseWait(ctx context.Context) (string, error) {
	return p.db.LogReuseWait(ctx)
}

func (p tempdbProbe) ActiveSessions(ctx context.Context) ([]mssql.Session, error) {
	return p.sessions.ActiveSessions(ctx)
}
