package mssql

import (
	"context"
	"database/sql"
	"fmt"

	// Register the "sqlserver" driver.
	_ "github.com/microsoft/go-mssqldb"
)

// Conn holds the two distinct connections SqlGoPace needs: a pinned, dedicated
// execution connection (so @@SPID is stable across the whole DDL) and a separate
// monitoring pool that is never blocked by the DDL it observes.
type Conn struct {
	pool *sql.DB   // monitoring connections
	exec *sql.Conn // pinned execution connection
	spid int
}

// Open connects with the given DSN ("sqlserver" driver, ADO or URL form),
// pins a dedicated execution connection, hardens its session, and records its
// SPID. The driver applies no query timeout: long DDL is bounded by monitoring
// and context cancellation, never a fixed timer.
func Open(ctx context.Context, dsn string) (*Conn, error) {
	pool, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, fmt.Errorf("open connection: %w", err)
	}
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
