package ddl

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// batchOps is the allowlist of comparison operators usable in a declarative
// batch_update/batch_delete where condition, mapping accepted spellings to their
// canonical T-SQL form. Arbitrary SQL (including set membership) goes through
// where_raw; IN is intentionally not in this iteration's allowlist.
var batchOps = map[string]string{
	"=":           "=",
	"<>":          "<>",
	"!=":          "<>",
	"<":           "<",
	"<=":          "<=",
	">":           ">",
	">=":          ">=",
	"is null":     "IS NULL",
	"is not null": "IS NOT NULL",
}

// normalizeBatchOp canonicalizes an operator (case-insensitive, trimmed) and
// reports whether it is in the allowlist.
func normalizeBatchOp(op string) (string, bool) {
	canon, ok := batchOps[strings.ToLower(strings.TrimSpace(op))]
	return canon, ok
}

// batchOpIsNullTest reports whether a canonical operator is a NULL test (which
// takes no value).
func batchOpIsNullTest(canonical string) bool {
	return canonical == "IS NULL" || canonical == "IS NOT NULL"
}

// renderCondition renders one validated declarative condition as T-SQL.
func renderCondition(c Condition) string {
	op, _ := normalizeBatchOp(c.Op)
	col := quoteIdent(c.Column)
	if batchOpIsNullTest(op) {
		return col + " " + op
	}
	return col + " " + op + " " + renderLiteral(*c.Value)
}

// setClause renders the UPDATE SET list: the raw SET verbatim, or the declarative
// map sorted by column for deterministic SQL.
func (o BatchDML) setClause() string {
	if raw := strings.TrimSpace(o.SetRaw); raw != "" {
		return raw
	}
	parts := make([]string, 0, len(o.Set))
	for _, col := range sortedSetColumns(o.Set) {
		parts = append(parts, quoteIdent(col)+" = "+renderLiteral(o.Set[col]))
	}
	return strings.Join(parts, ", ")
}

// userWhere renders the operator-supplied filter (raw or declarative), or "" when
// none (whole table). It is the predicate of which rows to act on, before any
// self-limiting clause.
func (o BatchDML) userWhere() string {
	if raw := strings.TrimSpace(o.WhereRaw); raw != "" {
		return raw
	}
	if len(o.Where) == 0 {
		return ""
	}
	parts := make([]string, len(o.Where))
	for i, c := range o.Where {
		parts[i] = renderCondition(c)
	}
	return strings.Join(parts, " AND ")
}

