package run

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// victimFixture builds a killer plus the levers a test needs over it. events are what
// the engine route (the ReactionEvent sink) saw; narrated is what the presentation
// route (the onKill callback) saw. Both must fire on a kill.
type victimFixture struct {
	killer   *VictimKiller
	clk      *ManualClock
	killed   []int
	events   []AmplifierKillEvent
	narrated []AmplifierKillEvent
	killErr  error
}

func newVictimFixture(t *testing.T, p AmplifierPolicy) *victimFixture {
	t.Helper()
	f := &victimFixture{clk: NewManualClock(time.Unix(1_800_000_000, 0))}
	f.killer = NewVictimKiller(
		func(_ context.Context, spid int) error {
			if f.killErr != nil {
				return f.killErr
			}
			f.killed = append(f.killed, spid)
			return nil
		},
		func(_ context.Context, hex string, step int) (mssql.AgentJob, error) {
			return mssql.AgentJob{Resolved: true, JobID: hex, StepID: step, JobName: "IndexOptimize"}, nil
		},
		func(ev AmplifierKillEvent) { f.narrated = append(f.narrated, ev) },
		f.clk,
		mssql.AppNamePrefix,
	)
	f.killer.SetSink(func(ev ReactionEvent) {
		if ev.Amplifier != nil {
			f.events = append(f.events, *ev.Amplifier)
		}
	})
	f.killer.Arm(p)
	return f
}

// amplifierSnapshot is the PRODDB shape: our DDL (67), a blocked UPDATE STATISTICS
// (79), and `behind` readers queued behind it.
func amplifierSnapshot(behind int) []mssql.Session {
	out := []mssql.Session{
		{SPID: 67, Command: "ALTER INDEX", Program: "SqlGoPace"},
		{SPID: 79, Command: "UPDATE STATISTICS", BlockingSPID: 67,
			Program: "SQLAgent - TSQL JobStep (Job 0xAB : Step 1)",
			Login:   "svc_agent", Host: "SQLPROD01", Database: "PRODDB",
			ActiveQuery: "UPDATE STATISTICS [dbo].[MEASUREMENT]", WaitMS: 19_280_023},
	}
	for i := 0; i < behind; i++ {
		out = append(out, mssql.Session{SPID: 1000 + i, Command: "SELECT", BlockingSPID: 79})
	}
	return out
}

func defaultPolicy() AmplifierPolicy {
	return AmplifierPolicy{MinBlockedBehind: 1, After: 60 * time.Second}
}

func TestVictimKillerKillsAfterDwell(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	snap := amplifierSnapshot(16)
	ctx := context.Background()

	f.killer.consider(ctx, snap, 67, nil)
	if len(f.killed) != 0 {
		t.Fatalf("killed %v before the dwell elapsed, want none", f.killed)
	}
	if !f.killer.Suppressed(79) {
		t.Error("Suppressed(79) = false while the kill is pending, want true")
	}

	f.clk.Advance(59 * time.Second)
	f.killer.consider(ctx, snap, 67, nil)
	if len(f.killed) != 0 {
		t.Fatalf("killed %v one second early, want none", f.killed)
	}

	f.clk.Advance(2 * time.Second)
	f.killer.consider(ctx, snap, 67, nil)
	if len(f.killed) != 1 || f.killed[0] != 79 {
		t.Fatalf("killed = %v, want [79]", f.killed)
	}

	// Killed at most once per episode.
	f.killer.consider(ctx, snap, 67, nil)
	if len(f.killed) != 1 {
		t.Errorf("killed = %v, want a single KILL per episode", f.killed)
	}
}

func TestVictimKillerEventCarriesChainAndJob(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	snap := amplifierSnapshot(16)
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(61 * time.Second)
	f.killer.consider(context.Background(), snap, 67, nil)

	if len(f.events) != 1 {
		t.Fatalf("events = %d, want 1", len(f.events))
	}
	ev := f.events[0]
	if ev.SPID != 79 {
		t.Errorf("event SPID = %d, want 79", ev.SPID)
	}
	if ev.BlockedBehind != 16 {
		t.Errorf("event BlockedBehind = %d, want 16", ev.BlockedBehind)
	}
	if ev.Waited != 61*time.Second {
		t.Errorf("event Waited = %v, want 61s", ev.Waited)
	}
	if want := time.Unix(1_800_000_000, 0); !ev.FirstEligible.Equal(want) {
		t.Errorf("event FirstEligible = %v, want %v", ev.FirstEligible, want)
	}
	// Both routes must fire: the engine sink feeds the report and sidecar, the
	// presentation callback feeds the TUI, whose run output is io.Discard.
	if len(f.narrated) != 1 || f.narrated[0].SPID != 79 {
		t.Errorf("narrated = %+v, want a single event for SPID 79", f.narrated)
	}
	if !ev.Job.Resolved || ev.Job.JobName != "IndexOptimize" || ev.Job.StepID != 1 {
		t.Errorf("event Job = %+v, want a resolved IndexOptimize step 1", ev.Job)
	}
	if ev.Login != "svc_agent" || ev.Host != "SQLPROD01" || ev.Database != "PRODDB" {
		t.Errorf("event identity = %+v, want the victim's login/host/database", ev)
	}
}

