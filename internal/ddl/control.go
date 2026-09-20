package ddl

import "fmt"

// ResumableControlSQL renders an ALTER INDEX ... PAUSE/RESUME/ABORT statement for
// a resumable index operation. Only index operations support resumable control.
//
// For RESUME prefer ResumeSQL, which carries the low-priority lock options: the bare
// form this renders is correct only for an operation that never asked for them.
func ResumableControlSQL(op Operation, action string) (string, error) {
	index, table, err := resumableTarget(op)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ALTER INDEX %s ON %s %s;", index, table, action), nil
}

// ResumeSQL renders an ALTER INDEX ... RESUME carrying the low-priority lock options the
// operation was started with.
//
// SQL Server does not remember them across a pause. "Omitting the WAIT_AT_LOW_PRIORITY
// option is equivalent to WAIT_AT_LOW_PRIORITY (MAX_DURATION = 0 minutes,
// ABORT_AFTER_WAIT = NONE)" (ALTER INDEX, RESUME), and ABORT_AFTER_WAIT = NONE is
// documented as "continue waiting for the lock with normal priority". So a bare RESUME
// silently drops the yielding the manifest asked for — on every resume, including the
// ones the pressure loop takes by itself, which is where most resumes happen.
//
// The clause is not nested in ONLINE = ON here, unlike the REBUILD that started the
// operation: RESUME takes it directly inside its own WITH.
func ResumeSQL(op Operation, res ResolvedOptions) (string, error) {
	index, table, err := resumableTarget(op)
	if err != nil {
		return "", err
	}
	if !res.WaitAtLowPriority {
		return fmt.Sprintf("ALTER INDEX %s ON %s RESUME;", index, table), nil
	}
	return fmt.Sprintf(
		"ALTER INDEX %s ON %s RESUME WITH (WAIT_AT_LOW_PRIORITY (MAX_DURATION = %d MINUTES, ABORT_AFTER_WAIT = %s));",
		index, table, res.MaxDurationMinutes, res.AbortAfterWait), nil
}

// resumableTarget returns the quoted index and qualified table of an operation that
// supports resumable control, or ErrUnsupportedOperation for one that does not.
func resumableTarget(op Operation) (index, table string, err error) {
	var schema, tbl, idx string
	switch o := op.(type) {
	case RebuildIndex:
		schema, tbl, idx = o.Schema, o.Table, o.Index
	case CreateIndex:
		schema, tbl, idx = o.Schema, o.Table, o.Index
	default:
		return "", "", fmt.Errorf("%s does not support resumable control: %w", op.CommandType(), ErrUnsupportedOperation)
	}
	return quoteIdent(idx), qualified(schema, tbl), nil
}
