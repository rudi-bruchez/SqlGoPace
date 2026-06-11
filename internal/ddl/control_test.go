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