func TestVictimKillerSkipsWhenNothingIsQueuedBehind(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	snap := amplifierSnapshot(0)
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(10 * time.Minute)
	f.killer.consider(context.Background(), snap, 67, nil)

	if len(f.killed) != 0 {
		t.Errorf("killed = %v, want none — a lone maintenance victim is not an amplifier", f.killed)
	}
	if f.killer.Suppressed(79) {
		t.Error("Suppressed(79) = true for a non-amplifying victim, want false")
	}
}

func TestVictimKillerSkipsNonMaintenanceVictim(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	snap := amplifierSnapshot(16)
	snap[1].Command = "SELECT"
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(10 * time.Minute)
	f.killer.consider(context.Background(), snap, 67, nil)

	if len(f.killed) != 0 {
		t.Errorf("killed = %v, want none — an application victim is never killed", f.killed)
	}
}

func TestVictimKillerSkipsOwnProgram(t *testing.T) {
	// Our own program_name carries the build version ("SqlGoPace/0.13.0"), so
	// self-exclusion is a prefix match — which also excludes a DIFFERENT version of
	// SqlGoPace running a concurrent manifest, exactly the PRODDB size-split case.
	for _, program := range []string{"SqlGoPace", "SqlGoPace/0.13.0", "SqlGoPace/0.11.2"} {
		t.Run(program, func(t *testing.T) {
			f := newVictimFixture(t, defaultPolicy())
			snap := amplifierSnapshot(16)
			snap[1].Program = program
			snap[1].Command = "ALTER INDEX"
			f.killer.consider(context.Background(), snap, 67, nil)
			f.clk.Advance(10 * time.Minute)
			f.killer.consider(context.Background(), snap, 67, nil)

			if len(f.killed) != 0 {
				t.Errorf("killed = %v, want none — never kill another SqlGoPace session", f.killed)
			}
		})
	}
}

func TestVictimKillerRespectsIgnoreRules(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	ignore, err := CompileIgnoredSessions([]ddl.IgnoredSession{{LoginName: "^svc_agent$"}})
	if err != nil {
		t.Fatalf("CompileIgnoredSessions() error = %v", err)
	}
	snap := amplifierSnapshot(16)
	f.killer.consider(context.Background(), snap, 67, ignore)
	f.clk.Advance(10 * time.Minute)
	f.killer.consider(context.Background(), snap, 67, ignore)

	if len(f.killed) != 0 {
		t.Errorf("killed = %v, want none — an explicitly ignored victim is never killed", f.killed)
	}
	if f.killer.Suppressed(79) {
		t.Error("Suppressed(79) = true for an ignored victim; ignoring already excludes it from Unignored")
	}
}

func TestVictimKillerSkipsOurOwnBlocker(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	snap := amplifierSnapshot(16)
	snap[0].BlockingSPID = 79 // mutual block: 79 blocks us and we block 79
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(10 * time.Minute)
	f.killer.consider(context.Background(), snap, 67, nil)

	if len(f.killed) != 0 {
		t.Errorf("killed = %v, want none — our own direct blocker belongs to BlockerKiller", f.killed)
	}
}

func TestVictimKillerFailedKillWithdrawsSuppression(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	f.killErr = errors.New("permission denied")
	snap := amplifierSnapshot(16)
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(61 * time.Second)
	f.killer.consider(context.Background(), snap, 67, nil)

	if f.killer.Suppressed(79) {
		t.Error("Suppressed(79) = true after a failed KILL, want false so the normal yield timer applies")
	}
	if len(f.events) != 0 || len(f.narrated) != 0 {
		t.Errorf("events = %v, narrated = %v, want none on either route when the KILL failed", f.events, f.narrated)
	}
}

func TestVictimKillerGraceWindowAfterKill(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	snap := amplifierSnapshot(16)
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(61 * time.Second)
	f.killer.consider(context.Background(), snap, 67, nil)

	if !f.killer.Suppressed(79) {
		t.Fatal("Suppressed(79) = false immediately after a successful KILL, want true during the grace window")
	}
	f.clk.Advance(14 * time.Second)
	f.killer.consider(context.Background(), snap, 67, nil)
	if !f.killer.Suppressed(79) {
		t.Error("Suppressed(79) = false at 14s, want true — the grace window is 15s")
	}
	f.clk.Advance(2 * time.Second)
	f.killer.consider(context.Background(), snap, 67, nil)
	if f.killer.Suppressed(79) {
		t.Error("Suppressed(79) = true past the 15s grace window, want false")
	}
}

