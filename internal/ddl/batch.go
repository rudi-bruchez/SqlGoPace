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
	parts := make([]string, 0, len(o.Set))
	for _, col := range sortedSetColumns(o.Set) {
		q := quoteIdent(col)
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

// batchStatement builds one predicate-strategy batch statement with the given TOP
// token (a row count for execution, or a placeholder for the indicative plan SQL).
func batchStatement(o BatchDML, topToken string, res ResolvedOptions) string {
	where := ""
	if w := o.predicateWhere(); w != "" {
		where = " WHERE " + w
	}
	opt := ""
	if res.MaxDOP != nil {
		opt = " OPTION (MAXDOP " + strconv.Itoa(*res.MaxDOP) + ")"
	}
	table := qualified(o.Schema, o.Table)
	if o.Verb == "delete" {
		return fmt.Sprintf("DELETE TOP (%s) FROM %s%s%s;", topToken, table, where, opt)
	}
	return fmt.Sprintf("UPDATE TOP (%s) %s SET %s%s%s;", topToken, table, o.setClause(), where, opt)
}

// BatchDMLChunkSQL builds one batch statement (predicate strategy): an UPDATE or
// DELETE limited to batchSize rows, looped by the driver until it affects no rows.
func BatchDMLChunkSQL(o BatchDML, batchSize int, res ResolvedOptions) string {
	return batchStatement(o, strconv.Itoa(batchSize), res)
}

// generateBatchDML returns an INDICATIVE statement for the plan/report. The real
// SQL is built per batch at run time by the batch-DML driver (the batch size comes
// from adaptive sizing), so PlannedOperation.SQL is illustrative only; the engine
// routes batch_update/batch_delete to the driver rather than executing this string.
func generateBatchDML(o BatchDML, res ResolvedOptions) string {
	return fmt.Sprintf("-- %s runs at run time in batches; representative statement:\n%s",
		o.CommandType(), batchStatement(o, "<batch_rows>", res))
}
