package mssql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Aborting a statement (the cancel reaction) can leave the pinned execution
// connection poisoned: the next use fails with driver.ErrBadConn, and once
// database/sql has seen that, the *sql.Conn is done and every later statement
// returns sql.ErrConnDone. These tests drive that sequence over a fake driver, so
// they need no server.

// fakeConnector hands out fakeConns and remembers them, so a test can assert how
// many sessions were opened and what ran on each.
type fakeConnector struct {
	mu       sync.Mutex
	conns    []*fakeConn
	nextSPID int

	// What the identity probe reports for the session the repair asks about, and what
	// KILL does to it. Shared by every connection, because the probe runs on the pool.
	probeErr     error
	blockConnect chan struct{} // when non-nil, Connect blocks on it: the server is down
	probeDelay   time.Duration // how long the identity probe takes to answer
	orphan       *fakeSession
	killed       []int
	killClears   bool // a KILL makes the orphan inactive
}

// fakeSession is one row of sys.dm_exec_sessions as the identity probe sees it.
type fakeSession struct {
	login  string
	active bool
}

// newFakePool returns a pool backed by the fake driver, plus the connector that
// records the sessions it opens.
func newFakePool(t *testing.T) (*sql.DB, *fakeConnector) {
	t.Helper()
	fc := &fakeConnector{nextSPID: 100}
	pool := sql.OpenDB(fc)
	t.Cleanup(func() { _ = pool.Close() })
	return pool, fc
}

func (f *fakeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	f.mu.Lock()
	block := f.blockConnect
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSPID++
	c := &fakeConn{spid: f.nextSPID, owner: f}
	f.conns = append(f.conns, c)
	return c, nil
}

func (f *fakeConnector) Driver() driver.Driver { return nil }

// session returns the i-th session the pool opened.
func (f *fakeConnector) session(t *testing.T, i int) *fakeConn {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.conns) {
		t.Fatalf("session %d was never opened (%d opened)", i, len(f.conns))
	}
	return f.conns[i]
}

func (f *fakeConnector) opened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

// ranAnywhere reports whether any session the pool opened ran a statement containing
// substr. The identity probe opens pooled connections of its own, so counting sessions
// does not answer "did the next operation execute" — this does.
func (f *fakeConnector) ranAnywhere(substr string) bool {
	f.mu.Lock()
	conns := append([]*fakeConn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		if c.sawStatement(substr) {
			return true
		}
	}
	return false
}

// killsIssued returns the session ids KILLed through the monitoring pool.
func (f *fakeConnector) killsIssued() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.killed...)
}

// identity answers the repair's probe: (rows, error). A nil orphan means no such session.
func (f *fakeConnector) identity() (*fakeSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orphan, f.probeErr
}

func (f *fakeConnector) probeDelayFor() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probeDelay
}

func (f *fakeConnector) recordKill(spid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, spid)
	if f.killClears && f.orphan != nil {
		f.orphan.active = false
	}
}

// fakeConn is one server session. failWith arms the next statement to fail, and
// optionally to leave the session unusable the way an aborted statement does.
type fakeConn struct {
	owner  *fakeConnector
	mu     sync.Mutex
	spid   int
	ran    []string
	fail   error
	poison bool
	bad    bool
}

func (c *fakeConn) failWith(err error, poison bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail, c.poison = err, poison
}

// statements returns what ran on this session.
func (c *fakeConn) statements() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ran...)
}

