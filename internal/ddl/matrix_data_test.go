package ddl_test

import (
	"path/filepath"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// matrixPath points at the canonical data file shipped at the repository root.
const matrixPath = "../../ddl_compatibility.yaml"

// TestShippedMatrixParses guards the real ddl_compatibility.yaml: it must parse
// (strict, unknown fields rejected) and encode the documented capability facts.
func TestShippedMatrixParses(t *testing.T) {
	m, err := ddl.LoadFile(filepath.FromSlash(matrixPath))
	if err != nil {
		t.Fatalf("LoadFile(%q) error = %v, want nil", matrixPath, err)
	}

	facts := []struct {
		name    string
		major   int
		tier    ddl.Tier
		command string
		option  string
		want    bool
	}{
		// rebuild_index RESUMABLE introduced in 2017 (major 14).
		{"rebuild resumable 2016 no", 13, ddl.TierEnterprise, "rebuild_index", "resumable", false},
		{"rebuild resumable 2017 yes", 14, ddl.TierEnterprise, "rebuild_index", "resumable", true},
		// create_index WAIT_AT_LOW_PRIORITY introduced in 2022 (major 16).
		{"create walp 2019 no", 15, ddl.TierEnterprise, "create_index", "wait_at_low_priority", false},
		{"create walp 2022 yes", 16, ddl.TierEnterprise, "create_index", "wait_at_low_priority", true},
		// ALTER COLUMN ONLINE introduced in 2016 (major 13), NOT 2022.
		{"alter_column online 2014 no", 12, ddl.TierEnterprise, "alter_column", "online", false},
		{"alter_column online 2016 yes", 13, ddl.TierEnterprise, "alter_column", "online", true},
		// WAIT_AT_LOW_PRIORITY is NOT supported with ONLINE ALTER COLUMN, any version.
		{"alter_column walp never", 16, ddl.TierEnterprise, "alter_column", "wait_at_low_priority", false},
		// Online DDL is Enterprise/Azure only — Standard is blocked.
		{"rebuild online standard blocked", 16, ddl.TierStandard, "rebuild_index", "online", false},
		// Azure is evergreen.
		{"azure resumable yes", 0, ddl.TierAzure, "rebuild_index", "resumable", true},
		// check_db MAXDOP introduced in 2014 (major 12); available on Standard too.
		{"checkdb maxdop 2012 no", 11, ddl.TierStandard, "check_db", "maxdop", false},
		{"checkdb maxdop 2014 yes", 12, ddl.TierStandard, "check_db", "maxdop", true},
		// rebuild_heap ONLINE introduced in 2016 (major 13), Enterprise/Azure only.
		{"heap online 2014 no", 12, ddl.TierEnterprise, "rebuild_heap", "online", false},
		{"heap online 2016 yes", 13, ddl.TierEnterprise, "rebuild_heap", "online", true},
		{"heap online standard blocked", 16, ddl.TierStandard, "rebuild_heap", "online", false},
		// rebuild_heap never supports RESUMABLE or WAIT_AT_LOW_PRIORITY.
		{"heap resumable never", 16, ddl.TierEnterprise, "rebuild_heap", "resumable", false},
		{"heap walp never", 16, ddl.TierEnterprise, "rebuild_heap", "wait_at_low_priority", false},
	}
	for _, f := range facts {
		t.Run(f.name, func(t *testing.T) {
			got := m.Applicable(f.major, f.tier, f.command, f.option)
			if got != f.want {
				t.Errorf("Applicable(%d, %v, %q, %q) = %t, want %t",
					f.major, f.tier, f.command, f.option, got, f.want)
			}
		})
	}
}
