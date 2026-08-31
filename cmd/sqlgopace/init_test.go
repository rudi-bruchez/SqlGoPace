package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitScaffoldsThenDryRunsOffline is the end-to-end promise of the
// subcommand: after `init`, an armed manifest renders T-SQL with no server and
// no further setup. It is the offline dry run the printed instructions lead
// with, and the reason they lead with it.
func TestInitScaffoldsThenDryRunsOffline(t *testing.T) {
	dir := t.TempDir()

	var out, errOut bytes.Buffer
	if err := cli(&out, &errOut, []string{"init", "--dir", dir}); err != nil {
		t.Fatalf("init: %v (stderr %s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "created  config.yaml") {
		t.Errorf("init did not report config.yaml:\n%s", out.String())
	}

	example := filepath.Join(dir, "01.to_run", ".010_example_rebuild.yaml")
	armed := filepath.Join(dir, "01.to_run", "010_rebuild.yaml")
	body, err := os.ReadFile(example)
	if err != nil {
		t.Fatalf("read scaffolded example: %v", err)
	}
	if err := os.WriteFile(armed, body, 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	err = cli(&out, &errOut, []string{
		"--dry-run", "--assume-version", "16",
		"--matrix", filepath.Join(dir, "ddl_compatibility.yaml"),
		armed,
	})
	if err != nil {
		t.Fatalf("dry run on the scaffolded tree: %v (stderr %s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "ALTER INDEX") {
		t.Errorf("dry run produced no DDL:\n%s", out.String())
	}
}

func TestInitRejectsPositionalArgument(t *testing.T) {
	var out, errOut bytes.Buffer
	err := cli(&out, &errOut, []string{"init", t.TempDir()})
	if err == nil {
		t.Fatal("init accepted a positional argument; --dir is the only way to pick a directory")
	}
	if !strings.Contains(err.Error(), "--dir") {
		t.Errorf("error does not point at --dir: %v", err)
	}
}
