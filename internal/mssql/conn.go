package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	mssqldb "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// Conn holds the two distinct connections SqlGoPace needs: a pinned, dedicated
// execution connection (so @@SPID is stable across the whole DDL) and a separate
// monitoring pool that is never blocked by the DDL it observes.
type Conn struct {
	pool *sql.DB   // monitoring connections
	exec *sql.Conn // pinned execution connection
	spid int
}

// Open connects with the given DSN (ADO or URL form), stamps the application
// version into the connection's application name (visible server-side as
// program_name), pins a dedicated execution connection, hardens its session, and
// records its SPID. The driver applies no query timeout: long DDL is bounded by
// monitoring and context cancellation, never a fixed timer.
func Open(ctx context.Context, dsn, version string) (*Conn, error) {
	return open(ctx, dsn, "", version)
}

// OpenDatabase connects like Open but targets a specific database, reusing the
// server and credentials from the base DSN with the catalog overridden. It is how
// multi-database maintenance reaches each database in its own context, rather than
// issuing USE on a pooled connection (the pool trap, SPECS §3 / spec §17.2).
func OpenDatabase(ctx context.Context, dsn, database, version string) (*Conn, error) {
	return open(ctx, dsn, database, version)
}

// open is the shared connection setup. A non-empty database overrides the DSN's
// catalog so the pinned execution connection lands in that database.
func open(ctx context.Context, dsn, database, version string) (*Conn, error) {
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	cfg.AppName = appNameWithVersion(cfg.AppName, version)
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

	c := &Conn{pool: pool, exec: exec}
	if err := c.harden(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.exec.QueryRowContext(ctx, "SELECT @@SPID").Scan(&c.spid); err != nil {
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
	return c, nil
}

// harden applies the safety session settings to the execution connection:
// XACT_ABORT ON, and DEADLOCK_PRIORITY LOW so the DDL is the deadlock victim
// rather than a user query.
func (c *Conn) harden(ctx context.Context) error {
	const stmt = "SET XACT_ABORT ON; SET DEADLOCK_PRIORITY LOW;"
	if _, err := c.exec.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("harden execution session: %w", err)
	}
	return nil
}

// appNameWithVersion appends the application version to the configured application
// name so the running build is visible server-side (sys.dm_exec_sessions
// program_name), e.g. "SqlGoPace/0.1.0". A missing or default-driver app name
// falls back to "SqlGoPace".
func appNameWithVersion(appName, version string) string {
	base := strings.TrimSpace(appName)
	if base == "" || base == "go-mssqldb" {
		base = "SqlGoPace"
	}
	v := strings.TrimSpace(version)
	if v == "" {
		return base
	}
	return base + "/" + v
}

// SPID returns the session id of the pinned execution connection.
func (c *Conn) SPID() int { return c.spid }

// Detect reads the target server's version, edition, recovery model, and ADR
// state over the execution connection.
func (c *Conn) Detect(ctx context.Context) (ServerInfo, error) {
	return DetectServer(ctx, c.exec)
}

// SetMarker writes a run marker into CONTEXT_INFO on the execution session so an
// orphaned DDL can be correlated to its run after a crash.
func (c *Conn) SetMarker(ctx context.Context, marker [16]byte) error {
	if _, err := c.exec.ExecContext(ctx, "SET CONTEXT_INFO "+ContextInfoLiteral(marker)); err != nil {
		return fmt.Errorf("set context_info: %w", err)
	}
	return nil
}

// ExecDDL runs a DDL statement on the pinned execution connection. Cancellation
// is propagated via ctx; see Kill for the server-side fallback.
func (c *Conn) ExecDDL(ctx context.Context, statement string) error {
	if _, err := c.exec.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("execute ddl: %w", err)
	}
	return nil
}

// ExecRows runs a statement on the pinned execution connection and returns the
// number of rows it affected. The batch-DML driver uses the count to know when a
// predicate loop is exhausted (zero rows affected). Cancellation is propagated via
// ctx; see Kill for the server-side fallback.
func (c *Conn) ExecRows(ctx context.Context, statement string) (int64, error) {
	res, err := c.exec.ExecContext(ctx, statement)
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
	if c.exec != nil {
		err = c.exec.Close()
	}
	if c.pool != nil {
		if cerr := c.pool.Close(); err == nil {
			err = cerr
		}
	}
	return err
}
