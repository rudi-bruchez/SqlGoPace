package ddl_test

import (
	"errors"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestResumableControlSQL(t *testing.T) {
	tests := []struct {
		name   string
		op     ddl.Operation
		action string
		want   string
	}{
		{"rebuild pause", ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}, "PAUSE", "ALTER INDEX [IX] ON [dbo].[T] PAUSE;"},
		{"create resume", ddl.CreateIndex{Schema: "dbo", Table: "T", Index: "IX", Columns: []string{"C"}}, "RESUME", "ALTER INDEX [IX] ON [dbo].[T] RESUME;"},
		{"rebuild abort escapes ]", ddl.RebuildIndex{Schema: "dbo", Table: "weird]name", Index: "IX]1"}, "ABORT", "ALTER INDEX [IX]]1] ON [dbo].[weird]]name] ABORT;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ddl.ResumableControlSQL(tt.op, tt.action)
			if err != nil {
				t.Fatalf("ResumableControlSQL() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ResumableControlSQL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResumableControlSQLUnsupported(t *testing.T) {
	_, err := ddl.ResumableControlSQL(ddl.AddColumn{Schema: "dbo", Table: "T", Column: "C", DataType: "BIT"}, "PAUSE")
	if !errors.Is(err, ddl.ErrUnsupportedOperation) {
		t.Errorf("error = %v, want ErrUnsupportedOperation", err)
	}
}

// A paused rebuild resumed with a bare RESUME takes its final locks at NORMAL priority.
// Microsoft documents that omitting WAIT_AT_LOW_PRIORITY on RESUME is equivalent to
// WAIT_AT_LOW_PRIORITY (MAX_DURATION = 0 minutes, ABORT_AFTER_WAIT = NONE), and NONE is
// "continue waiting for the lock with normal priority". So a manifest that asked to yield
// stops yielding from its first pause onward, which is every pause the pressure loop takes.
func TestResumeSQLCarriesWaitAtLowPriority(t *testing.T) {
	res := ddl.ResolvedOptions{
		Online: true, Resumable: true, WaitAtLowPriority: true,
		AbortAfterWait: "SELF", MaxDurationMinutes: 1,
	}
	got, err := ddl.ResumeSQL(ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}, res)
	if err != nil {
		t.Fatalf("ResumeSQL() error = %v", err)
	}
	want := "ALTER INDEX [IX] ON [dbo].[T] RESUME WITH (WAIT_AT_LOW_PRIORITY (MAX_DURATION = 1 MINUTES, ABORT_AFTER_WAIT = SELF));"
	if got != want {
		t.Errorf("ResumeSQL() = %q, want %q", got, want)
	}
}

// An operation that never asked for low-priority locks must not acquire the clause by
// accident: the bare form is what it asked for.
func TestResumeSQLWithoutLowPriorityIsBare(t *testing.T) {
	got, err := ddl.ResumeSQL(ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}, ddl.ResolvedOptions{Resumable: true})
	if err != nil {
		t.Fatalf("ResumeSQL() error = %v", err)
	}
	if want := "ALTER INDEX [IX] ON [dbo].[T] RESUME;"; got != want {
		t.Errorf("ResumeSQL() = %q, want %q", got, want)
	}
}

func TestResumeSQLUnsupported(t *testing.T) {
	_, err := ddl.ResumeSQL(ddl.AddColumn{Schema: "dbo", Table: "T", Column: "C", DataType: "BIT"}, ddl.ResolvedOptions{})
	if !errors.Is(err, ddl.ErrUnsupportedOperation) {
		t.Errorf("error = %v, want ErrUnsupportedOperation", err)
	}
}