func TestVictimKillerEpisodeResetsWhenVictimClears(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	snap := amplifierSnapshot(16)
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(30 * time.Second)

	// The victim goes away, then comes back: the dwell restarts from zero.
	f.killer.consider(context.Background(), []mssql.Session{{SPID: 67, Program: "SqlGoPace"}}, 67, nil)
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(59 * time.Second)
	f.killer.consider(context.Background(), snap, 67, nil)

	if len(f.killed) != 0 {
		t.Errorf("killed = %v, want none — the dwell must restart after the victim clears", f.killed)
	}
}

func TestVictimKillerDisarmed(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	f.killer.Disarm()
	snap := amplifierSnapshot(16)
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(10 * time.Minute)
	f.killer.consider(context.Background(), snap, 67, nil)

	if len(f.killed) != 0 {
		t.Errorf("killed = %v, want none while disarmed", f.killed)
	}
	if f.killer.Suppressed(79) {
		t.Error("Suppressed(79) = true while disarmed, want false")
	}
}

func TestVictimKillerNilReceiverIsSafe(t *testing.T) {
	var k *VictimKiller
	k.consider(context.Background(), amplifierSnapshot(16), 67, nil)
	if k.Suppressed(79) {
		t.Error("Suppressed() on a nil killer = true, want false")
	}
	k.Disarm() // must not panic
}

