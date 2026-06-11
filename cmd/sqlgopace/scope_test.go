package main

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func scopeProfile(t *testing.T, yaml string) *maint.Profile {
	t.Helper()
	p, err := maint.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return p
}

func TestResolveDatabases(t *testing.T) {
	all := []mssql.DatabaseInfo{
		{Name: "APP1", Eligible: true},
		{Name: "APP2", Eligible: true},
		{Name: "RO", Eligible: false, IneligibleReason: "read-only"},
		{Name: "APP_ARCHIVE", Eligible: true},
	}
	plain := scopeProfile(t, "{}\n")
	excludeArchive := scopeProfile(t, "scope:\n  databases:\n    exclude: ['*_ARCHIVE']\n")

	t.Run("no flag → connected database only", func(t *testing.T) {
		sel, skip := resolveDatabases(all, false, nil, "CONN", plain)
		if diff := cmp.Diff([]string{"CONN"}, sel); diff != "" {
			t.Errorf("selected mismatch (-want +got):\n%s", diff)
		}
		if len(skip) != 0 {
			t.Errorf("skipped = %+v, want none", skip)
		}
	})

	t.Run("all → eligible selected, ineligible skipped with reason", func(t *testing.T) {
		sel, skip := resolveDatabases(all, true, nil, "CONN", plain)
		if diff := cmp.Diff([]string{"APP1", "APP2", "APP_ARCHIVE"}, sel); diff != "" {
			t.Errorf("selected mismatch (-want +got):\n%s", diff)
		}
		if len(skip) != 1 || skip[0].name != "RO" || skip[0].reason != "read-only" {
			t.Errorf("skipped = %+v, want [{RO read-only}]", skip)
		}
	})

	t.Run("all ∩ scope: excluded dropped silently", func(t *testing.T) {
		sel, skip := resolveDatabases(all, true, nil, "CONN", excludeArchive)
		if diff := cmp.Diff([]string{"APP1", "APP2"}, sel); diff != "" {
			t.Errorf("selected mismatch (-want +got):\n%s", diff)
		}
		// APP_ARCHIVE is excluded by scope (not a skip); RO is ineligible (a skip).
		if len(skip) != 1 || skip[0].name != "RO" {
			t.Errorf("skipped = %+v, want only [{RO …}]", skip)
		}
	})

	t.Run("explicit wins over scope; eligibility still enforced; unknown reported", func(t *testing.T) {
		sel, skip := resolveDatabases(all, false, []string{"app1", "RO", "NOPE"}, "CONN", excludeArchive)
		if diff := cmp.Diff([]string{"APP1"}, sel); diff != "" { // canonical name, case-insensitive match
			t.Errorf("selected mismatch (-want +got):\n%s", diff)
		}
		if len(skip) != 2 {
			t.Fatalf("skipped = %+v, want 2 (RO ineligible, NOPE unknown)", skip)
		}
		if skip[0].name != "RO" || skip[0].reason != "read-only" {
			t.Errorf("skip[0] = %+v, want RO read-only", skip[0])
		}
		if skip[1].name != "NOPE" {
			t.Errorf("skip[1] = %+v, want NOPE not-found", skip[1])
		}
	})
}

func TestRunnableTargets(t *testing.T) {
	eligible := []mssql.DatabaseInfo{
		{Name: "DB1", Eligible: true},
		{Name: "DB2", Eligible: false, IneligibleReason: "availability-group secondary"},
	}
	// Targets: connected (always runs), DB1 (eligible), DB2 (failed over → skip),
	// DB3 (absent from the sweep → run, let the engine surface any error).
	run, skip := runnableTargets([]string{"CONN", "DB1", "DB2", "DB3"}, "CONN", eligible)

	if diff := cmp.Diff([]string{"CONN", "DB1", "DB3"}, run); diff != "" {
		t.Errorf("run mismatch (-want +got):\n%s", diff)
	}
	if len(skip) != 1 || skip[0].name != "DB2" || !strings.Contains(skip[0].reason, "secondary") {
		t.Errorf("skip = %+v, want only DB2 (no longer eligible)", skip)
	}
}

func TestNeedsEligibilityRecheck(t *testing.T) {
	if needsEligibilityRecheck([]string{"CONN"}, "conn") {
		t.Errorf("single connected target should not need a re-check")
	}
	if !needsEligibilityRecheck([]string{"CONN", "DB2"}, "CONN") {
		t.Errorf("a non-connected target should need a re-check")
	}
}

func TestSplitDatabases(t *testing.T) {
	if got := splitDatabases(""); got != nil {
		t.Errorf("splitDatabases(\"\") = %v, want nil", got)
	}
	if diff := cmp.Diff([]string{"a", "b", "c"}, splitDatabases(" a, b ,c ,")); diff != "" {
		t.Errorf("splitDatabases mismatch (-want +got):\n%s", diff)
	}
}
