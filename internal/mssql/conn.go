package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	mssqldb "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// Conn holds the two distinct connections SqlGoPace needs: a pinned, dedicated
// execution connection (so @@SPID is stable across the whole DDL) and a separate
// monitoring pool that is never blocked by the DDL it observes.
//
// The pinned connection survives one DDL statement, not necessarily the whole run:
// aborting a statement can leave it unusable, so repairIfBroken replaces it. mu
// guards everything that changes when it does — the connection itself, its session
// id, and the run marker to restamp on a new session — because the monitoring
// goroutine reads SPID while the execution goroutine runs statements.
type Conn struct {
	pool         *sql.DB // monitoring connections
	appName      string  // effective application-name base, version suffix excluded
	repairBudget time.Duration

	mu        sync.Mutex
	exec      *sql.Conn // pinned execution connection
	spid      int
	loginTime string   // login_time of the pinned session; tells it from a reused spid
	marker    [16]byte // last run marker set on the execution session
	hasMarker bool
	suspect   bool // the last statement failed: check the connection before the next one
}

// Open connects with the given DSN (ADO or URL form), stamps the application
// version into the connection's application name (visible server-side as
// program_name), pins a dedicated execution connection, hardens its session, and
// records its SPID. The driver applies no query timeout: long DDL is bounded by
// monitoring and context cancellation, never a fixed timer.
func Open(ctx context.Context, dsn, version string, opts ...Option) (*Conn, error) {
	return open(ctx, dsn, "", version, opts...)
}

// OpenDatabase connects like Open but targets a specific database, reusing the
// server and credentials from the base DSN with the catalog overridden. It is how
// multi-database maintenance reaches each database in its own context, rather than
// issuing USE on a pooled connection (the pool trap, SPECS §3 / spec §17.2).
func OpenDatabase(ctx context.Context, dsn, database, version string, opts ...Option) (*Conn, error) {
	return open(ctx, dsn, database, version, opts...)
}

// settings is what the options build up: the driver's own configuration, plus the
// connection-level budgets that are not the driver's business.
type settings struct {
	cfg    msdsn.Config
	repair time.Duration
}

// Option adjusts a connection before it is opened.
type Option func(*settings)

// WithLoginTimeout bounds how long a connection attempt may take. A value of zero or
// less leaves the driver's own default in place. This is a connection timeout and never
// a query timeout: SqlGoPace puts no timer on an executing statement.
func WithLoginTimeout(d time.Duration) Option {
	return func(s *settings) {
		if d > 0 {
			s.cfg.DialTimeout = d
		}
	}
}

// WithReconnectTimeout bounds re-pinning the execution connection after a statement
// left it unusable — how long to wait for the server to come back, which is what
// monitoring.reconnect_timeout_minutes means. A value of zero or less keeps
// defaultRepairTimeout. It does not bound the wait for an abandoned session to stop:
// that is a different question (has *our* statement finished?) with its own budget.
func WithReconnectTimeout(d time.Duration) Option {
	return func(s *settings) {
		if d > 0 {
			s.repair = d
		}
	}
}

// open is the shared connection setup. A non-empty database overrides the DSN's
// catalog so the pinned execution connection lands in that database.
func open(ctx context.Context, dsn, database, version string, opts ...Option) (*Conn, error) {
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	set := settings{cfg: cfg, repair: defaultRepairTimeout}
	for _, o := range opts {
		o(&set)
	}
	cfg = set.cfg
	// The base is what program_name is built from, and therefore what self-exclusion
	// must key off — not the AppNamePrefix constant, which is only the fallback when
	// the DSN sets no application name of its own.
	base := appNameBase(cfg.AppName)
	cfg.AppName = appNameWithVersion(base, version)
	if database != "" {
		cfg.Database = database
	}

	pool := sql.OpenDB(mssqldb.NewConnectorConfig(cfg))
	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping server: %w", err)
	}

	exec, err := pool.Conn(ctx)
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("pin execution connection: %w", err)
	}

	c := &Conn{pool: pool, exec: exec, appName: base, repairBudget: set.repair}
	if err := c.harden(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.execSession().QueryRowContext(ctx, "SELECT @@SPID").Scan(&c.spid); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("read @@SPID: %w", err)
	}
	// Block attribution keys off this SPID everywhere (Session.BlockedBy). A zero here would
	// make BlockedBy always false — silently disabling every blocking reaction — so refuse to
	// run rather than monitor blind. @@SPID is never 0 for a live session; this is a guard.
	if c.spid <= 0 {
		_ = c.Close()
		return nil, fmt.Errorf("read @@SPID: got non-positive session id %d", c.spid)
	}
	// Recorded so a later repair can tell this session from another connection that has
	// since been given the same id. Best-effort: without it the repair declines to KILL,
	// which is the safe direction.
	c.loginTime, _ = c.LoginTime(ctx)
	return c, nil
}