// ran reports whether a statement containing substr ran on this session.
func (c *fakeConn) sawStatement(substr string) bool {
	for _, s := range c.statements() {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// next records the statement and returns the error it should fail with, if any.
func (c *fakeConn) next(query string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bad {
		return driver.ErrBadConn
	}
	c.ran = append(c.ran, query)
	if c.fail == nil {
		return nil
	}
	err := c.fail
	c.fail = nil
	c.bad = c.poison
	return err
}

func (c *fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if err := c.next(query); err != nil {
		return nil, err
	}
	if spid, ok := strings.CutPrefix(query, "KILL "); ok {
		n, err := strconv.Atoi(strings.TrimSpace(spid))
		if err != nil {
			return nil, fmt.Errorf("fake driver: malformed KILL %q", query)
		}
		c.owner.recordKill(n)
	}
	return driver.RowsAffected(1), nil
}

func (c *fakeConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := c.next(query); err != nil {
		return nil, err
	}
	// The repair's identity probe, the only query here with more than one column.
	if strings.Contains(query, "context_info") {
		// A real driver notices a dead context; this one must too, or a test cannot tell
		// a slow server apart from an expired budget.
		if d := c.owner.probeDelayFor(); d > 0 {
			time.Sleep(d)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sess, err := c.owner.identity()
		if err != nil {
			return nil, err
		}
		if sess == nil {
			return &identityRows{}, nil
		}
		return &identityRows{sess: sess}, nil
	}
	c.mu.Lock()
	spid := c.spid
	c.mu.Unlock()
	return &intRows{val: int64(spid)}, nil
}

func (c *fakeConn) Ping(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bad {
		return driver.ErrBadConn
	}
	return nil
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("the fake driver only supports the context statement interfaces")
}
func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("no transactions in the fake driver")
}
func (c *fakeConn) Close() error { return nil }

// identityRows is what sessionIdentitySQL returns: login_time, context_info, active.
// A zero value is the no-such-session case.
type identityRows struct {
	sess *fakeSession
	done bool
}

func (r *identityRows) Columns() []string { return []string{"login_time", "context_info", "active"} }
func (r *identityRows) Close() error      { return nil }
func (r *identityRows) Next(dest []driver.Value) error {
	if r.done || r.sess == nil {
		return io.EOF
	}
	r.done = true
	dest[0], dest[1], dest[2] = r.sess.login, "0x00", r.sess.active
	return nil
}

// intRows is the single-column, single-row result SELECT @@SPID returns.
type intRows struct {
	val  int64
	done bool
}

func (r *intRows) Columns() []string { return []string{"spid"} }
func (r *intRows) Close() error      { return nil }
func (r *intRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.val
	return nil
}

// newFakeConn pins an execution connection on the fake pool, the way open() does.
func newFakeConn(t *testing.T, pool *sql.DB) *Conn {
	t.Helper()
	exec, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin execution connection: %v", err)
	}
	c := &Conn{pool: pool, exec: exec, appName: AppNamePrefix, loginTime: pinnedLogin}
	if err := c.harden(context.Background()); err != nil {
		t.Fatalf("harden: %v", err)
	}
	if err := exec.QueryRowContext(context.Background(), "SELECT @@SPID").Scan(&c.spid); err != nil {
		t.Fatalf("read @@SPID: %v", err)
	}
	return c
}

// One canceled rebuild took the twelve operations queued behind it down with it,
// each failing in milliseconds on the connection the cancel had poisoned. A
// statement that leaves the pinned connection unusable must re-pin it, so the next
// operation runs.
func TestExecDDLRepairsAPoisonedExecutionConnection(t *testing.T) {
	tests := []struct {
		name   string
		fail   error
		poison bool
	}{
		{"an aborted statement leaves the session unusable", context.Canceled, true},
		{"the driver reports the connection is bad", driver.ErrBadConn, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, connector := newFakePool(t)
			c := newFakeConn(t, pool)
			connector.session(t, 0).failWith(tt.fail, tt.poison)

			if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_A] ON [dbo].[A] REBUILD;"); err == nil {
				t.Fatal("ExecDDL() = nil, want the failing statement's error")
			}

			if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_B] ON [dbo].[B] REBUILD;"); err != nil {
				t.Fatalf("ExecDDL() on the next operation = %v, want nil", err)
			}
			if !connector.session(t, 1).sawStatement("[PK_B]") {
				t.Errorf("the next operation did not run on the re-pinned session; it ran %q", connector.session(t, 1).statements())
			}
		})
	}
}

