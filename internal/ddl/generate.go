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
	case ReorganizeIndex:
		return generateReorganizeIndex(o), nil
	case RebuildHeap:
		return generateRebuildHeap(o, res), nil
	case UpdateStatistics:
		return generateUpdateStatistics(o), nil
	case CheckDB:
		return generateCheckDB(o, res), nil
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
	case Shrink:
		return generateShrink(o, res), nil
	case ShrinkTempdb:
		return generateShrinkTempdb(o, res), nil
	case BatchDML:
		return generateBatchDML(o, res), nil
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

// isSinglePartitionRebuild reports whether op rebuilds a single named partition
// (REBUILD PARTITION = n). Its WITH clause accepts only SORT_IN_TEMPDB / MAXDOP /
// DATA_COMPRESSION / XML_COMPRESSION plus ONLINE / WAIT_AT_LOW_PRIORITY — never
// RESUMABLE, which option resolution must therefore drop.
func isSinglePartitionRebuild(op Operation) bool {
	ri, ok := op.(RebuildIndex)
	return ok && ri.Partition != nil
}

func generateRebuildIndex(o RebuildIndex, res ResolvedOptions) string {
	index := quoteIdent(o.Index)
	if strings.EqualFold(o.Index, "ALL") {
		index = "ALL"
	}
	return fmt.Sprintf("ALTER INDEX %s ON %s REBUILD%s%s;",
		index, qualified(o.Schema, o.Table), partitionClause(o.Partition), withClause(res, o.DataCompression))
}

// partitionClause renders " PARTITION = n" for a partition-targeted index
// operation, or "" for a whole-index operation.
func partitionClause(partition *int) string {
	if partition == nil {
		return ""
	}
	return " PARTITION = " + strconv.Itoa(*partition)
}

func generateReorganizeIndex(o ReorganizeIndex) string {
	with := ""
	if o.LOBCompaction {
		with = " WITH (LOB_COMPACTION = ON)"
	}
	return fmt.Sprintf("ALTER INDEX %s ON %s REORGANIZE%s%s;",
		quoteIdent(o.Index), qualified(o.Schema, o.Table), partitionClause(o.Partition), with)
}

func generateRebuildHeap(o RebuildHeap, res ResolvedOptions) string {
	return fmt.Sprintf("ALTER TABLE %s REBUILD%s;",
		qualified(o.Schema, o.Table), withClause(res, o.DataCompression))
}

func generateUpdateStatistics(o UpdateStatistics) string {
	target := qualified(o.Schema, o.Table)
	if o.Statistic != "" {
		target += " " + quoteIdent(o.Statistic)
	}
	with := ""
	switch {
	case o.FullScan:
		with = " WITH FULLSCAN"
	case o.SamplePercent != nil:
		with = fmt.Sprintf(" WITH SAMPLE %d PERCENT", *o.SamplePercent)
	case o.Resample:
		with = " WITH RESAMPLE"
	}
	return fmt.Sprintf("UPDATE STATISTICS %s%s;", target, with)
}

func generateCheckDB(o CheckDB, res ResolvedOptions) string {
	opts := []string{"NO_INFOMSGS", "ALL_ERRORMSGS"}
	if o.PhysicalOnly {
		opts = append(opts, "PHYSICAL_ONLY")
	}
	if o.DataPurity {
		opts = append(opts, "DATA_PURITY")
	}
	if res.MaxDOP != nil {
		opts = append(opts, "MAXDOP = "+strconv.Itoa(*res.MaxDOP))
	}
	return fmt.Sprintf("DBCC CHECKDB (%s) WITH %s;", quoteIdent(o.Database), strings.Join(opts, ", "))
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

// --- shrink generators ---------------------------------------------------
//
// DBCC SHRINKFILE does not fit the WITH-clause builder used by index/table DDL:
// its WAIT_AT_LOW_PRIORITY has no MAX_DURATION and is not nested in ONLINE = ON.
// These dedicated helpers are called per chunk by the runtime shrink driver.

// shrinkWith renders the trailing " WITH ..." for a shrink statement. WALP is
// emitted only when resolved on; NO_INFOMSGS is always added to cut noise.
func shrinkWith(res ResolvedOptions) string {
	if res.WaitAtLowPriority {
		return fmt.Sprintf(" WITH WAIT_AT_LOW_PRIORITY (ABORT_AFTER_WAIT = %s), NO_INFOMSGS", res.AbortAfterWait)
	}
	return " WITH NO_INFOMSGS"
}

// ShrinkChunkSQL builds one page-moving shrink statement: DBCC SHRINKFILE with a
// target size in megabytes. file is a logical file name (sys.database_files.name).
func ShrinkChunkSQL(file string, targetMB int, res ResolvedOptions) string {
	return fmt.Sprintf("DBCC SHRINKFILE (%s, %d)%s;", nLiteral(file), targetMB, shrinkWith(res))
}

// ShrinkTruncateOnlySQL builds a TRUNCATEONLY shrink: it releases trailing free
// space without moving pages (no fragmentation, no target size). WALP does not
// apply, so NO_INFOMSGS is the only option.
func ShrinkTruncateOnlySQL(file string) string {
	return fmt.Sprintf("DBCC SHRINKFILE (%s, TRUNCATEONLY) WITH NO_INFOMSGS;", nLiteral(file))
}

// generateShrink returns an INDICATIVE statement for a shrink operation. The real
// SQL is multi-statement and built at run time by the shrink driver: target sizes
// come from live DMV reads and files:all expands to one op per file. So
// PlannedOperation.SQL for a shrink is illustrative only; the engine routes shrink
// operations to the driver rather than executing this string.
func generateShrink(o Shrink, res ResolvedOptions) string {
	return fmt.Sprintf(
		"-- shrink is built at run time per chunk; representative statement:\n"+
			"DBCC SHRINKFILE (%s, <target_mb>)%s;",
		nLiteral(o.FilesOrAll()), shrinkWith(res))
}

// generateShrinkTempdb returns an INDICATIVE statement. Real SQL is multi-statement
// and built at run time (per file, target from live DMV reads), run in tempdb context.
func generateShrinkTempdb(o ShrinkTempdb, res ResolvedOptions) string {
	return fmt.Sprintf(
		"-- shrink_tempdb is built at run time per file (tempdb context); representative statement:\n"+
			"USE [tempdb]; DBCC SHRINKFILE (<file>, %d)%s;",
		o.TargetSizeMB, shrinkWith(res))
}

func renderLiteral(l Literal) string {
	if l.IsNull() {
		return "NULL"
	}
	if l.String {
		return nLiteral(l.Raw)
	}
	return l.Raw
}
