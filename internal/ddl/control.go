package ddl

import "fmt"

// ResumableControlSQL renders an ALTER INDEX ... PAUSE/RESUME/ABORT statement for
// a resumable index operation. Only index operations support resumable control.
func ResumableControlSQL(op Operation, action string) (string, error) {
	var schema, table, index string
	switch o := op.(type) {
	case RebuildIndex:
		schema, table, index = o.Schema, o.Table, o.Index
	case CreateIndex:
		schema, table, index = o.Schema, o.Table, o.Index
	default:
		return "", fmt.Errorf("%s does not support resumable control: %w", op.CommandType(), ErrUnsupportedOperation)
	}
	return fmt.Sprintf("ALTER INDEX %s ON %s %s;", quoteIdent(index), qualified(schema, table), action), nil
}