// A re-pinned session is a different server session: it starts with the driver's
// defaults and without the run marker, so both have to be applied again. A stale
// SPID would silently disable every blocking reaction, since block attribution
// keys off it.
func TestRepairedSessionIsHardenedAndCarriesTheRunMarker(t *testing.T) {
	pool, connector := newFakePool(t)
	c := newFakeConn(t, pool)
	marker := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	if err := c.SetMarker(context.Background(), marker); err != nil {
		t.Fatalf("SetMarker: %v", err)
	}
	connector.session(t, 0).failWith(context.Canceled, true)

	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_A] ON [dbo].[A] REBUILD;"); err == nil {
		t.Fatal("ExecDDL() = nil, want the failing statement's error")
	}
	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_B] ON [dbo].[B] REBUILD;"); err != nil {
		t.Fatalf("ExecDDL() on the next operation = %v, want nil", err)
	}

	repaired := connector.session(t, 1)
	if !repaired.sawStatement("SET XACT_ABORT ON") {
		t.Errorf("the re-pinned session was not hardened; it ran %q", repaired.statements())
	}
	if !repaired.sawStatement("SET CONTEXT_INFO " + ContextInfoLiteral(marker)) {
		t.Errorf("the re-pinned session did not carry the run marker; it ran %q", repaired.statements())
	}
	if got, want := c.SPID(), repaired.spid; got != want {
		t.Errorf("SPID() = %d, want the re-pinned session's %d", got, want)
	}
}

// A statement SQL Server rejects leaves the session perfectly usable. Re-pinning
// there would churn the session (and its SPID) on every ordinary DDL error.
func TestExecDDLKeepsTheSessionAfterAnOrdinaryStatementError(t *testing.T) {
	pool, connector := newFakePool(t)
	c := newFakeConn(t, pool)
	before := c.SPID()
	connector.session(t, 0).failWith(errors.New("mssql: Msg 1205, deadlock victim"), false)

	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_A] ON [dbo].[A] REBUILD;"); err == nil {
		t.Fatal("ExecDDL() = nil, want the statement's error")
	}

	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_B] ON [dbo].[B] REBUILD;"); err != nil {
		t.Fatalf("ExecDDL() on the next operation = %v, want nil", err)
	}

	if got := connector.opened(); got != 1 {
		t.Errorf("opened %d session(s), want 1: an ordinary statement error must not re-pin", got)
	}
	if got := c.SPID(); got != before {
		t.Errorf("SPID() = %d, want the unchanged %d", got, before)
	}
}

// The batched-DML driver runs its statements through ExecRows, and is canceled by
// the same reaction hierarchy.
func TestExecRowsRepairsAPoisonedExecutionConnection(t *testing.T) {
	pool, connector := newFakePool(t)
	c := newFakeConn(t, pool)
	connector.session(t, 0).failWith(context.Canceled, true)

	if _, err := c.ExecRows(context.Background(), "DELETE TOP (1000) FROM [dbo].[A];"); err == nil {
		t.Fatal("ExecRows() = nil, want the failing statement's error")
	}

	n, err := c.ExecRows(context.Background(), "DELETE TOP (1000) FROM [dbo].[A];")
	if err != nil {
		t.Fatalf("ExecRows() on the next batch = %v, want nil", err)
	}
	if n != 1 {
		t.Errorf("ExecRows() = %d rows, want 1", n)
	}
}

// pinnedLogin is the login_time the test's pinned execution session was opened with.
// The repair uses it to tell our own abandoned session apart from a reused session id.
const pinnedLogin = "2026-09-03T02:00:00.000"

