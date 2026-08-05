package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// killRecorder captures the SPIDs a killer terminated.
type killRecorder struct{ spids []int }

func (r *killRecorder) kill(_ context.Context, spid int) error {
	r.spids = append(r.spids, spid)
	return nil
}

// blockedSnapshot builds an ActiveSessions snapshot where ddlSPID is blocked by a blocker
// session with the given login.
func blockedSnapshot(ddlSPID, blockerSPID int, login string) []mssql.Session {
	return []mssql.Session{
		{SPID: ddlSPID, BlockingSPID: blockerSPID, WaitType: "LCK_M_SCH_M"},
		{SPID: blockerSPID, Login: login, Program: "Reporting"},
	}
}

// killRule builds a KilledSession from a login regexp and delay for the tests.
func killRuleFor(login string, after int) ddl.KilledSession {
	return ddl.KilledSession{IgnoredSession: ddl.IgnoredSession{LoginName: login}, AfterSeconds: after}
}

func killerFor(t *testing.T, rec *killRecorder, now *time.Time, rules []ddl.KilledSession) *BlockerKiller {
	t.Helper()
	k := NewBlockerKiller(rec.kill, nil, func() time.Time { return *now })
	compiled, err := compileKilledSessions(rules, 0)
	if err != nil {
		t.Fatalf("compile kill rules: %v", err)
	}
	k.SetSource(staticKill{rules: compiled})
	return k
}

func TestBlockerKillerKillsAfterDelay(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 300)})
	snap := blockedSnapshot(100, 104, "SVC_RPT")

	k.consider(context.Background(), snap, 100) // first sight: start the clock
	if len(rec.spids) != 0 {
		t.Fatalf("must not kill before the delay elapses, got %v", rec.spids)
	}
	now = now.Add(4 * time.Minute)
	k.consider(context.Background(), snap, 100)
	if len(rec.spids) != 0 {
		t.Fatalf("still under the 5m delay, got %v", rec.spids)
	}
	now = now.Add(2 * time.Minute) // total 6m > 5m
	k.consider(context.Background(), snap, 100)
	if len(rec.spids) != 1 || rec.spids[0] != 104 {
		t.Fatalf("expected kill of SPID 104 after the delay, got %v", rec.spids)
	}
	// Same blocker still present: must not be killed again in the same episode.
	now = now.Add(1 * time.Minute)
	k.consider(context.Background(), snap, 100)
	if len(rec.spids) != 1 {
		t.Fatalf("blocker killed twice in one episode: %v", rec.spids)
	}
}

func TestBlockerKillerOnSight(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 0)}) // after=0
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	if len(rec.spids) != 1 || rec.spids[0] != 104 {
		t.Fatalf("after=0 should kill on sight, got %v", rec.spids)
	}
}

func TestBlockerKillerLeavesNonMatchAlone(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 0)})
	// Blocker login does not match the rule.
	k.consider(context.Background(), blockedSnapshot(100, 104, "SOMEONE_ELSE"), 100)
	if len(rec.spids) != 0 {
		t.Fatalf("non-matching blocker must be left alone, got %v", rec.spids)
	}
}

func TestBlockerKillerNoBlockerIsNoOp(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 0)})
	// Our session is not blocked (BlockingSPID 0).
	k.consider(context.Background(), []mssql.Session{{SPID: 100}}, 100)
	if len(rec.spids) != 0 {
		t.Fatalf("no blocker: nothing to kill, got %v", rec.spids)
	}
}

func TestBlockerKillerNoSourceIsNoOp(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := NewBlockerKiller(rec.kill, nil, func() time.Time { return now })
	// No SetSource called (the disarmed state between manifests): must never kill.
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	if len(rec.spids) != 0 {
		t.Fatalf("no source: must not kill, got %v", rec.spids)
	}
}

