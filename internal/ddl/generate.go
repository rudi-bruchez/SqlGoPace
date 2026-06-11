package ddl

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUnsupportedOperation is returned by Generate for an operation type it does
// not know how to render (should not happen for the closed operation set).
var ErrUnsupportedOperation = errors.New("unsupported operation for generation")

// Generate renders the final T-SQL for op with the resolved options injected.
// It assumes op has already been validated.
func Generate(op Operation, res ResolvedOptions) (string, error) {
	switch o := op.(type) {
	case RebuildIndex:
		return generateRebuildIndex(o, res), nil
	case CreateIndex:
		return generateCreateIndex(o, res), nil
	case AlterColumn:
		return generateAlterColumn(o, res), nil
	case AddColumn:
		return generateAddColumn(o), nil
	case AddConstraint:
		return generateAddConstraint(o, res), nil
	case DropIndex:
		return generateDropIndex(o), nil
	case DropColumn:
		return generateDropColumn(o), nil
	case DropConstraint:
		return generateDropConstraint(o), nil
	default:
		return "", fmt.Errorf("%T: %w", op, ErrUnsupportedOperation)
	}
}

// --- identifier and literal quoting --------------------------------------

// quoteIdent wraps an identifier in brackets, doubling any embedded ].
func quoteIdent(s string) string {
	return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
}

// qualified renders [schema].[table].
func qualified(schema, table string) string {
	return quoteIdent(schema) + "." + quoteIdent(table)
}

// nLiteral renders an N'...' string literal, doubling any embedded quote.
func nLiteral(s string) string {
	return "N'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// objectLiteral renders N'[schema].[table]' for OBJECT_ID and COL_LENGTH.
func objectLiteral(schema, table string) string {
	return nLiteral(qualified(schema, table))
}

func nullability(nullable bool) string {
	if nullable {
		return "NULL"
	}
	return "NOT NULL"
}

func guard(condition, statement string) string {
	return condition + "\n    " + statement
}

// --- WITH-clause builder -------------------------------------------------

// withClause builds a single WITH (...) clause from the resolved options and an
// optional DATA_COMPRESSION value, or "" when no options apply.
func withClause(res ResolvedOptions, dataCompression string) string {
	var parts []string
	switch {
	case res.Online && res.WaitAtLowPriority:
		parts = append(parts, fmt.Sprintf(
			"ONLINE = ON (WAIT_AT_LOW_PRIORITY (MAX_DURATION = %d MINUTES, ABORT_AFTER_WAIT = %s))",
			res.MaxDurationMinutes, res.AbortAfterWait))
	case res.Online:
		parts = append(parts, "ONLINE = ON")
	}
	if res.Resumable {
		parts = append(parts, "RESUMABLE = ON")
	}
	if res.MaxDOP != nil {
		parts = append(parts, "MAXDOP = "+strconv.Itoa(*res.MaxDOP))
	}
	if res.SortInTempDB {
		parts = append(parts, "SORT_IN_TEMPDB = ON")
	}
	if dataCompression != "" {
		parts = append(parts, "DATA_COMPRESSION = "+dataCompression)
	}
	if len(parts) == 0 {
		return ""
	}
	return " WITH (" + strings.Join(parts, ", ") + ")"
}

func quoteColumns(columns []string) string {
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = quoteIdent(c)
	}
	return strings.Join(quoted, ", ")
}

// --- per-operation generators --------------------------------------------

// IsAllIndexRebuild reports whether op is a REBUILD over the "ALL" sentinel,
// i.e. ALTER INDEX ALL ... REBUILD. Several rules differ for ALL: there is no
// single named index to verify at preflight, and RESUMABLE is not supported.
func IsAllIndexRebuild(op Operation) bool {
	ri, ok := op.(RebuildIndex)
	return ok && strings.EqualFold(ri.Index, "ALL")
}

func generateRebuildIndex(o RebuildIndex, res ResolvedOptions) string {
	index := quoteIdent(o.Index)
	if strings.EqualFold(o.Index, "ALL") {
		index = "ALL"
	}
	return fmt.Sprintf("ALTER INDEX %s ON %s REBUILD%s;",
		index, qualified(o.Schema, o.Table), withClause(res, o.DataCompression))
}

func generateCreateIndex(o CreateIndex, res ResolvedOptions) string {
	unique := ""
	if o.Unique {
		unique = "UNIQUE "
	}
	stmt := fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)%s;",
		unique, quoteIdent(o.Index), qualified(o.Schema, o.Table),
		quoteColumns(o.Columns), withClause(res, o.DataCompression))
	cond := fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = %s AND object_id = OBJECT_ID(%s))",
		nLiteral(o.Index), objectLiteral(o.Schema, o.Table))
	return guard(cond, stmt)
}

func generateAlterColumn(o AlterColumn, res ResolvedOptions) string {
	with := ""
	if res.Online {
		with = " WITH (ONLINE = ON)"
	}
	return fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s %s %s%s;",
		qualified(o.Schema, o.Table), quoteIdent(o.Column), o.DataType, nullability(o.Nullable), with)
}

func generateAddColumn(o AddColumn) string {
	def := ""
	if o.Default != nil {
		def = " DEFAULT " + renderLiteral(*o.Default)
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD %s %s %s%s;",
		qualified(o.Schema, o.Table), quoteIdent(o.Column), o.DataType, nullability(o.Nullable), def)
	cond := fmt.Sprintf("IF COL_LENGTH(%s, %s) IS NULL",
		objectLiteral(o.Schema, o.Table), nLiteral(o.Column))
	return guard(cond, stmt)
}

func generateAddConstraint(o AddConstraint, res ResolvedOptions) string {
	kind := "PRIMARY KEY"
	if strings.EqualFold(o.Kind, "unique") {
		kind = "UNIQUE"
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s (%s)%s;",
		qualified(o.Schema, o.Table), quoteIdent(o.Constraint), kind,
		quoteColumns(o.Columns), withClause(res, ""))
	cond := fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE name = %s AND parent_object_id = OBJECT_ID(%s))",
		nLiteral(o.Constraint), objectLiteral(o.Schema, o.Table))
	return guard(cond, stmt)
}

func generateDropIndex(o DropIndex) string {
	stmt := fmt.Sprintf("DROP INDEX %s ON %s;", quoteIdent(o.Index), qualified(o.Schema, o.Table))
	cond := fmt.Sprintf("IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = %s AND object_id = OBJECT_ID(%s))",
		nLiteral(o.Index), objectLiteral(o.Schema, o.Table))
	return guard(cond, stmt)
}

func generateDropColumn(o DropColumn) string {
	stmt := fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s;", qualified(o.Schema, o.Table), quoteIdent(o.Column))
	cond := fmt.Sprintf("IF COL_LENGTH(%s, %s) IS NOT NULL",
		objectLiteral(o.Schema, o.Table), nLiteral(o.Column))
	return guard(cond, stmt)
}

func generateDropConstraint(o DropConstraint) string {
	stmt := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s;", qualified(o.Schema, o.Table), quoteIdent(o.Constraint))
	cond := fmt.Sprintf("IF EXISTS (SELECT 1 FROM sys.objects WHERE name = %s AND parent_object_id = OBJECT_ID(%s))",
		nLiteral(o.Constraint), objectLiteral(o.Schema, o.Table))
	return guard(cond, stmt)
}

func renderLiteral(l Literal) string {
	if l.String {
		return nLiteral(l.Raw)
	}
	return l.Raw
}