// shortOrphanWait makes the bounded wait for an abandoned session finish in test time.
func shortOrphanWait(t *testing.T) {
	t.Helper()
	timeout, interval := orphanStopTimeout, orphanPollInterval
	orphanStopTimeout, orphanPollInterval = 50*time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { orphanStopTimeout, orphanPollInterval = timeout, interval })
}

// The driver abandons a connection when SQL Server does not confirm the attention
// within ~10s (go-mssqldb token.go, cancelDrainTimeout), which is well inside
// kill_grace — so the runner's fallback KILL never fires and the statement may still
// be running server-side. Re-pinning and carrying on there runs the next operation
// beside our own orphan, blocked on its locks and invisible to our monitoring.
func TestRepairStopsTheOrphanedSessionBeforeAdoptingANewOne(t *testing.T) {
	shortOrphanWait(t)
	pool, connector := newFakePool(t)
	c := newFakeConn(t, pool)
	orphanSPID := c.SPID()
	connector.orphan = &fakeSession{login: pinnedLogin, active: true}
	connector.killClears = true
	connector.session(t, 0).failWith(context.Canceled, true)

	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_A] ON [dbo].[A] REBUILD;"); err == nil {
		t.Fatal("ExecDDL() = nil, want the failing statement's error")
	}
	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_B] ON [dbo].[B] REBUILD;"); err != nil {
		t.Fatalf("ExecDDL() on the next operation = %v, want nil", err)
	}

	if got, want := connector.killsIssued(), []int{orphanSPID}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("KILLs issued = %v, want %v — the abandoned session must be stopped before the next statement", got, want)
	}
}

// If the abandoned session will not stop, starting the next operation runs it beside
// a request still holding its locks. Failing loudly is the safe outcome: the manifest
// stops, and every later attempt says the same thing.
func TestRepairRefusesToAdoptWhileTheOrphanStillRuns(t *testing.T) {
	shortOrphanWait(t)
	pool, connector := newFakePool(t)
	c := newFakeConn(t, pool)
	orphanSPID := c.SPID()
	connector.orphan = &fakeSession{login: pinnedLogin, active: true}
	connector.killClears = false // the KILL lands but the rollback keeps the request alive
	connector.session(t, 0).failWith(context.Canceled, true)

	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_A] ON [dbo].[A] REBUILD;"); err == nil {
		t.Fatal("ExecDDL() = nil, want the failing statement's error")
	}

	err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_B] ON [dbo].[B] REBUILD;")
	if err == nil {
		t.Fatal("ExecDDL() = nil, want a refusal while the abandoned session is still running")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(orphanSPID)) {
		t.Errorf("error = %q, want it to name the session still running (%d)", err, orphanSPID)
	}
	if connector.ranAnywhere("[PK_B]") {
		t.Error("the next operation executed anyway; it must not run while the orphan holds its locks")
	}
	if got := c.SPID(); got != orphanSPID {
		t.Errorf("SPID() = %d, want the unchanged %d — nothing was adopted", got, orphanSPID)
	}
}

// SQL Server reuses session ids. Once our connection is gone the id can belong to
// somebody else, and a KILL issued on it would terminate their work.
func TestRepairDoesNotKillAReassignedSessionID(t *testing.T) {
	shortOrphanWait(t)
	pool, connector := newFakePool(t)
	c := newFakeConn(t, pool)
	connector.orphan = &fakeSession{login: "2026-09-03T04:30:00.000", active: true} // a different login_time: not ours
	connector.session(t, 0).failWith(context.Canceled, true)

	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_A] ON [dbo].[A] REBUILD;"); err == nil {
		t.Fatal("ExecDDL() = nil, want the failing statement's error")
	}
	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_B] ON [dbo].[B] REBUILD;"); err != nil {
		t.Fatalf("ExecDDL() on the next operation = %v, want nil", err)
	}

	if got := connector.killsIssued(); len(got) != 0 {
		t.Errorf("KILLs issued = %v, want none: session id reassigned, that session is not ours", got)
	}
}