// TestVictimKillerFailedKillEmitsWarnOnce covers review finding 1: the design's §5
// error-handling table requires a failed KILL to be operator-visible (a warn event),
// not a silent fallback to the normal yield timer. It must fire exactly once per
// episode, not once per poll, and it must never set Amplifier — that field means a
// kill happened.
func TestVictimKillerFailedKillEmitsWarnOnce(t *testing.T) {
	clk := NewManualClock(time.Unix(1_800_000_000, 0))
	killErr := errors.New("permission denied")
	var raw []ReactionEvent
	k := NewVictimKiller(
		func(context.Context, int) error { return killErr },
		func(context.Context, string, int) (mssql.AgentJob, error) { return mssql.AgentJob{}, nil },
		nil,
		clk,
		mssql.AppNamePrefix,
	)
	k.SetSink(func(ev ReactionEvent) { raw = append(raw, ev) })
	k.Arm(defaultPolicy())

	snap := amplifierSnapshot(16)
	k.consider(context.Background(), snap, 67, nil)
	clk.Advance(61 * time.Second)
	k.consider(context.Background(), snap, 67, nil) // first failed attempt: warn fires
	k.consider(context.Background(), snap, 67, nil) // second poll after failure: no repeat

	var warns []ReactionEvent
	for _, ev := range raw {
		if ev.Kind == "warn" {
			warns = append(warns, ev)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("warn events = %d, want exactly 1 (once per episode, not once per poll)", len(warns))
	}
	if warns[0].Amplifier != nil {
		t.Error("warn event Amplifier != nil, want nil — Amplifier means a kill happened")
	}
	for _, want := range []string{"79", "UPDATE STATISTICS", "permission denied"} {
		if !strings.Contains(warns[0].Detail, want) {
			t.Errorf("warn Detail = %q, missing %q", warns[0].Detail, want)
		}
	}
}

// TestVictimKillerTracksIndependentDwellsForTwoVictims covers review finding 2: the
// per-SPID episodes map is the structural departure from BlockerKiller's single
// current/since/killed triple, justified because several victims can be eligible at
// once (design §1.5). A single-victim implementation would fail this test even
// though it passes every other test in this file, because amplifierSnapshot only
// ever produces one direct victim.
func TestVictimKillerTracksIndependentDwellsForTwoVictims(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	ctx := context.Background()

	// t0: victim A (79) appears alone, eligible immediately.
	f.killer.consider(ctx, twoVictimSnapshot(false), 67, nil)

	f.clk.Advance(30 * time.Second)
	// t0+30s: victim B (80) appears alongside A, on its own clock.
	f.killer.consider(ctx, twoVictimSnapshot(true), 67, nil)

	f.clk.Advance(30 * time.Second) // t0+60s: A's 60s dwell elapsed, B's (30s in) has not
	f.killer.consider(ctx, twoVictimSnapshot(true), 67, nil)
	if len(f.killed) != 1 || f.killed[0] != 79 {
		t.Fatalf("killed = %v at t0+60s, want [79] only", f.killed)
	}

	f.clk.Advance(30 * time.Second) // t0+90s: B's 60s dwell (since t0+30s) elapsed
	f.killer.consider(ctx, twoVictimSnapshot(true), 67, nil)
	if len(f.killed) != 2 || f.killed[1] != 80 {
		t.Fatalf("killed = %v at t0+90s, want [79 80]", f.killed)
	}
}

// twoVictimSnapshot is DirectVictims 79 and (when includeB) 80, both blocked by our
// DDL (67) and each with one session queued behind it — two independent amplifiers.
func twoVictimSnapshot(includeB bool) []mssql.Session {
	out := []mssql.Session{
		{SPID: 67, Command: "ALTER INDEX", Program: "SqlGoPace"},
		{SPID: 79, Command: "UPDATE STATISTICS", BlockingSPID: 67},
		{SPID: 1001, Command: "SELECT", BlockingSPID: 79},
	}
	if includeB {
		out = append(out,
			mssql.Session{SPID: 80, Command: "UPDATE STATISTICS", BlockingSPID: 67},
			mssql.Session{SPID: 2001, Command: "SELECT", BlockingSPID: 80},
		)
	}
	return out
}

// TestVictimKillerZeroValuePolicyDoesNotKillWithoutFanout covers review finding 3:
// the zero value of AmplifierPolicy must not mean "kill every blocked maintenance
// statement immediately" — MinBlockedBehind < 1 floors to 1 inside Arm, so a lone
// victim with nothing queued behind it is never an amplifier, whatever the caller
// (config included) supplied.
func TestVictimKillerZeroValuePolicyDoesNotKillWithoutFanout(t *testing.T) {
	f := newVictimFixture(t, AmplifierPolicy{})
	snap := amplifierSnapshot(0) // nothing queued behind the victim
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(time.Second)
	f.killer.consider(context.Background(), snap, 67, nil)

	if len(f.killed) != 0 {
		t.Errorf("killed = %v, want none — AmplifierPolicy{} floors MinBlockedBehind to 1", f.killed)
	}
}

// TestVictimKillerDefaultsSelfProgramWhenEmpty covers review finding 4: design §1.4
// calls self-exclusion "not hypothetical" — a wiring slip that leaves selfProgram
// empty must not silently disable it. NewVictimKiller defaults "" to
// mssql.AppNamePrefix, exactly as clk defaults to System.
func TestVictimKillerDefaultsSelfProgramWhenEmpty(t *testing.T) {
	var killed []int
	k := NewVictimKiller(
		func(_ context.Context, spid int) error { killed = append(killed, spid); return nil },
		nil,
		nil,
		NewManualClock(time.Unix(1_800_000_000, 0)),
		"", // empty selfProgram must still default to mssql.AppNamePrefix
	)
	k.Arm(AmplifierPolicy{MinBlockedBehind: 1})

	snap := amplifierSnapshot(16)
	snap[1].Program = "SqlGoPace/0.13.0"
	snap[1].Command = "ALTER INDEX"
	k.consider(context.Background(), snap, 67, nil)

	if len(killed) != 0 {
		t.Errorf("killed = %v, want none — an empty selfProgram must still exclude another SqlGoPace session", killed)
	}
}

func TestAmplifierDetailNamesTheObjectAndJob(t *testing.T) {
	ev := AmplifierKillEvent{
		SPID: 79, Command: "UPDATE STATISTICS", BlockedBehind: 16,
		Statement: "UPDATE STATISTICS\n   [dbo].[MEASUREMENT] [PK_MEASUREMENT]\n   WITH MAXDOP 2",
		Job:       mssql.AgentJob{Resolved: true, JobName: "IndexOptimize - USER_DATABASES", StepID: 1},
	}
	got := AmplifierDetail(ev)
	for _, want := range []string{
		"SPID 79",
		"[dbo].[MEASUREMENT]",          // the object, not just the verb
		"16 session(s) queued behind it",
		`SQL Agent job "IndexOptimize - USER_DATABASES" step 1`,
		"sp_update_job",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("AmplifierDetail() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("AmplifierDetail() must be one line, got %q", got)
	}
}

func TestAmplifierDetailFallsBackToProgramWhenUnattributed(t *testing.T) {
	ev := AmplifierKillEvent{
		SPID: 79, Command: "UPDATE STATISTICS", BlockedBehind: 3,
		Program: "SQLAgent - Job invocation engine", Login: "svc_agent", Host: "SQLPROD01",
	}
	got := AmplifierDetail(ev)
	for _, want := range []string{"SQLAgent - Job invocation engine", "login=svc_agent", "host=SQLPROD01"} {
		if !strings.Contains(got, want) {
			t.Errorf("AmplifierDetail() = %q, missing %q", got, want)
		}
	}
}