// harden applies the safety session settings to the execution connection:
// XACT_ABORT ON, DEADLOCK_PRIORITY LOW (so the DDL is the deadlock victim rather than
// a user query), and IMPLICIT_TRANSACTIONS OFF (so a REORGANIZE releases its locks
// incrementally instead of holding them until an implicit transaction commits —
// defensive; go-mssqldb already defaults it off, but a server-level `user options`
// default could turn it on).
func (c *Conn) harden(ctx context.Context) error { return hardenSession(ctx, c.execSession()) }

// hardenSession applies those settings to one connection. A re-pinned execution
// connection is a fresh server session, so it needs them again.
func hardenSession(ctx context.Context, conn *sql.Conn) error {
	const stmt = "SET XACT_ABORT ON; SET DEADLOCK_PRIORITY LOW; SET IMPLICIT_TRANSACTIONS OFF;"
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("harden execution session: %w", err)
	}
	return nil
}

// AppNamePrefix is the application name SqlGoPace connects with when the DSN sets
// none of its own, before the version suffix appNameWithVersion appends
// ("SqlGoPace/0.13.0"). It is only the fallback: an operator DSN carrying
// `app name=...` overrides it, which is why self-exclusion keys off
// (*Conn).AppNamePrefix rather than this constant.
const AppNamePrefix = "SqlGoPace"

// appNameBase returns the application name program_name is built from: whatever the
// DSN configured, or AppNamePrefix when it configured nothing. The driver's own
// default ("go-mssqldb") counts as nothing.
func appNameBase(appName string) string {
	base := strings.TrimSpace(appName)
	if base == "" || base == "go-mssqldb" {
		return AppNamePrefix
	}
	return base
}

// appNameWithVersion appends the application version to the configured application
// name so the running build is visible server-side (sys.dm_exec_sessions
// program_name), e.g. "SqlGoPace/0.1.0". A missing or default-driver app name
// falls back to "SqlGoPace".
func appNameWithVersion(appName, version string) string {
	base := appNameBase(appName)
	v := strings.TrimSpace(version)
	if v == "" {
		return base
	}
	return base + "/" + v
}

// AppNamePrefix returns the application-name base this connection presents
// server-side, without the version suffix — "SqlGoPace" by default, or whatever the
// DSN's `app name` set. The victim killer matches program_name against it by prefix
// so it never terminates another SqlGoPace session, including one running a different
// build (whose program_name differs only in the suffix). Reading it from the live
// connection rather than from the AppNamePrefix constant is what makes that guarantee
// hold for an operator who renamed the application in the DSN.
func (c *Conn) AppNamePrefix() string { return c.appName }

// SPID returns the session id of the pinned execution connection. Read it on every
// use rather than caching it: repairIfBroken re-pins onto a new server session, and
// a caller holding the old id would attribute no blocking to us at all.
func (c *Conn) SPID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spid
}

// execSession returns the currently pinned execution connection.
func (c *Conn) execSession() *sql.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exec
}

// Detect reads the target server's version, edition, recovery model, and ADR
// state over the execution connection.
func (c *Conn) Detect(ctx context.Context) (ServerInfo, error) {
	return DetectServer(ctx, c.execSession())
}

// SetMarker writes a run marker into CONTEXT_INFO on the execution session so an
// orphaned DDL can be correlated to its run after a crash. The marker is remembered
// so a re-pinned session can be stamped with it again; without that, a crash after a
// repair leaves an orphan no run can claim.
func (c *Conn) SetMarker(ctx context.Context, marker [16]byte) error {
	if err := setSessionMarker(ctx, c.execSession(), marker); err != nil {
		return err
	}
	c.mu.Lock()
	c.marker, c.hasMarker = marker, true
	c.mu.Unlock()
	return nil
}

func setSessionMarker(ctx context.Context, conn *sql.Conn, marker [16]byte) error {
	if _, err := conn.ExecContext(ctx, "SET CONTEXT_INFO "+ContextInfoLiteral(marker)); err != nil {
		return fmt.Errorf("set context_info: %w", err)
	}
	return nil
}

