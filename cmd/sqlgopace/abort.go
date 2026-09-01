package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/version"
)

// resumableLister lists resumable index operations. *mssql.Conn satisfies it.
type resumableLister interface {
	ResumableOps(ctx context.Context) ([]mssql.ResumableOp, error)
}

// ddlExecer runs a DDL statement. *mssql.Conn satisfies it.
type ddlExecer interface {
	ExecDDL(ctx context.Context, sql string) error
}

// abortOptions configures abortResumables. table and index are the target filters;
// all is the explicit opt-out from targeting. One of the three must be set — see
// parseAbortFlags for why.
type abortOptions struct {
	dryRun         bool
	includeRunning bool
	all            bool
	table          string // "schema.table" or a bare table name
	index          string
}

// abortSummary counts the outcome of an abort-resumable run.
type abortSummary struct {
	Matched int // operations selected to abort
	Aborted int // actually aborted (0 in dry-run)
	Failed  int
}

// abortResumables lists resumable operations, selects those to cancel (PAUSED,
// plus RUNNING when includeRunning), and aborts each with ALTER INDEX … ABORT. I/O
// goes through the injected interfaces, so the selection and dry-run logic is
// tested with fakes. A failed abort does not stop the others; all failures are
// joined into the returned error.
func abortResumables(ctx context.Context, lister resumableLister, exec ddlExecer, opts abortOptions, out io.Writer) (abortSummary, error) {
	ops, err := lister.ResumableOps(ctx)
	if err != nil {
		return abortSummary{}, fmt.Errorf("list resumable operations: %w", err)
	}

	var sum abortSummary
	var failures []error
	for _, op := range ops {
		if !selectedForAbort(op, opts) {
			continue
		}
		sum.Matched++
		target := fmt.Sprintf("%s.%s.%s", op.Schema, op.Table, op.Name)

		if opts.dryRun {
			fmt.Fprintf(out, "  %-52s %-7s %5.1f%% → would abort\n", target, op.StateDesc, op.PercentComplete)
			continue
		}

		stmt, err := ddl.ResumableControlSQL(ddl.RebuildIndex{Schema: op.Schema, Table: op.Table, Index: op.Name}, "ABORT")
		if err == nil {
			err = exec.ExecDDL(ctx, stmt)
		}
		if err != nil {
			sum.Failed++
			failures = append(failures, fmt.Errorf("%s: %w", target, err))
			fmt.Fprintf(out, "  %-52s %-7s %5.1f%% → ERROR: %v\n", target, op.StateDesc, op.PercentComplete, err)
			continue
		}
		sum.Aborted++
		fmt.Fprintf(out, "  %-52s %-7s %5.1f%% → aborted\n", target, op.StateDesc, op.PercentComplete)
	}

	if sum.Matched == 0 {
		fmt.Fprintln(out, "  (no matching resumable operations)")
	}
	if len(failures) > 0 {
		return sum, fmt.Errorf("%d abort(s) failed: %w", len(failures), errors.Join(failures...))
	}
	return sum, nil
}

// selectedForAbort reports whether an operation is in scope: the right state (PAUSED
// always, RUNNING only when explicitly requested) and a matching target.
//
// The target test is the one that was missing. sys.index_resumable_operations is
// database-wide and was read with no WHERE clause, so "all" was the only mode: one
// command on a shared server aborted every colleague's paused index build, and
// SQL Server documents an aborted resumable as unresumable. Ownership cannot be
// consulted — the tool does not know which paused operations it started — so the
// operator has to say.
func selectedForAbort(op mssql.ResumableOp, opts abortOptions) bool {
	switch op.StateDesc {
	case "PAUSED":
	case "RUNNING":
		if !opts.includeRunning {
			return false
		}
	default:
		return false
	}
	return matchesAbortTarget(op, opts)
}

// matchesAbortTarget applies the --table / --index filters, ANDed. --all matches
// everything and is the only way to run without a filter.
func matchesAbortTarget(op mssql.ResumableOp, opts abortOptions) bool {
	if opts.index != "" && !strings.EqualFold(opts.index, op.Name) {
		return false
	}
	if opts.table != "" && !matchesTableName(opts.table, op.Schema, op.Table) {
		return false
	}
	return opts.all || opts.table != "" || opts.index != ""
}

// matchesTableName accepts either "schema.table" or a bare table name, so an operator
// who knows the table but not which schema it is in is not stuck.
func matchesTableName(want, schema, table string) bool {
	if s, t, ok := strings.Cut(want, "."); ok {
		return strings.EqualFold(s, schema) && strings.EqualFold(t, table)
	}
	return strings.EqualFold(want, table)
}

