// Package preflight runs health and precondition checks before any DDL is
// executed. A failing report routes the manifest to the failed directory without
// a single lock having been taken. The check logic is pure; the server facts are
// gathered through the narrow Prober interface (satisfied by *mssql.Conn).
package preflight

import (
	"context"
	"fmt"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// Severity is the outcome level of a single check.
type Severity int

const (
	// Pass means the check is satisfied.
	Pass Severity = iota
	// Warn means a non-blocking concern was found.
	Warn
	// Fail means the manifest must not run.
	Fail
)

// String returns the uppercase label of the severity.
func (s Severity) String() string {
	switch s {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	default:
		return "UNKNOWN"
	}
}

// Check is the result of one preflight check.
type Check struct {
	Name     string
	Severity Severity
	Detail   string
}

// Report aggregates all checks for one manifest.
type Report struct {
	Checks []Check
}

func (r *Report) add(c Check) { r.Checks = append(r.Checks, c) }

// HasFailure reports whether any check failed.
func (r Report) HasFailure() bool {
	for _, c := range r.Checks {
		if c.Severity == Fail {
			return true
		}
	}
	return false
}

// HasWarning reports whether any check raised a warning.
func (r Report) HasWarning() bool {
	for _, c := range r.Checks {
		if c.Severity == Warn {
			return true
		}
	}
	return false
}

// OK reports whether the manifest may proceed (no failures).
func (r Report) OK() bool { return !r.HasFailure() }

// CheckServer verifies the engine edition is one SqlGoPace supports.
func CheckServer(info mssql.ServerInfo) Check {
	if !info.Supported() {
		return Check{"server", Fail, fmt.Sprintf("unsupported engine edition %d", info.EngineEdition)}
	}
	return Check{"server", Pass, fmt.Sprintf("tier=%s major=%d adr=%t recovery=%s",
		info.Tier(), info.MajorVersion, info.ADREnabled, info.RecoveryModel)}
}

// CheckLog verifies the transaction log is healthy before starting. The absolute
// used bytes is the primary signal; the percent (of the current file) is
// secondary. A non-NOTHING log_reuse_wait is surfaced as a warning.
func CheckLog(ls mssql.LogSpace, reuseWait string, maxBytes int64, maxPercent int) Check {
	switch used := ls.UsedBytes(); {
	case used >= maxBytes:
		return Check{"transaction log", Fail, fmt.Sprintf("log already uses %d bytes (cap %d)", used, maxBytes)}
	case int(ls.UsedPercent) >= maxPercent:
		return Check{"transaction log", Fail, fmt.Sprintf("log already at %.0f%% (cap %d%%)", ls.UsedPercent, maxPercent)}
	case reuseWait != "" && reuseWait != "NOTHING":
		return Check{"transaction log", Warn, fmt.Sprintf("log_reuse_wait = %s", reuseWait)}
	default:
		return Check{"transaction log", Pass, fmt.Sprintf("%.0f%% used, reuse_wait=%s", ls.UsedPercent, reuseWait)}
	}
}

// CheckBlocking warns when sessions are already blocked before we start, so we
// know the server is not in a clean state.
func CheckBlocking(sessions []mssql.Session) Check {
	blocked := 0
	for _, s := range sessions {
		if s.BlockingSPID > 0 {
			blocked++
		}
	}
	if blocked > 0 {
		return Check{"blocking", Warn, fmt.Sprintf("%d session(s) already blocked before start", blocked)}
	}
	return Check{"blocking", Pass, "no pre-existing blocking"}
}

// CheckOperation verifies an operation's preconditions: the table must exist, and
// the target object must exist (for rebuild/alter/drop) or not exist (for
// create/add — already present means the idempotent guard will skip it).
func CheckOperation(op ddl.Operation, tableExists, targetExists bool) Check {
	ref := op.Target()
	name := fmt.Sprintf("%s %s.%s", op.CommandType(), ref.Schema, ref.Table)

	if !tableExists {
		return Check{name, Fail, fmt.Sprintf("table [%s].[%s] does not exist", ref.Schema, ref.Table)}
	}

	switch op.(type) {
	case ddl.RebuildIndex, ddl.DropIndex, ddl.AlterColumn, ddl.DropColumn, ddl.DropConstraint:
		if !targetExists {
			return Check{name, Fail, fmt.Sprintf("%q does not exist", ref.Name)}
		}
	case ddl.CreateIndex, ddl.AddColumn, ddl.AddConstraint:
		if targetExists {
			return Check{name, Warn, fmt.Sprintf("%q already exists; operation will be skipped (idempotent)", ref.Name)}
		}
	}
	return Check{name, Pass, "preconditions satisfied"}
}

// Prober is the narrow set of server facts preflight needs. *mssql.Conn satisfies it.
type Prober interface {
	LogSpace(ctx context.Context) (mssql.LogSpace, error)
	LogReuseWait(ctx context.Context) (string, error)
	ActiveSessions(ctx context.Context) ([]mssql.Session, error)
	TableExists(ctx context.Context, schema, table string) (bool, error)
	IndexExists(ctx context.Context, schema, table, index string) (bool, error)
	ColumnExists(ctx context.Context, schema, table, column string) (bool, error)
	ConstraintExists(ctx context.Context, schema, table, constraint string) (bool, error)
}

var _ Prober = (*mssql.Conn)(nil)

// Thresholds are the log-pressure limits sourced from config.
type Thresholds struct {
	LogMaxBytes   int64
	LogMaxPercent int
}

// Run gathers server facts and builds the preflight report for a manifest.
func Run(ctx context.Context, p Prober, info mssql.ServerInfo, m *ddl.Manifest, th Thresholds) (Report, error) {
	var rep Report
	rep.add(CheckServer(info))

	ls, err := p.LogSpace(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("preflight log space: %w", err)
	}
	reuseWait, err := p.LogReuseWait(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("preflight log reuse wait: %w", err)
	}
	rep.add(CheckLog(ls, reuseWait, th.LogMaxBytes, th.LogMaxPercent))

	sessions, err := p.ActiveSessions(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("preflight active sessions: %w", err)
	}
	rep.add(CheckBlocking(sessions))

	for _, op := range m.Operations {
		tableExists, targetExists, err := objectExistence(ctx, p, op)
		if err != nil {
			return Report{}, err
		}
		rep.add(CheckOperation(op, tableExists, targetExists))
	}
	return rep, nil
}

// objectExistence resolves whether the operation's table and target object exist.
// The target lookup is skipped when the table is absent (the check already fails).
func objectExistence(ctx context.Context, p Prober, op ddl.Operation) (table, target bool, err error) {
	ref := op.Target()
	table, err = p.TableExists(ctx, ref.Schema, ref.Table)
	if err != nil {
		return false, false, fmt.Errorf("check table %s.%s: %w", ref.Schema, ref.Table, err)
	}
	if !table {
		return false, false, nil
	}

	// ALTER INDEX ALL ... REBUILD has no single named index to verify; the table's
	// existence (checked above) is the precondition.
	if ddl.IsAllIndexRebuild(op) {
		return table, true, nil
	}

	switch op.(type) {
	case ddl.RebuildIndex, ddl.CreateIndex, ddl.DropIndex:
		target, err = p.IndexExists(ctx, ref.Schema, ref.Table, ref.Name)
	case ddl.AddColumn, ddl.AlterColumn, ddl.DropColumn:
		target, err = p.ColumnExists(ctx, ref.Schema, ref.Table, ref.Name)
	case ddl.AddConstraint, ddl.DropConstraint:
		target, err = p.ConstraintExists(ctx, ref.Schema, ref.Table, ref.Name)
	}
	if err != nil {
		return false, false, fmt.Errorf("check target %q: %w", ref.Name, err)
	}
	return table, target, nil
}
