// Command sqlgopace runs demanding DDL operations against Microsoft SQL Server
// while monitoring their impact on locking and the transaction log.
//
// See specs/SPECS.md for the full behaviour and specs/IMPLEMENTATION.md for the
// implementation plan. So far only offline planning (--dry-run / --explain) is
// wired; execution and monitoring are added in later phases.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// version is the build version, overridden at release time via -ldflags.
var version = "dev"

func main() {
	if err := run(os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sqlgopace:", err)
		os.Exit(1)
	}
}

// run is the testable entry point; main only wires it to the process streams.
func run(stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("sqlgopace", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dryRun        = fs.Bool("dry-run", false, "render the final T-SQL without executing anything")
		explain       = fs.Bool("explain", false, "with --dry-run, show why each option was injected")
		assumeVersion = fs.Int("assume-version", 0, "target SQL Server major version for offline dry-run (e.g. 16 for 2022)")
		assumeEdition = fs.String("assume-edition", "enterprise", "target edition tier: enterprise, standard, express, azure")
		matrixPath    = fs.String("matrix", "ddl_compatibility.yaml", "path to the DDL compatibility matrix")
		showVersion   = fs.Bool("version", false, "print version and exit")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if *showVersion {
		fmt.Fprintf(stdout, "sqlgopace %s\n", version)
		return nil
	}
	if !*dryRun {
		return errors.New("only --dry-run is implemented so far; pass --dry-run")
	}

	target, err := offlineTarget(*assumeVersion, *assumeEdition)
	if err != nil {
		return err
	}
	matrix, err := ddl.LoadFile(*matrixPath)
	if err != nil {
		return err
	}
	manifests := fs.Args()
	if len(manifests) == 0 {
		return errors.New("no manifest files given")
	}

	for _, path := range manifests {
		if err := dryRunManifest(stdout, path, target, matrix, ddl.Policy{}, *explain); err != nil {
			return err
		}
	}
	return nil
}

// offlineTarget builds a resolution target from the --assume-* flags.
func offlineTarget(major int, edition string) (ddl.Target, error) {
	tier, err := ddl.ParseTier(edition)
	if err != nil {
		return ddl.Target{}, fmt.Errorf("invalid --assume-edition: %w", err)
	}
	if major <= 0 && tier != ddl.TierAzure {
		return ddl.Target{}, errors.New("--assume-version is required for dry-run (e.g. 16 for SQL Server 2022)")
	}
	return ddl.Target{MajorVersion: major, Tier: tier}, nil
}

// dryRunManifest loads, plans, and renders one manifest to w.
func dryRunManifest(w io.Writer, path string, target ddl.Target, matrix *ddl.Matrix, policy ddl.Policy, explain bool) error {
	manifest, err := ddl.LoadManifestFile(path)
	if err != nil {
		return err
	}
	planned, err := ddl.Plan(manifest, target, matrix, policy)
	if err != nil {
		return err
	}
	renderPlan(w, path, manifest, planned, explain)
	return nil
}

// renderPlan prints a manifest's planned operations as runnable, commented T-SQL.
func renderPlan(w io.Writer, source string, manifest *ddl.Manifest, planned []ddl.PlannedOperation, explain bool) {
	if manifest.Description != "" {
		fmt.Fprintf(w, "-- manifest: %s — %s\n", source, manifest.Description)
	} else {
		fmt.Fprintf(w, "-- manifest: %s\n", source)
	}

	for i, step := range planned {
		ref := step.Operation.Target()
		fmt.Fprintf(w, "-- [%d] %s %s.%s.%s\n",
			i+1, step.Operation.CommandType(), ref.Schema, ref.Table, ref.Name)
		fmt.Fprintln(w, step.SQL)
		if explain {
			for _, d := range step.Decisions {
				fmt.Fprintf(w, "--     %s = %s  (%s)\n", d.Option, d.Value, d.Reason)
			}
		}
		fmt.Fprintln(w)
	}
}