func TestBlockerKillerMatchesRosterArmedRule(t *testing.T) {
	// The roster builds kill rules via ddl.KilledSessionFor(criterion, value, 0); this test
	// locks that such a rule actually matches what BlockerKiller.consider evaluates against the
	// blocking session — the one seam a criterion-string mismatch would silently no-op.
	t.Run("host_name", func(t *testing.T) {
		rule, ok := ddl.KilledSessionFor("host_name", "BATCH01", 0)
		if !ok {
			t.Fatal("KilledSessionFor(host_name) returned ok=false")
		}
		rec := &killRecorder{}
		now := time.Unix(0, 0)
		k := killerFor(t, rec, &now, []ddl.KilledSession{rule})
		// ddlSPID 100 blocked by 104, whose Host is BATCH01.
		snap := []mssql.Session{
			{SPID: 100, BlockingSPID: 104, WaitType: "LCK_M_SCH_M"},
			{SPID: 104, Host: "BATCH01", Login: "svc", Program: "Batch"},
		}
		k.consider(context.Background(), snap, 100)
		if len(rec.spids) != 1 || rec.spids[0] != 104 {
			t.Fatalf("a roster-armed host_name rule should kill the matching blocker, got %v", rec.spids)
		}
	})
	t.Run("login_name", func(t *testing.T) {
		rule, ok := ddl.KilledSessionFor("login_name", "SVC_RPT", 0)
		if !ok {
			t.Fatal("KilledSessionFor(login_name) returned ok=false")
		}
		rec := &killRecorder{}
		now := time.Unix(0, 0)
		k := killerFor(t, rec, &now, []ddl.KilledSession{rule})
		k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
		if len(rec.spids) != 1 || rec.spids[0] != 104 {
			t.Fatalf("a roster-armed login_name rule should kill the matching blocker, got %v", rec.spids)
		}
	})
}

func TestManifestKillSourceReloadPicksUpNewRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	base := "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
	if err := os.WriteFile(path, []byte("kill_blocking_sessions:\n  - login_name: \"^SVC_RPT$\"\n"+base), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ddl.LoadManifestFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	initial, err := compileKilledSessions(m.KillBlockingSessions, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	src := newManifestKillSource(path, 0, initial)
	if len(src.Current()) != 1 {
		t.Fatalf("expected 1 initial kill rule, got %d", len(src.Current()))
	}

	// Append a second rule mid-run; live reload must pick it up.
	if err := os.WriteFile(path, []byte("kill_blocking_sessions:\n  - login_name: \"^SVC_RPT$\"\n  - host_name: \"^BATCH01$\"\n"+base), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if len(src.Current()) != 2 {
		t.Errorf("reload should pick up the appended rule, got %d rules", len(src.Current()))
	}
}

func TestSessionRuleKeyIsStableAndInjective(t *testing.T) {
	compile := func(t *testing.T, rules ...ddl.KilledSession) []killRule {
		t.Helper()
		out, err := compileKilledSessions(rules, 0)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return out
	}

	t.Run("same rule text compiles to the same key", func(t *testing.T) {
		a := compile(t, killRuleFor("^SVC_RPT$", 60))
		b := compile(t, killRuleFor("^SVC_RPT$", 600)) // the delay is not part of the identity
		if a[0].key != b[0].key {
			t.Errorf("same matcher must give the same key: %q vs %q", a[0].key, b[0].key)
		}
	})

	t.Run("different rule text gives a different key", func(t *testing.T) {
		rules := compile(t, killRuleFor("^SVC_RPT$", 0), killRuleFor("^SVC_ETL$", 0))
		if rules[0].key == rules[1].key {
			t.Errorf("different logins must give different keys, both %q", rules[0].key)
		}
	})

	t.Run("an alternation cannot collide with two fields", func(t *testing.T) {
		alt := ddl.KilledSession{IgnoredSession: ddl.IgnoredSession{AppName: "x|y"}}
		split := ddl.KilledSession{IgnoredSession: ddl.IgnoredSession{AppName: "x", HostName: "y"}}
		rules := compile(t, alt, split)
		if rules[0].key == rules[1].key {
			t.Errorf("a | inside a regexp must not collide with the field separator, both %q", rules[0].key)
		}
	})
}

func TestSessionRuleStringOmitsUnsetFields(t *testing.T) {
	rules, err := compileKilledSessions([]ddl.KilledSession{{
		IgnoredSession: ddl.IgnoredSession{AppName: "^SQLAgent", LoginName: `^CORP\\svc_`},
	}}, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := rules[0].match.String()
	want := `{app=~"^SQLAgent", login=~"^CORP\\\\svc_"}`
	if got != want {
		t.Errorf("String() = %s, want %s", got, want)
	}
}

func TestBlockerKillerRecidivistDoesNotBuyAFreshDwell(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)})
	first := blockedSnapshot(100, 104, "SVC_RPT")

	k.consider(context.Background(), first, 100) // banks 0, starts the episode
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), first, 100) // banks 60s -> kill
	if len(rec.spids) != 1 || rec.spids[0] != 104 {
		t.Fatalf("expected the first blocker killed after its dwell, got %v", rec.spids)
	}

	// One second later the same job is back under a new SPID: the debt is already paid.
	now = now.Add(time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 155, "SVC_RPT"), 100)
	if len(rec.spids) != 2 || rec.spids[1] != 155 {
		t.Fatalf("a returning offender must be killed at once, got %v", rec.spids)
	}
}

