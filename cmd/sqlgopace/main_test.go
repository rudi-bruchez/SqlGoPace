package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

const (
	exampleManifest = "../../01.to_run/010_example_rebuild.yaml"
	matrixFlag      = "--matrix=../../ddl_compatibility.yaml"
)

func TestRunDryRunEnterprise2022(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--dry-run", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, exampleManifest}

	if err := run(&out, io.Discard, args); err != nil {
		t.Fatalf("run(dry-run) error = %v, want nil", err)
	}

	got := out.String()
	wants := []string{
		"ALTER INDEX [IX_DISPATCH] ON [dbo].[DISPATCH] REBUILD WITH (ONLINE = ON",
		"MAXDOP = 4",
		"DATA_COMPRESSION = PAGE",
		"IF COL_LENGTH(N'[dbo].[DISPATCH]', N'PROCESSED') IS NULL",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("dry-run output missing %q\n--- output ---\n%s", w, got)
		}
	}
}

func TestRunDryRunStandardOmitsOnline(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--dry-run", "--assume-version=16", "--assume-edition=standard", matrixFlag, exampleManifest}

	if err := run(&out, io.Discard, args); err != nil {
		t.Fatalf("run(dry-run standard) error = %v, want nil", err)
	}
	if strings.Contains(out.String(), "ONLINE = ON") {
		t.Errorf("Standard edition output should not inject ONLINE:\n%s", out.String())
	}
}

func TestRunDryRunWithConfigPolicy(t *testing.T) {
	var out bytes.Buffer
	// Offline target (assume flags) but policy comes from --config, which forces
	// ONLINE off; the matrix path is taken from the config file.
	args := []string{
		"--dry-run", "--assume-version=16", "--assume-edition=enterprise",
		"--config=testdata/config_force_online_off.yaml", exampleManifest,
	}

	if err := run(&out, io.Discard, args); err != nil {
		t.Fatalf("run(config policy) error = %v, want nil", err)
	}
	if strings.Contains(out.String(), "ONLINE = ON") {
		t.Errorf("config forced ONLINE off, but output still injects it:\n%s", out.String())
	}
}

func TestRunExplain(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--dry-run", "--explain", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, exampleManifest}

	if err := run(&out, io.Discard, args); err != nil {
		t.Fatalf("run(explain) error = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "online = ON") {
		t.Errorf("explain output missing option decision trail:\n%s", out.String())
	}
}

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out, io.Discard, []string{"--version"}); err != nil {
		t.Fatalf("run(--version) error = %v, want nil", err)
	}
	if !strings.HasPrefix(out.String(), "sqlgopace ") {
		t.Errorf("run(--version) = %q, want it to start with 'sqlgopace '", out.String())
	}
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing version on non-azure", []string{"--dry-run", "--assume-edition=enterprise", matrixFlag, exampleManifest}},
		{"no manifests", []string{"--dry-run", "--assume-version=16", matrixFlag}},
		{"bad edition", []string{"--dry-run", "--assume-version=16", "--assume-edition=bogus", matrixFlag, exampleManifest}},
		{"missing matrix file", []string{"--dry-run", "--assume-version=16", "--matrix=does-not-exist.yaml", exampleManifest}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := run(io.Discard, io.Discard, tt.args); err == nil {
				t.Errorf("run(%v) error = nil, want non-nil", tt.args)
			}
		})
	}
}
