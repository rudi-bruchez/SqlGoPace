package main

import (
	"context"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

type fakeLogProbe struct {
	name  string
	calls int
}

func (p *fakeLogProbe) LogSpace(context.Context) (mssql.LogSpace, error) {
	p.calls++
	return mssql.LogSpace{TotalBytes: 1}, nil
}

func (p *fakeLogProbe) LogReuseWait(context.Context) (string, error) {
	p.calls++
	return p.name, nil
}

type fakeSessionProbe struct{ calls int }

func (p *fakeSessionProbe) ActiveSessions(context.Context) ([]mssql.Session, error) {
	p.calls++
	return nil, nil
}

// The tempdb shrink runs its DBCC on a connection whose database context is tempdb, but
// its sampler was built with the primary connection as the probe, justified by a comment
// saying DMV reads are instance-wide. That is true of sys.dm_exec_requests and
// sys.dm_exec_sessions, which is why session detection worked. It is false of both log
// reads: sys.dm_db_log_space_usage reports the connected database, and the reuse-wait read
// filters on DB_ID(). So a tempdb shrink watched the user database's log — reacting to
// pressure that had nothing to do with it, and blind to the pressure it was causing.
func TestTempdbProbeReadsTheLogInTempdb(t *testing.T) {
	tempdb := &fakeLogProbe{name: "tempdb"}
	p := tempdbProbe{db: tempdb, sessions: &fakeSessionProbe{}}

	if _, err := p.LogSpace(context.Background()); err != nil {
		t.Fatalf("LogSpace() error = %v", err)
	}
	got, err := p.LogReuseWait(context.Background())
	if err != nil {
		t.Fatalf("LogReuseWait() error = %v", err)
	}
	if got != "tempdb" {
		t.Errorf("LogReuseWait() = %q, want the read to land in tempdb", got)
	}
	if tempdb.calls != 2 {
		t.Errorf("tempdb connection answered %d log read(s), want 2", tempdb.calls)
	}
}

// Sessions stay on the other connection on purpose: the read is instance-wide, so either
// would answer correctly, and keeping it off the connection running the DBCC keeps the
// monitoring load away from the statement being monitored.
func TestTempdbProbeReadsSessionsOffTheExecutingConnection(t *testing.T) {
	tempdb := &fakeLogProbe{name: "tempdb"}
	sessions := &fakeSessionProbe{}

	p := tempdbProbe{db: tempdb, sessions: sessions}
	if _, err := p.ActiveSessions(context.Background()); err != nil {
		t.Fatalf("ActiveSessions() error = %v", err)
	}

	if sessions.calls != 1 {
		t.Errorf("session probe called %d time(s), want 1", sessions.calls)
	}
	if tempdb.calls != 0 {
		t.Errorf("the executing connection answered %d session read(s), want 0", tempdb.calls)
	}
}
