package ddl_test

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestPlan(t *testing.T) {
	m := resolveMatrix()
	manifest := &ddl.Manifest{
		Operations: []ddl.Operation{
			ddl.RebuildIndex{Schema: "dbo", Table: "DISPATCH", Index: "IX_DISPATCH", DataCompression: "PAGE"},
		},
	}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	planned, err := ddl.Plan(manifest, target, m, ddl.Policy{})
	if err != nil {
		t.Fatalf("Plan() error = %v, want nil", err)
	}
	if got, want := len(planned), 1; got != want {
		t.Fatalf("len(planned) = %d, want %d", got, want)
	}

	step := planned[0]
	if !strings.Contains(step.SQL, "REBUILD WITH (ONLINE = ON") {
		t.Errorf("planned SQL = %q, want it to contain the injected ONLINE option", step.SQL)
	}
	if !strings.Contains(step.SQL, "DATA_COMPRESSION = PAGE") {
		t.Errorf("planned SQL = %q, want it to carry DATA_COMPRESSION = PAGE", step.SQL)
	}
	if len(step.Decisions) == 0 {
		t.Errorf("planned Decisions empty, want an explanation trail")
	}
	if step.Operation != manifest.Operations[0] {
		t.Errorf("planned Operation = %+v, want the source operation", step.Operation)
	}
}