// A probe that cannot be answered says nothing about an orphan. Refusing there would
// turn a network blip into a run that can never repair itself; re-pinning is itself
// the liveness test, and fails on its own if the server is really gone.
func TestRepairProceedsWhenTheOrphanCannotBeProbed(t *testing.T) {
	shortOrphanWait(t)
	pool, connector := newFakePool(t)
	c := newFakeConn(t, pool)
	connector.probeErr = errors.New("network error reading session identity")
	connector.session(t, 0).failWith(context.Canceled, true)

	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_A] ON [dbo].[A] REBUILD;"); err == nil {
		t.Fatal("ExecDDL() = nil, want the failing statement's error")
	}
	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_B] ON [dbo].[B] REBUILD;"); err != nil {
		t.Fatalf("ExecDDL() on the next operation = %v, want nil", err)
	}
}

// The wait for an abandoned session and the budget for pinning a new one are separate
// things. Sharing one context makes the shorter of the two silently win, and because an
// unanswerable probe means "cannot tell" and lets the repair continue, the run would go
// right back to issuing DDL beside a live request — the failure this whole path exists to
// prevent, reintroduced by a context.
func TestOrphanWaitIsNotCutShortByTheRepairBudget(t *testing.T) {
	timeout, interval := orphanStopTimeout, orphanPollInterval
	orphanStopTimeout, orphanPollInterval = time.Second, time.Millisecond
	t.Cleanup(func() { orphanStopTimeout, orphanPollInterval = timeout, interval })

	pool, connector := newFakePool(t)
	c := newFakeConn(t, pool)
	c.repairBudget = 5 * time.Millisecond
	orphanSPID := c.SPID()
	connector.orphan = &fakeSession{login: pinnedLogin, active: true}
	connector.killClears = false
	// The probe answers only after the repair budget would have expired, so it is the
	// probe — deterministically, not by a race with the poll — that meets the dead
	// context. Sharing that context makes a refusal look like "cannot tell".
	connector.probeDelay = 20 * time.Millisecond
	connector.session(t, 0).failWith(context.Canceled, true)

	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_A] ON [dbo].[A] REBUILD;"); err == nil {
		t.Fatal("ExecDDL() = nil, want the failing statement's error")
	}

	err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_B] ON [dbo].[B] REBUILD;")
	if err == nil {
		t.Fatal("ExecDDL() = nil, want a refusal: the orphan is still running")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(orphanSPID)) {
		t.Errorf("error = %q, want it to name the session still running (%d)", err, orphanSPID)
	}
	if connector.ranAnywhere("[PK_B]") {
		t.Error("the next operation executed while the orphan was still running")
	}
}

// A failover is exactly when the repair matters and exactly when it takes time. The
// operator already configures how long to wait for the server to come back
// (monitoring.reconnect_timeout_minutes); a hardcoded budget shorter than theirs fails
// the repair while the instance is still coming up, and charges each failed attempt to
// a different operation.
func TestRepairGivesUpAfterTheConfiguredReconnectTimeout(t *testing.T) {
	pool, connector := newFakePool(t)
	c := newFakeConn(t, pool)
	c.loginTime = "" // no orphan check: this test is about the re-pin budget alone
	c.repairBudget = 30 * time.Millisecond
	connector.blockConnect = make(chan struct{}) // the server is down; dialing hangs
	connector.session(t, 0).failWith(context.Canceled, true)

	if err := c.ExecDDL(context.Background(), "ALTER INDEX [PK_A] ON [dbo].[A] REBUILD;"); err == nil {
		t.Fatal("ExecDDL() = nil, want the failing statement's error")
	}

	done := make(chan error, 1)
	go func() { done <- c.ExecDDL(context.Background(), "ALTER INDEX [PK_B] ON [dbo].[B] REBUILD;") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ExecDDL() = nil, want a re-pin failure: the server never came back")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the repair was still waiting after 2s; it is using a hardcoded budget, not the configured one")
	}
}