// selfLimitClause returns a predicate that excludes rows already holding every
// target literal value, so a literal UPDATE's predicate loop terminates. It is
// empty for a raw SET (the operator's where_raw must self-limit) and for DELETE
// (deleted rows do not reappear).
func (o BatchDML) selfLimitClause() string {
	if o.Verb != "update" || strings.TrimSpace(o.SetRaw) != "" || len(o.Set) == 0 {
		return ""
	}
	// A row still needs work if ANY target column differs from (or is NULL against)
	// its literal target. The disjunction is wrapped once so it is safe to AND with
	// the operator filter.
	//
	// A NULL target needs its own form: `col <> NULL` is UNKNOWN for every row, so the
	// generic clause would reduce to `col IS NULL` — true for exactly the rows the batch
	// had just set, putting every completed row straight back into the match set. The
	// clause written to guarantee termination would guarantee non-termination.
	parts := make([]string, 0, len(o.Set))
	for _, col := range sortedSetColumns(o.Set) {
		q := quoteIdent(col)
		if o.Set[col].IsNull() {
			parts = append(parts, q+" IS NOT NULL")
			continue
		}
		parts = append(parts, q+" IS NULL OR "+q+" <> "+renderLiteral(o.Set[col]))
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// predicateWhere combines the self-limiting clause and the operator filter into the
// effective WHERE for one predicate-strategy batch. It is "" only for a confirmed
// whole-table DELETE.
func (o BatchDML) predicateWhere() string {
	self, user := o.selfLimitClause(), o.userWhere()
	switch {
	case self != "" && user != "":
		return self + " AND (" + user + ")"
	case self != "":
		return self
	default:
		return user
	}
}

// sortedSetColumns returns the SET map's column names in a stable order, so the
// generated SQL is deterministic.
func sortedSetColumns(set map[string]Literal) []string {
	cols := make([]string, 0, len(set))
	for col := range set {
		cols = append(cols, col)
	}
	sort.Strings(cols)
	return cols
}

// maxdopOption renders the trailing OPTION (MAXDOP n) query hint, or "" when MAXDOP
// is not set.
func maxdopOption(res ResolvedOptions) string {
	if res.MaxDOP != nil {
		return " OPTION (MAXDOP " + strconv.Itoa(*res.MaxDOP) + ")"
	}
	return ""
}

// batchStatement builds one predicate-strategy batch statement with the given TOP
// token (a row count for execution, or a placeholder for the indicative plan SQL).
func batchStatement(o BatchDML, topToken string, res ResolvedOptions) string {
	where := ""
	if w := o.predicateWhere(); w != "" {
		where = " WHERE " + w
	}
	opt := maxdopOption(res)
	table := qualified(o.Schema, o.Table)
	if o.Verb == "delete" {
		return fmt.Sprintf("DELETE TOP (%s) FROM %s%s%s;", topToken, table, where, opt)
	}
	return fmt.Sprintf("UPDATE TOP (%s) %s SET %s%s%s;", topToken, table, o.setClause(), where, opt)
}

// keyRangeWhere builds the WHERE body for a key_range statement: the lower bound
// (key > watermark, omitted on the first batch), an optional upper bound (key <=
// next), and the operator filter, all AND-ed. The watermark/next bounds are integer
// literals the driver controls, so inlining them is safe.
func keyRangeWhere(o BatchDML, key string, watermark int64, hasWatermark bool, upper *int64) string {
	k := quoteIdent(key)
	var parts []string
	if hasWatermark {
		parts = append(parts, fmt.Sprintf("%s > %d", k, watermark))
	}
	if upper != nil {
		parts = append(parts, fmt.Sprintf("%s <= %d", k, *upper))
	}
	if uw := o.userWhere(); uw != "" {
		parts = append(parts, "("+uw+")")
	}
	return strings.Join(parts, " AND ")
}

// BatchKeyRangeNextSQL returns the upper-bound key of the next batch: the
// batchSize-th smallest key above the watermark that matches the filter, or NULL
// when the walk is exhausted. The driver reads it as a nullable scalar.
func BatchKeyRangeNextSQL(o BatchDML, key string, batchSize int, watermark int64, hasWatermark bool) string {
	k := quoteIdent(key)
	where := keyRangeWhere(o, key, watermark, hasWatermark, nil)
	if where != "" {
		where = " WHERE " + where
	}
	inner := fmt.Sprintf("SELECT TOP (%d) %s AS k FROM %s%s ORDER BY %s",
		batchSize, k, qualified(o.Schema, o.Table), where, k)
	return fmt.Sprintf("SELECT MAX(k) FROM (%s) x;", inner)
}

// BatchKeyRangeUpdateSQL updates the rows in (watermark, next] that match the filter.
// Each key is processed exactly once across batches, so no self-limiting clause is
// needed.
func BatchKeyRangeUpdateSQL(o BatchDML, key string, watermark, next int64, hasWatermark bool, res ResolvedOptions) string {
	where := keyRangeWhere(o, key, watermark, hasWatermark, &next)
	return fmt.Sprintf("UPDATE %s SET %s WHERE %s%s;",
		qualified(o.Schema, o.Table), o.setClause(), where, maxdopOption(res))
}

// BatchDMLChunkSQL builds one batch statement (predicate strategy): an UPDATE or
// DELETE limited to batchSize rows, looped by the driver until it affects no rows.
func BatchDMLChunkSQL(o BatchDML, batchSize int, res ResolvedOptions) string {
	return batchStatement(o, strconv.Itoa(batchSize), res)
}

// BatchUnmatchedRowsSQL counts, up to limit, the rows the operation's filter would
// NOT act on. Preflight uses it to give confirm_full_table a meaning: zero spared rows
// is a whole-table operation however the filter is spelled, so `where_raw: "1=1"` and
// `where: [{column: Id, op: ">=", value: 0}]` no longer walk past the guard that only
// ever checked whether the filter was absent.
//
// The CASE wrapper is not decoration. A plain `NOT (pred)` returns only the rows where
// the predicate is FALSE, dropping those where it is UNKNOWN — but the DML does not act
// on those either, so they are spared and must be counted. CASE resolves UNKNOWN to its
// ELSE branch, which makes "= 0" exactly "this row survives".
//
// TOP bounds the cost: the probe stops at the first `limit` survivors, so a selective
// filter is answered almost immediately. Only a filter that really does match everything
// scans the table — and that is the case worth paying for.
//
// It returns "" when there is no filter at all: that is whole-table by construction and
// Validate already requires confirm_full_table for it, so there is nothing to probe.
func BatchUnmatchedRowsSQL(o BatchDML, limit int) string {
	// Probe the predicate the operation will actually run, not just the operator's filter.
	// For a literal batch_update that includes the self-limiting clause, so an idempotent
	// update whose filter is deliberately broad (`where_raw: "1=1"`, `set: {Archived: 1}`)
	// is credited with the rows already at the target — it was reported as a whole-table
	// rewrite, and the remedy the failure names is confirm_full_table, which turns this
	// check off. A false positive that teaches operators to disarm the guard is worse than
	// no guard.
	//
	// key_range is the exception: its statement carries no self-limiting clause (each key
	// is processed once), so crediting it with one would spare rows the walk does update.
	return unmatchedRowsSQL(o, o.userWhere(), limit)
}

// BatchUntouchedRowsSQL counts, up to limit, the rows the operation will not modify: the
// operator's filter plus the self-limiting clause a literal UPDATE carries. It answers a
// different question from BatchUnmatchedRowsSQL, and preflight needs both.
//
// The filter decides whether an operation is whole-table *as written* — `where_raw: "1=1"`
// excludes nothing whatever the data looks like. The self-limit decides only whether that
// is survivable on this particular table today, which is a warning and not a pass: a
// verdict that turned on it would let the same manifest fail on an untouched table and
// pass once a prior run had left rows at the target.
//
// It is equal to BatchUnmatchedRowsSQL for a DELETE (which has no self-limiting clause)
// and under key_range (whose statement does not carry one), so preflight only pays for the
// second probe when the two can differ.
func BatchUntouchedRowsSQL(o BatchDML, limit int) string {
	if o.Batch.IsKeyRange() {
		return unmatchedRowsSQL(o, o.userWhere(), limit)
	}
	return unmatchedRowsSQL(o, o.predicateWhere(), limit)
}

// unmatchedRowsSQL builds the shared probe: how many of the first limit rows the given
// predicate does not match. An empty predicate returns "" — no filter at all is whole-table
// by construction, and Validate already requires confirm_full_table for it.
func unmatchedRowsSQL(o BatchDML, where string, limit int) string {
	if where == "" {
		return ""
	}
	return fmt.Sprintf(
		"SELECT COUNT(*) FROM (SELECT TOP (%d) 1 AS c FROM %s WHERE CASE WHEN (%s) THEN 1 ELSE 0 END = 0) x;",
		limit, qualified(o.Schema, o.Table), where)
}

// generateBatchDML returns an INDICATIVE statement for the plan/report. The real
// SQL is built per batch at run time by the batch-DML driver (the batch size comes
// from adaptive sizing), so PlannedOperation.SQL is illustrative only; the engine
// routes batch_update/batch_delete to the driver rather than executing this string.
func generateBatchDML(o BatchDML, res ResolvedOptions) string {
	return fmt.Sprintf("-- %s runs at run time in batches; representative statement:\n%s",
		o.CommandType(), batchStatement(o, "<batch_rows>", res))
}