func TestBlockerKillerDebtIsPerRule(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{
		killRuleFor("^SVC_RPT$", 60),
		killRuleFor("^SVC_ETL$", 60),
	})

	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("setup: the first rule's blocker should have been killed, got %v", rec.spids)
	}

	// A blocker matching a DIFFERENT rule has its own debt and serves the full dwell.
	now = now.Add(time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 200, "SVC_ETL"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("another rule's blocker must not inherit the first rule's debt, got %v", rec.spids)
	}
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 200, "SVC_ETL"), 100)
	if len(rec.spids) != 2 || rec.spids[1] != 200 {
		t.Fatalf("the second rule's blocker should be killed after its own dwell, got %v", rec.spids)
	}
}

func TestBlockerKillerDebtIsForgottenAfterTheWindow(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)})

	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("setup: expected one kill, got %v", rec.spids)
	}

	// Quiet for longer than the window: the bucket is forgotten, the dwell is full again.
	now = now.Add(recidivismWindow + time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 155, "SVC_RPT"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("after a quiet window the offender must serve the full dwell, got %v", rec.spids)
	}
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 155, "SVC_RPT"), 100)
	if len(rec.spids) != 2 || rec.spids[1] != 155 {
		t.Fatalf("expected the kill once the full dwell was served again, got %v", rec.spids)
	}
}

func TestBlockerKillerSetSourceClearsTheDebt(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)})

	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("setup: expected one kill, got %v", rec.spids)
	}

	// A new manifest re-arms the killer: debt must not leak across manifests.
	compiled, err := compileKilledSessions([]ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)}, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	k.SetSource(staticKill{rules: compiled})

	now = now.Add(time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 155, "SVC_RPT"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("SetSource must clear the debt, got %v", rec.spids)
	}
}

func TestBlockerKillerReportsTheDebtAsWaited(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	var events []KillEvent
	k := NewBlockerKiller(rec.kill, func(ev KillEvent) { events = append(events, ev) },
		func() time.Time { return now })
	compiled, err := compileKilledSessions([]ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)}, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	k.SetSource(staticKill{rules: compiled})

	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 155, "SVC_RPT"), 100)

	if len(events) != 2 {
		t.Fatalf("expected two kill events, got %d", len(events))
	}
	// The recidivist's own episode is one second old; the identity has blocked us for 60s,
	// and that is what the operator must read.
	if events[1].Waited != 60*time.Second {
		t.Errorf("Waited = %s, want the identity's debt (60s)", events[1].Waited)
	}
}
