package ddl_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// resolveMatrix mirrors the real rules for the operations exercised here.
func resolveMatrix() *ddl.Matrix {
	ent := []ddl.Tier{ddl.TierEnterprise, ddl.TierAzure}
	return &ddl.Matrix{
		AzurePseudoMajor: 9999,
		Commands: map[string]ddl.CommandRules{
			"rebuild_index": {
				"online":               {MinMajor: 9, Editions: ent},
				"wait_at_low_priority": {MinMajor: 12, Editions: ent, Requires: []string{"online"}},
				"resumable":            {MinMajor: 14, Editions: ent, Requires: []string{"online"}},
				"sort_in_tempdb":       {MinMajor: 9, Editions: []ddl.Tier{ddl.TierEnterprise, ddl.TierStandard, ddl.TierAzure}},
				"maxdop":               {MinMajor: 9, Editions: []ddl.Tier{ddl.TierEnterprise, ddl.TierStandard, ddl.TierAzure}},
			},
		},
	}
}

func decisionValue(decisions []ddl.Decision, option string) (string, bool) {
	for _, d := range decisions {
		if d.Option == option {
			return d.Value, true
		}
	}
	return "", false
}

func TestResolveAutoEnterprise2022(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, decisions := ddl.Resolve(op, target, m, ddl.Policy{})

	want := ddl.ResolvedOptions{
		Online:             true,
		Resumable:          true,
		WaitAtLowPriority:  true,
		AbortAfterWait:     "SELF",
		MaxDurationMinutes: 1,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Resolve() mismatch (-want +got):\n%s", diff)
	}
	if len(decisions) == 0 {
		t.Errorf("Resolve() returned no decisions, want an explanation trail")
	}
	if v, _ := decisionValue(decisions, "online"); v != "ON" {
		t.Errorf("decision[online] = %q, want ON", v)
	}
}

func TestResolveEnterprise2016DropsResumable(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}
	target := ddl.Target{MajorVersion: 13, Tier: ddl.TierEnterprise}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{})

	if !got.Online {
		t.Errorf("Online = false, want true (supported since 2005)")
	}
	if !got.WaitAtLowPriority {
		t.Errorf("WaitAtLowPriority = false, want true (supported since 2014)")
	}
	if got.Resumable {
		t.Errorf("Resumable = true, want false (not supported before 2017)")
	}
}

func TestResolveSinglePartitionRebuildDropsResumable(t *testing.T) {
	m := resolveMatrix()
	// A single-partition rebuild: ONLINE and WALP are permitted, but RESUMABLE is
	// not (single-partition syntax does not accept it).
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX", Partition: intPtr(3)}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, decisions := ddl.Resolve(op, target, m, ddl.Policy{})

	if got.Resumable {
		t.Errorf("Resumable = true, want false (not supported when rebuilding a single partition)")
	}
	if !got.Online || !got.WaitAtLowPriority {
		t.Errorf("Online/WALP = %t/%t, want both true (permitted for single-partition rebuild)",
			got.Online, got.WaitAtLowPriority)
	}
	if v, ok := decisionValue(decisions, "resumable"); !ok || v != "OFF" {
		t.Errorf("decision[resumable] = %q (present=%t), want OFF with an explanation", v, ok)
	}
}

func TestResolveStandardOmitsEnterpriseOptions(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierStandard}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{})

	if got.Online || got.Resumable || got.WaitAtLowPriority {
		t.Errorf("Online/Resumable/WALP = %t/%t/%t, want all false on Standard",
			got.Online, got.Resumable, got.WaitAtLowPriority)
	}
}

func TestResolvePerOpOverrideOnlineOffCascades(t *testing.T) {
	m := resolveMatrix()
	// Forcing online off must drop resumable and WALP (both require online).
	op := ddl.RebuildIndex{
		Schema: "dbo", Table: "T", Index: "IX",
		Options: ddl.OptionOverrides{Online: boolPtr(false)},
	}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{})

	if got.Online || got.Resumable || got.WaitAtLowPriority {
		t.Errorf("with online forced off: got Online=%t Resumable=%t WALP=%t, want all false",
			got.Online, got.Resumable, got.WaitAtLowPriority)
	}
}

func TestResolveForcedUnsupportedIsOmitted(t *testing.T) {
	m := resolveMatrix()
	// Config forces resumable on, but target is Standard → must be omitted.
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierStandard}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{Resumable: boolPtr(true)})

	if got.Resumable {
		t.Errorf("Resumable = true, want false (forced but unsupported on Standard)")
	}
}

func TestResolveMaxDOPAndAbortBlockers(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{
		Schema: "dbo", Table: "T", Index: "IX",
		Options: ddl.OptionOverrides{MaxDOP: intPtr(4)},
	}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{AllowAbortBlockers: true, WaitMaxDurationMinutes: 5})

	if got.MaxDOP == nil || *got.MaxDOP != 4 {
		t.Errorf("MaxDOP = %v, want 4", got.MaxDOP)
	}
	if got.AbortAfterWait != "BLOCKERS" {
		t.Errorf("AbortAfterWait = %q, want BLOCKERS (allowed by policy)", got.AbortAfterWait)
	}
	if got.MaxDurationMinutes != 5 {
		t.Errorf("MaxDurationMinutes = %d, want 5", got.MaxDurationMinutes)
	}
}

func TestResolveNoOptionsForPlainOperation(t *testing.T) {
	m := resolveMatrix()
	op := ddl.AddColumn{Schema: "dbo", Table: "T", Column: "C", DataType: "BIT"}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{})

	if got != (ddl.ResolvedOptions{}) {
		t.Errorf("Resolve(add_column) = %+v, want zero ResolvedOptions", got)
	}
}
