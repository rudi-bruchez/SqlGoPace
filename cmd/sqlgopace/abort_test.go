package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

type fakeLister struct {
	ops []mssql.ResumableOp
	err error
}

func (f fakeLister) ResumableOps(context.Context) ([]mssql.ResumableOp, error) {
	return f.ops, f.err
}

type fakeExecer struct {
	stmts  []string
	failOn string // ExecDDL errors when the statement contains this substring
}

func (f *fakeExecer) ExecDDL(_ context.Context, sql string) error {
	f.stmts = append(f.stmts, sql)
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		return errors.New("boom")
	}
	return nil
}

func TestAbortResumablesPausedOnly(t *testing.T) {
	lister := fakeLister{ops: []mssql.ResumableOp{
		{Schema: "dbo", Table: "T", Name: "PK_T", StateDesc: "PAUSED", PercentComplete: 31.7},
		{Schema: "dbo", Table: "T", Name: "IX_run", StateDesc: "RUNNING"}, // skipped by default
	}}
	exec := &fakeExecer{}

	sum, err := abortResumables(context.Background(), lister, exec, abortOptions{all: true}, io.Discard)
	if err != nil {
		t.Fatalf("abortResumables() error = %v", err)
	}
	if sum.Matched != 1 || sum.Aborted != 1 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want Matched:1 Aborted:1 Failed:0", sum)
	}
	if len(exec.stmts) != 1 || !strings.Contains(exec.stmts[0], "[PK_T]") || !strings.Contains(exec.stmts[0], "ABORT") {
		t.Errorf("stmts = %v, want one ABORT on [PK_T]", exec.stmts)
	}
}

func TestAbortResumablesDryRun(t *testing.T) {
	lister := fakeLister{ops: []mssql.ResumableOp{
		{Schema: "dbo", Table: "T", Name: "PK_T", StateDesc: "PAUSED"},
	}}
	exec := &fakeExecer{}
	var out strings.Builder

	sum, err := abortResumables(context.Background(), lister, exec, abortOptions{all: true, dryRun: true}, &out)
	if err != nil {
		t.Fatalf("abortResumables() error = %v", err)
	}
	if sum.Matched != 1 || sum.Aborted != 0 {
		t.Errorf("summary = %+v, want Matched:1 Aborted:0", sum)
	}
	if len(exec.stmts) != 0 {
		t.Errorf("dry-run executed %v, want nothing", exec.stmts)
	}
	if !strings.Contains(out.String(), "would abort") {
		t.Errorf("output missing 'would abort'\n%s", out.String())
	}
}

func TestAbortResumablesIncludeRunning(t *testing.T) {
	lister := fakeLister{ops: []mssql.ResumableOp{
		{Schema: "dbo", Table: "T", Name: "PK_T", StateDesc: "PAUSED"},
		{Schema: "dbo", Table: "T", Name: "IX_run", StateDesc: "RUNNING"},
	}}
	exec := &fakeExecer{}

	sum, _ := abortResumables(context.Background(), lister, exec, abortOptions{all: true, includeRunning: true}, io.Discard)
	if sum.Matched != 2 || sum.Aborted != 2 {
		t.Errorf("summary = %+v, want Matched:2 Aborted:2", sum)
	}
}

func TestAbortResumablesContinuesOnError(t *testing.T) {
	lister := fakeLister{ops: []mssql.ResumableOp{
		{Schema: "dbo", Table: "T", Name: "IX_bad", StateDesc: "PAUSED"},
		{Schema: "dbo", Table: "T", Name: "IX_ok", StateDesc: "PAUSED"},
	}}
	exec := &fakeExecer{failOn: "[IX_bad]"}

	sum, err := abortResumables(context.Background(), lister, exec, abortOptions{all: true}, io.Discard)
	if err == nil {
		t.Fatal("expected an aggregated error when an abort fails")
	}
	if sum.Matched != 2 || sum.Aborted != 1 || sum.Failed != 1 {
		t.Errorf("summary = %+v, want Matched:2 Aborted:1 Failed:1", sum)
	}
}

// TestSelectedForAbortFilters pins the target filter. Without one the command
// selected every paused resumable in the database, which on a shared server means
// every colleague's in-flight index build — and Microsoft documents an aborted
// resumable as unresumable, so the work is gone.
func TestSelectedForAbortFilters(t *testing.T) {
	mine := mssql.ResumableOp{Schema: "dbo", Table: "MEASUREMENT", Name: "PK_MEASUREMENT", StateDesc: "PAUSED"}
	theirs := mssql.ResumableOp{Schema: "sales", Table: "Invoice", Name: "IX_Invoice_Date", StateDesc: "PAUSED"}

	tests := []struct {
		name string
		opts abortOptions
		op   mssql.ResumableOp
		want bool
	}{
		{"all selects everything", abortOptions{all: true}, theirs, true},
		{"table filter selects its own", abortOptions{table: "dbo.MEASUREMENT"}, mine, true},
		{"table filter excludes others", abortOptions{table: "dbo.MEASUREMENT"}, theirs, false},
		{"table filter is case-insensitive", abortOptions{table: "DBO.measurement"}, mine, true},
		{"bare table name matches any schema", abortOptions{table: "MEASUREMENT"}, mine, true},
		{"index filter selects its own", abortOptions{index: "PK_MEASUREMENT"}, mine, true},
		{"index filter excludes others", abortOptions{index: "PK_MEASUREMENT"}, theirs, false},
		{"filters are ANDed", abortOptions{table: "dbo.MEASUREMENT", index: "IX_Invoice_Date"}, mine, false},
		{"running still needs include-running", abortOptions{all: true},
			mssql.ResumableOp{Schema: "dbo", Table: "T", Name: "IX", StateDesc: "RUNNING"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectedForAbort(tt.op, tt.opts); got != tt.want {
				t.Errorf("selectedForAbort(%s.%s) = %v, want %v", tt.op.Table, tt.op.Name, got, tt.want)
			}
		})
	}
}

// TestParseAbortFlags pins the gate. The command is irreversible and was reachable
// with no target, no confirmation and no second gesture; --dry-run stays free of
// ceremony because it is the review path the operator should be using first.
func TestParseAbortFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string // substring; empty means it must parse
	}{
		{"bare invocation is refused", []string{"--config", "c.yaml"}, "--table"},
		{"a table target is enough", []string{"--config", "c.yaml", "--table", "dbo.T"}, ""},
		{"an index target is enough", []string{"--config", "c.yaml", "--index", "PK_T"}, ""},
		{"all needs yes", []string{"--config", "c.yaml", "--all"}, "--yes"},
		{"all with yes is allowed", []string{"--config", "c.yaml", "--all", "--yes"}, ""},
		{"all in dry-run needs no yes", []string{"--config", "c.yaml", "--all", "--dry-run"}, ""},
		{"include-running needs yes", []string{"--config", "c.yaml", "--table", "dbo.T", "--include-running"}, "--yes"},
		{"include-running in dry-run needs no yes", []string{"--config", "c.yaml", "--table", "dbo.T", "--include-running", "--dry-run"}, ""},
		{"config is still required", []string{"--table", "dbo.T"}, "--config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseAbortFlags(tt.args, io.Discard)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("parseAbortFlags(%v) = %v, want no error", tt.args, err)
			case tt.wantErr != "" && err == nil:
				t.Errorf("parseAbortFlags(%v) succeeded, want an error mentioning %q", tt.args, tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("parseAbortFlags(%v) = %v, want it to mention %q", tt.args, err, tt.wantErr)
			}
		})
	}
}
