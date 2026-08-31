package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/rudi-bruchez/SqlGoPace/internal/scaffold"
	"github.com/rudi-bruchez/SqlGoPace/internal/version"
)

// runInit is the "init" subcommand: it lays down a working configuration and the
// queue directories, so that a freshly downloaded binary has something to run
// against. It is deliberately safe to re-run: an existing file is reported and
// left alone unless --force is given.
func runInit(stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("sqlgopace init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dir   = fs.String("dir", ".", "directory to initialize")
		force = fs.Bool("force", false, "overwrite files that already exist")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("init takes no arguments; use --dir to choose the target directory (got %q)", fs.Arg(0))
	}

	results, err := scaffold.Write(*dir, *force)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "-- sqlgopace %s — init %s\n", version.Version(), *dir)
	skipped := 0
	for _, r := range results {
		if r.Created {
			fmt.Fprintf(stdout, "  created  %s\n", r.Path)
			continue
		}
		skipped++
		fmt.Fprintf(stdout, "  exists   %s\n", r.Path)
	}
	if skipped > 0 && !*force {
		fmt.Fprintf(stdout, "\n%d item(s) already existed and were left untouched (--force overwrites).\n", skipped)
	}

	fmt.Fprintf(stdout, `
Next:
  1. write a manifest into 01.to_run/, or arm the disabled example by copying it
     without its leading dot:
       cp %s 01.to_run/010_rebuild.yaml
  2. check the generated T-SQL without a server, which also proves the install:
       sqlgopace --dry-run --assume-version 16 --explain 01.to_run/010_rebuild.yaml
  3. cp .env.example .env    and fill in DB_SERVER, DB_NAME, DB_USER, DB_PASSWORD
  4. edit config.yaml        (directories and thresholds; secrets stay in .env)
  5. the same preview against the real target, which connects and detects it:
       sqlgopace --config config.yaml --dry-run 01.to_run/010_rebuild.yaml
  6. sqlgopace --config config.yaml

Documentation: https://github.com/rudi-bruchez/SqlGoPace/blob/main/docs/getting-started.md
`, filepath.Join("01.to_run", ".010_example_rebuild.yaml"))
	return nil
}
