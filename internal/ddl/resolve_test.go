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
			"shrink_data": {
				"wait_at_low_priority": {MinMajor: 16, Editions: []ddl.Tier{
					ddl.TierEnterprise, ddl.TierStandard, ddl.TierExpress, ddl.TierAzure,
				}},
			},
			"shrink_log": {},
			"shrink_tempdb": {
				"wait_at_low_priority": {MinMajor: 16, Editions: []ddl.Tier{
					ddl.TierEnterprise, ddl.TierStandard, ddl.TierExpress, ddl.TierAzure,
				}},
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

func TestResolveIgnoreBlocking(t *testing.T) {
	m := resolveMatrix()
	// ignore_blocking is a reaction-policy override: it flows through to the
	// resolved options (and a decision) without being a T-SQL WITH option, and is
	// honored even on Standard, where no soft reaction option is available.
	op := ddl.RebuildIndex{
		Schema: "dbo", Table: "T", Index: "IX",
		Options: ddl.OptionOverrides{IgnoreBlocking: boolPtr(true)},
	}
	target := ddl.Target{MajorVersion: 15, Tier: ddl.TierStandard}

	got, decisions := ddl.Resolve(op, target, m, ddl.Policy{})

	if !got.IgnoreBlocking {
		t.Errorf("IgnoreBlocking = false, want true (per-op override)")
	}
	var found bool
	for _, d := range decisions {
		if d.Option == "ignore_blocking" {
			found = true
		}
	}
	if !found {
		t.Errorf("decisions missing an ignore_blocking entry, want one for --explain")
	}

	// Absent override → false.
	plain := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}
	if got2, _ := ddl.Resolve(plain, target, m, ddl.Policy{}); got2.IgnoreBlocking {
		t.Errorf("IgnoreBlocking = true with no override, want false")
	}
}

func TestResolveMaxBlockMinutes(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{
		Schema: "dbo", Table: "T", Index: "IX",
		Options: ddl.OptionOverrides{MaxBlockMinutes: intPtr(15)},
	}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, decisions := ddl.Resolve(op, target, m, ddl.Policy{})
	if got.MaxBlockMinutes != 15 {
		t.Errorf("MaxBlockMinutes = %d, want 15", got.MaxBlockMinutes)
	}
	var found bool
	for _, d := range decisions {
		if d.Option == "max_block_minutes" && d.Value == "15" {
			found = true
		}
	}
	if !found {
		t.Errorf("decisions missing a max_block_minutes=15 entry, want one for --explain")
	}

	// Absent or non-positive → no cap.
	plain := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}
	if got2, _ := ddl.Resolve(plain, target, m, ddl.Policy{}); got2.MaxBlockMinutes != 0 {
		t.Errorf("MaxBlockMinutes = %d with no override, want 0", got2.MaxBlockMinutes)
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

func TestResolveShrinkWALPAuto2022(t *testing.T) {
	m := resolveMatrix()
	op := ddl.Shrink{Type: "data", Files: "MyDb_Data", TargetFreeSpace: "10%"}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierStandard} // not Enterprise-gated

	got, decisions := ddl.Resolve(op, target, m, ddl.Policy{})

	// WALP only; never ONLINE/RESUMABLE/MAX_DURATION for a shrink.
	want := ddl.ResolvedOptions{WaitAtLowPriority: true, AbortAfterWait: "SELF"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Resolve(shrink) mismatch (-want +got):\n%s", diff)
	}
	if got.MaxDurationMinutes != 0 {
		t.Errorf("shrink WALP MaxDurationMinutes = %d, want 0 (no MAX_DURATION)", got.MaxDurationMinutes)
	}
	if v, ok := decisionValue(decisions, "wait_at_low_priority"); !ok || v != "ON" {
		t.Errorf("decision[wait_at_low_priority] = %q (ok=%t), want ON", v, ok)
	}
	if _, ok := decisionValue(decisions, "online"); ok {
		t.Errorf("Resolve(shrink) emitted an online decision, want none")
	}
}

func TestResolveShrinkWALPUnsupportedPre2022(t *testing.T) {
	m := resolveMatrix()
	op := ddl.Shrink{Type: "data", Files: "MyDb_Data", TargetFreeSpace: "10%"}
	target := ddl.Target{MajorVersion: 15, Tier: ddl.TierEnterprise} // 2019: no shrink WALP

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{})

	if got.WaitAtLowPriority {
		t.Errorf("Resolve(shrink, 2019) WaitAtLowPriority = true, want false")
	}
}

func TestResolveShrinkAbortBlockers(t *testing.T) {
	m := resolveMatrix()
	op := ddl.Shrink{Type: "data", Files: "MyDb_Data", TargetFreeSpace: "10%"}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{AllowAbortBlockers: true})

	if !got.WaitAtLowPriority || got.AbortAfterWait != "BLOCKERS" {
		t.Errorf("Resolve(shrink, AllowAbortBlockers) = {WALP:%t Abort:%q}, want {true BLOCKERS}",
			got.WaitAtLowPriority, got.AbortAfterWait)
	}
}

