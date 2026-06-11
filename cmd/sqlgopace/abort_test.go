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

	sum, err := abortResumables(context.Background(), lister, exec, abortOptions{}, io.Discard)
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

	sum, err := abortResumables(context.Background(), lister, exec, abortOptions{dryRun: true}, &out)
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

	sum, _ := abortResumables(context.Background(), lister, exec, abortOptions{includeRunning: true}, io.Discard)
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

	sum, err := abortResumables(context.Background(), lister, exec, abortOptions{}, io.Discard)
	if err == nil {
		t.Fatal("expected an aggregated error when an abort fails")
	}
	if sum.Matched != 2 || sum.Aborted != 1 || sum.Failed != 1 {
		t.Errorf("summary = %+v, want Matched:2 Aborted:1 Failed:1", sum)
	}
}
