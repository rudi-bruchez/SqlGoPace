package mssql

import (
	"context"
	"fmt"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// Pause pauses a running resumable index operation. It is issued on the
// monitoring pool, since the execution connection is busy running the DDL.
func (c *Conn) Pause(ctx context.Context, op ddl.Operation) error {
	stmt, err := ddl.ResumableControlSQL(op, "PAUSE")
	if err != nil {
		return err
	}
	if _, err := c.pool.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("pause operation: %w", err)
	}
	return nil
}

// Resume resumes a paused resumable index operation on the execution connection,
// continuing the work that was already done.
func (c *Conn) Resume(ctx context.Context, op ddl.Operation) error {
	stmt, err := ddl.ResumableControlSQL(op, "RESUME")
	if err != nil {
		return err
	}
	return c.ExecDDL(ctx, stmt)
}