func TestResolveShrinkTempdbAlwaysSelf(t *testing.T) {
	m := resolveMatrix()
	// 2022 tier where WALP is supported, with AllowAbortBlockers ON: tempdb must
	// still never kill a blocker.
	op := ddl.ShrinkTempdb{TargetSizeMB: 20480}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{AllowAbortBlockers: true})

	if !got.WaitAtLowPriority {
		t.Fatalf("expected WALP eligible on 2022")
	}
	if got.AbortAfterWait != "SELF" {
		t.Errorf("AbortAfterWait = %q, want SELF (never BLOCKERS for tempdb)", got.AbortAfterWait)
	}
}

func TestResolveShrinkTempdbNoWALPOn2019(t *testing.T) {
	m := resolveMatrix()
	op := ddl.ShrinkTempdb{TargetSizeMB: 20480}
	target := ddl.Target{MajorVersion: 15, Tier: ddl.TierEnterprise}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{})

	if got.WaitAtLowPriority {
		t.Errorf("WALP must be off for shrink_tempdb on 2019")
	}
}

func TestResolveShrinkLogHasNoWALP(t *testing.T) {
	m := resolveMatrix()
	op := ddl.Shrink{Type: "log", TargetFreeSpace: "100MB"}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, decisions := ddl.Resolve(op, target, m, ddl.Policy{})

	if got.WaitAtLowPriority {
		t.Errorf("Resolve(shrink_log) WaitAtLowPriority = true, want false (WALP not valid for log)")
	}
	if len(decisions) != 0 {
		t.Errorf("Resolve(shrink_log) decisions = %v, want none", decisions)
	}
}

// TestResolveTempdbDropsResumable pins the tempdb restriction: SQL Server refuses a
// resumable index build there with Msg 11439 on every version and edition, while
// ONLINE alone is accepted. Reproduced against SQL Server 2022 CU26 in a container.
func TestResolveTempdbDropsResumable(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}

	for _, db := range []string{"tempdb", "TempDB", " tempdb "} {
		target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise, Database: db}

		got, decisions := ddl.Resolve(op, target, m, ddl.Policy{})

		if got.Resumable {
			t.Errorf("database %q: Resumable = true, want false (Msg 11439)", db)
		}
		if !got.Online {
			t.Errorf("database %q: Online = false, want true (ONLINE is accepted in tempdb)", db)
		}
		if v, ok := decisionValue(decisions, "resumable"); !ok || v != "OFF" {
			t.Errorf("database %q: decision[resumable] = %q (present=%t), want OFF", db, v, ok)
		}
	}
}

func TestResolveUserDatabaseKeepsResumable(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise, Database: "PRODDB"}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{})

	if !got.Resumable {
		t.Errorf("Resumable = false, want true (the tempdb restriction must not leak to user databases)")
	}
}

func TestTargetInDatabase(t *testing.T) {
	base := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise, Database: "PRODDB"}

	if got := base.InDatabase("tempdb").Database; got != "tempdb" {
		t.Errorf("InDatabase(%q).Database = %q, want tempdb", "tempdb", got)
	}
	if got := base.InDatabase("").Database; got != "PRODDB" {
		t.Errorf("InDatabase(\"\").Database = %q, want the connection's own database PRODDB", got)
	}
	if base.Database != "PRODDB" {
		t.Errorf("InDatabase mutated its receiver: Database = %q", base.Database)
	}
}

// TestResolveNeverPairsResumableWithSortInTempdb pins Msg 11438: "The SORT_IN_TEMPDB
// option cannot be set to 'ON' when the RESUMABLE option is set to 'ON'". It is raised
// at severity 15, so the batch fails at compile time and nothing runs. Reproduced
// against SQL Server 2022 CU26 in a container.
func TestResolveNeverPairsResumableWithSortInTempdb(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise, Database: "PRODDB"}

	// sort_in_tempdb is opt-in, so it only reaches ON through an explicit force —
	// here the global config policy, which is how an operator hits this in practice.
	got, decisions := ddl.Resolve(op, target, m, ddl.Policy{SortInTempDB: boolPtr(true)})

	if !got.Resumable {
		t.Errorf("Resumable = false, want true (RESUMABLE is the option worth keeping)")
	}
	if got.SortInTempDB {
		t.Errorf("SortInTempDB = true, want false (Msg 11438 kills the batch at compile time)")
	}
	if v, ok := decisionValue(decisions, "sort_in_tempdb"); !ok || v != "OFF" {
		t.Errorf("decision[sort_in_tempdb] = %q (present=%t), want OFF", v, ok)
	}
}

// TestResolveKeepsSortInTempdbWhenResumableIsOff is the other half: with no conflict,
// an explicitly forced sort_in_tempdb survives.
func TestResolveKeepsSortInTempdbWhenResumableIsOff(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise, Database: "PRODDB"}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{SortInTempDB: boolPtr(true), Resumable: boolPtr(false)})

	if got.Resumable {
		t.Errorf("Resumable = true, want false (forced off)")
	}
	if !got.SortInTempDB {
		t.Errorf("SortInTempDB = false, want true (no conflict once RESUMABLE is off)")
	}
}
