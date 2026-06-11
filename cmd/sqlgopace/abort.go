package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

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

// abortOptions configures abortResumables.
type abortOptions struct {
	dryRun         bool
	includeRunning bool
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

// selectedForAbort reports whether an operation is in scope: PAUSED always, and
// RUNNING only when explicitly requested.
func selectedForAbort(op mssql.ResumableOp, opts abortOptions) bool {
	switch op.StateDesc {
	case "PAUSED":
		return true
	case "RUNNING":
		return opts.includeRunning
	default:
		return false
	}
}

// runAbortResumable is the "abort-resumable" subcommand: it connects via the
// config and aborts the database's paused resumable index operations.
func runAbortResumable(stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("sqlgopace abort-resumable", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath     = fs.String("config", "", "path to config.yaml (provides the connection)")
		dryRun         = fs.Bool("dry-run", false, "list the resumable operations without aborting them")
		includeRunning = fs.Bool("include-running", false, "also abort RUNNING operations (default: PAUSED only)")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *configPath == "" {
		return errors.New("abort-resumable requires --config (for the connection)")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	conn, err := mssql.Open(ctx, cfg.Database.ConnectionString, version.Version())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	suffix := ""
	if *dryRun {
		suffix = " (dry-run)"
	}
	fmt.Fprintf(stdout, "-- sqlgopace %s — abort-resumable%s\n", version.Version(), suffix)
	if *includeRunning {
		fmt.Fprintln(stdout, "-- WARNING: --include-running also aborts RUNNING operations")
	}

	sum, err := abortResumables(ctx, conn, conn, abortOptions{dryRun: *dryRun, includeRunning: *includeRunning}, stdout)
	if *dryRun {
		fmt.Fprintf(stdout, "matched %d resumable operation(s); none aborted (--dry-run)\n", sum.Matched)
	} else {
		fmt.Fprintf(stdout, "aborted %d, failed %d (of %d matched)\n", sum.Aborted, sum.Failed, sum.Matched)
	}
	return err
}
