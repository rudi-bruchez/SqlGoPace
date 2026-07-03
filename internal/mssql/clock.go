package mssql

import (
	"context"
	"fmt"
	"time"
)

// ServerNow returns the SQL Server's local wall-clock time (SYSDATETIME()). It is
// read on the monitoring pool, never the pinned execution connection, so it is
// never blocked by the DDL in flight. The returned time carries the server's
// wall-clock components (datetime2 has no offset); callers read Hour/Minute/Weekday
// directly and must not treat its Location as meaningful.
func (c *Conn) ServerNow(ctx context.Context) (time.Time, error) {
	var t time.Time
	if err := c.pool.QueryRowContext(ctx, "SELECT SYSDATETIME()").Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("read server time: %w", err)
	}
	return t, nil
}