// defaultRepairTimeout bounds re-pinning the execution connection when nothing is
// configured. It is a connection timeout, not a query timeout: nothing is executing
// while it runs. WithReconnectTimeout replaces it with the operator's value.
const defaultRepairTimeout = 30 * time.Second

// How long a repair waits for an abandoned session to stop, and how often it looks.
// Two minutes is long enough for a KILL of a sizeable rollback to take effect and
// short enough that a run does not hang on one; past it the run stops rather than
// issuing DDL beside a request that still holds its locks. They are vars only so
// tests need not wait out the real budget.
var (
	orphanStopTimeout  = 2 * time.Minute
	orphanPollInterval = 2 * time.Second
)

// execStatement runs one statement on the pinned execution connection, repairing
// that connection first when the statement before it failed.
//
// The check happens here, before the next statement, rather than on the failing
// statement's own error path: the runner is still waiting for that statement to stop
// and issues a fallback KILL if it takes longer than kill_grace, so reconnecting
// there would race that KILL onto the session it had just re-pinned.
func (c *Conn) execStatement(ctx context.Context, statement string) (sql.Result, error) {
	if err := c.repairIfBroken(); err != nil {
		return nil, err
	}
	res, err := c.execSession().ExecContext(ctx, statement)
	c.mu.Lock()
	c.suspect = err != nil
	c.mu.Unlock()
	return res, err
}

// repairIfBroken re-pins the execution connection when the statement before it left
// it unusable. Aborting a statement — which is how the cancel reaction and a resumable
// pause both stop the DDL — sends an attention, and an attention the driver cannot
// complete leaves the connection poisoned: every later statement on it dies instantly
// with "driver: bad connection", then "sql: connection is already closed" once
// database/sql has retired it. That is how one canceled rebuild took the twelve
// operations queued behind it down with it (0.33.0).
//
// It costs nothing after a statement that succeeded, which is every statement in a run
// that is going well. After one that failed it costs a ping, because the error that
// surfaces to us is usually context.Canceled — our own cancellation, which says nothing
// about the connection underneath it.
//
// The failed statement is never re-run here: the server may still be working on it, and
// the retry policy belongs to the caller (max_retry_attempts). The new connection is
// published only once it is hardened, identified and re-stamped, so a half-built session
// never executes DDL; until then the dead one stays in place and the next statement
// tries the repair again.
func (c *Conn) repairIfBroken() error {
	c.mu.Lock()
	suspect := c.suspect
	c.mu.Unlock()
	if !suspect {
		return nil
	}

	// Detached on purpose: the context the statement ran on was very likely the one
	// that was just canceled, and reconnecting on it would fail instantly.
	ctx, cancel := context.WithTimeout(context.Background(), c.repairTimeout())
	defer cancel()

	old := c.execSession()
	if old == nil {
		return fmt.Errorf("re-pin execution connection: connection closed")
	}
	if old.PingContext(ctx) == nil {
		c.clearSuspect()
		return nil
	}

	// The client giving up on the connection says nothing about the server: an attention
	// the driver could not confirm leaves the request running. Settle that before pinning
	// a second session, or the next operation runs beside its own orphan.
	if err := c.stopOrphan(); err != nil {
		return err
	}

	fresh, err := c.pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("re-pin execution connection: %w", err)
	}
	if err := hardenSession(ctx, fresh); err != nil {
		_ = fresh.Close()
		return err
	}
	var spid int
	if err := fresh.QueryRowContext(ctx, "SELECT @@SPID").Scan(&spid); err != nil {
		_ = fresh.Close()
		return fmt.Errorf("read @@SPID: %w", err)
	}
	// Same guard as open(): block attribution keys off this id, so a zero would
	// silently disable every blocking reaction rather than fail loudly.
	if spid <= 0 {
		_ = fresh.Close()
		return fmt.Errorf("read @@SPID: got non-positive session id %d", spid)
	}

	c.mu.Lock()
	marker, hasMarker := c.marker, c.hasMarker
	c.mu.Unlock()
	if hasMarker {
		if err := setSessionMarker(ctx, fresh, marker); err != nil {
			_ = fresh.Close()
			return err
		}
	}

	login, _ := loginTimeOf(ctx, fresh)

	c.mu.Lock()
	c.exec, c.spid, c.loginTime, c.suspect = fresh, spid, login, false
	c.mu.Unlock()
	_ = old.Close()
	return nil
}

