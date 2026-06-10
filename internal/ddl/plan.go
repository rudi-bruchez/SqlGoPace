package ddl

import "fmt"

// PlannedOperation is one manifest operation with its resolved options rendered
// to T-SQL and the explanation trail behind that resolution.
type PlannedOperation struct {
	Operation Operation
	SQL       string
	Options   ResolvedOptions
	Decisions []Decision
}

// Plan resolves options and generates the T-SQL for every operation in the
// manifest against the target server. It is pure (no I/O), which is what makes
// --dry-run and --explain work offline. Note: an "ALL" index rebuild is rendered
// verbatim here; expanding it to one operation per index is a runtime concern
// handled before planning.
func Plan(m *Manifest, t Target, matrix *Matrix, p Policy) ([]PlannedOperation, error) {
	planned := make([]PlannedOperation, 0, len(m.Operations))
	for i, op := range m.Operations {
		res, decisions := Resolve(op, t, matrix, p)
		sql, err := Generate(op, res)
		if err != nil {
			return nil, fmt.Errorf("operation %d (%s): %w", i, op.CommandType(), err)
		}
		planned = append(planned, PlannedOperation{
			Operation: op,
			SQL:       sql,
			Options:   res,
			Decisions: decisions,
		})
	}
	return planned, nil
}