// describeAbortScope renders what the flags selected, so the header states the blast
// radius in the operator's own terms before a single ABORT is issued.
func describeAbortScope(opts abortOptions) string {
	states := "PAUSED"
	if opts.includeRunning {
		states = "PAUSED and RUNNING"
	}
	switch {
	case opts.table != "" && opts.index != "":
		return fmt.Sprintf("%s operations on %s, index %s", states, opts.table, opts.index)
	case opts.table != "":
		return fmt.Sprintf("%s operations on %s", states, opts.table)
	case opts.index != "":
		return fmt.Sprintf("%s operations named %s, in any table", states, opts.index)
	default:
		return fmt.Sprintf("ALL %s operations in the database, including other people's", states)
	}
}

// errAbortHelp signals that -h/--help was handled and the caller should exit quietly.
var errAbortHelp = errors.New("help requested")

// parseAbortFlags parses and gates the abort-resumable flags. Split out from
// runAbortResumable so the gate is testable without a database — it is the whole
// safety of the subcommand.
//
// ALTER INDEX ... ABORT is irreversible: SQL Server cannot resume an aborted index
// operation. The command previously needed nothing but --config to abort every paused
// resumable in the database, with no target, no confirmation and no second gesture,
// while docs/running.md and the engine's own error message send operators here. So a
// target is now required, --all is the explicit way to say "no target", and both --all
// and --include-running need --yes.
//
// --dry-run is deliberately free of all of it: it is the review path, it changes
// nothing, and making it awkward would push operators to the destructive form.
func parseAbortFlags(args []string, stderr io.Writer) (abortOptions, string, error) {
	fs := flag.NewFlagSet("sqlgopace abort-resumable", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath     = fs.String("config", "", "path to config.yaml (provides the connection)")
		dryRun         = fs.Bool("dry-run", false, "list the matching operations without aborting them")
		includeRunning = fs.Bool("include-running", false, "also abort RUNNING operations, killing their sessions (needs --yes)")
		table          = fs.String("table", "", "only this table: \"schema.table\", or a bare table name")
		index          = fs.String("index", "", "only this index, by name")
		all            = fs.Bool("all", false, "every resumable operation in the database (needs --yes)")
		yes            = fs.Bool("yes", false, "confirm a destructive, unresumable abort")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return abortOptions{}, "", errAbortHelp
		}
		return abortOptions{}, "", err
	}
	if *configPath == "" {
		return abortOptions{}, "", errors.New("abort-resumable requires --config (for the connection)")
	}

	opts := abortOptions{
		dryRun: *dryRun, includeRunning: *includeRunning,
		all: *all, table: strings.TrimSpace(*table), index: strings.TrimSpace(*index),
	}
	if !opts.all && opts.table == "" && opts.index == "" {
		return abortOptions{}, "", errors.New(
			"abort-resumable needs a target: --table and/or --index, or --all --yes for every resumable operation in the database. " +
				"An aborted index operation cannot be resumed, and this command cannot tell which ones are yours")
	}
	if opts.all && !*yes && !opts.dryRun {
		return abortOptions{}, "", errors.New(
			"--all aborts every paused resumable index operation in the database, unresumably, including other people's: add --yes, or run with --dry-run first")
	}
	if opts.includeRunning && !*yes && !opts.dryRun {
		return abortOptions{}, "", errors.New(
			"--include-running kills the sessions running those index builds as well as aborting them: add --yes, or run with --dry-run first")
	}
	return opts, *configPath, nil
}

// runAbortResumable is the "abort-resumable" subcommand: it connects via the config
// and aborts the resumable index operations the flags select.
func runAbortResumable(stdout, stderr io.Writer, args []string) error {
	opts, configPath, err := parseAbortFlags(args, stderr)
	if err != nil {
		if errors.Is(err, errAbortHelp) {
			return nil
		}
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	conn, err := mssql.Open(ctx, cfg.Database.ConnectionString, version.Version(), mssql.WithLoginTimeout(cfg.Database.LoginTimeout()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	suffix := ""
	if opts.dryRun {
		suffix = " (dry-run)"
	}
	fmt.Fprintf(stdout, "-- sqlgopace %s — abort-resumable%s\n", version.Version(), suffix)
	// Printed before anything is aborted: the old warning came out after the decision
	// was already made, which is not a warning.
	fmt.Fprintf(stdout, "-- scope: %s\n", describeAbortScope(opts))
	if opts.includeRunning {
		fmt.Fprintln(stdout, "-- WARNING: --include-running kills the sessions running these builds")
	}

	sum, err := abortResumables(ctx, conn, conn, opts, stdout)
	if opts.dryRun {
		fmt.Fprintf(stdout, "matched %d resumable operation(s); none aborted (--dry-run)\n", sum.Matched)
	} else {
		fmt.Fprintf(stdout, "aborted %d, failed %d (of %d matched)\n", sum.Aborted, sum.Failed, sum.Matched)
	}
	return err
}