// stopOrphan makes sure the session the dead connection was using is no longer running
// anything, so the next statement does not execute beside it. It owns its own timeout.
//
// The identity check is not ceremony. SQL Server reuses session ids, and by the time we
// look, ours may be gone and the id given to somebody else — Microsoft documents that a
// KILL can then "stop a new process". So the session is only ours when its login_time
// still matches the one recorded when we pinned it, and only then is it killed.
//
// A probe that errors is treated as "cannot tell" and lets the repair continue: refusing
// there would turn a momentary network fault into a run that can never repair itself, and
// re-pinning is itself a liveness test that fails on its own if the server is gone.
func (c *Conn) stopOrphan() error {
	c.mu.Lock()
	spid, login := c.spid, c.loginTime
	c.mu.Unlock()
	if spid <= 0 || login == "" {
		return nil
	}

	// Its own budget, not the repair's. Sharing one context lets the shorter of the two
	// win silently, and since an unanswerable probe means "cannot tell" and lets the
	// repair continue, that turns a refusal into exactly the thing this guards against.
	ctx, cancel := context.WithTimeout(context.Background(), orphanStopTimeout)
	defer cancel()

	refuse := func() error {
		return fmt.Errorf(
			"session %d is still running the abandoned statement after %s; refusing to start another on a second session",
			spid, orphanStopTimeout)
	}

	killed := false
	for {
		id, err := c.SessionIdentity(ctx, spid)
		// Checked before the error: a deadline we set ourselves is a refusal, never a
		// server we could not reach.
		if ctx.Err() != nil {
			return refuse()
		}
		if err != nil || !id.Exists || id.LoginTime != login || !id.Active {
			return nil //nolint:nilerr // gone, reassigned, idle, or unknowable: see above
		}

		if !killed {
			// Best effort: a login without ALTER ANY CONNECTION cannot issue it, and an
			// abandoned statement usually finishes its rollback anyway. The wait decides.
			_, _ = c.pool.ExecContext(ctx, fmt.Sprintf("KILL %d", spid))
			killed = true
		}

		select {
		case <-ctx.Done():
			return refuse()
		case <-time.After(orphanPollInterval):
		}
	}
}

// loginTimeOf reads login_time on the given connection, for the session it holds.
func loginTimeOf(ctx context.Context, conn *sql.Conn) (string, error) {
	var login string
	if err := conn.QueryRowContext(ctx, loginTimeSQL).Scan(&login); err != nil {
		return "", fmt.Errorf("read login_time: %w", err)
	}
	return login, nil
}

// repairTimeout is the budget for re-pinning: the operator's reconnect timeout, or the
// default when none was configured (a Conn built without options, as in tests).
func (c *Conn) repairTimeout() time.Duration {
	if c.repairBudget > 0 {
		return c.repairBudget
	}
	return defaultRepairTimeout
}

// clearSuspect marks the execution connection healthy again.
func (c *Conn) clearSuspect() {
	c.mu.Lock()
	c.suspect = false
	c.mu.Unlock()
}

// ExecDDL runs a DDL statement on the pinned execution connection. Cancellation
// is propagated via ctx; see Kill for the server-side fallback.
func (c *Conn) ExecDDL(ctx context.Context, statement string) error {
	if _, err := c.execStatement(ctx, statement); err != nil {
		return fmt.Errorf("execute ddl: %w", err)
	}
	return nil
}

// ExecRows runs a statement on the pinned execution connection and returns the
// number of rows it affected. The batch-DML driver uses the count to know when a
// predicate loop is exhausted (zero rows affected). Cancellation is propagated via
// ctx; see Kill for the server-side fallback.
func (c *Conn) ExecRows(ctx context.Context, statement string) (int64, error) {
	res, err := c.execStatement(ctx, statement)
	if err != nil {
		return 0, fmt.Errorf("execute dml: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// Kill issues KILL <spid> on the monitoring pool — never the execution
// connection — as the server-side fallback when Go cancellation does not stop
// the DDL.
func (c *Conn) Kill(ctx context.Context, spid int) error {
	if _, err := c.pool.ExecContext(ctx, fmt.Sprintf("KILL %d", spid)); err != nil {
		return fmt.Errorf("kill spid %d: %w", spid, err)
	}
	return nil
}

// Close releases the execution connection and the monitoring pool.
func (c *Conn) Close() error {
	var err error
	c.mu.Lock()
	exec := c.exec
	c.exec = nil
	c.mu.Unlock()
	if exec != nil {
		err = exec.Close()
	}
	if c.pool != nil {
		if cerr := c.pool.Close(); err == nil {
			err = cerr
		}
	}
	return err
}
