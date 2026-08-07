package run

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	raw      []ReactionEvent // every sink event, including the warns
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
		f.raw = append(f.raw, ev)
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
// They carry DIFFERENT Agent job steps on purpose: the dwell is served per offender
// identity, not per SPID, so two sessions sharing a login/host/program would be one
// offender sharing one debt and B would inherit A's dwell. Independent dwells are a
// property of independent offenders, and that is what this test is about.
func twoVictimSnapshot(includeB bool) []mssql.Session {
	out := []mssql.Session{
		{SPID: 67, Command: "ALTER INDEX", Program: "SqlGoPace"},
		{SPID: 79, Command: "UPDATE STATISTICS", BlockingSPID: 67,
			Program: "SQLAgent - TSQL JobStep (Job 0xA1 : Step 1)"},
		{SPID: 1001, Command: "SELECT", BlockingSPID: 79},
	}
	if includeB {
		out = append(out,
			mssql.Session{SPID: 80, Command: "UPDATE STATISTICS", BlockingSPID: 67,
				Program: "SQLAgent - TSQL JobStep (Job 0xB2 : Step 1)"},
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

// TestVictimKillerCallbacksRunUnlocked covers the review finding that the sink and
// onKill callbacks used to fire while k.mu was held (inside the old notify method,
// called from considerLocked's predecessor): Disarm — called from the engine goroutine
// at manifest end — would then block behind whichever callback was slow, and SqlGoPace
// has no query timeouts to bound that wait. Calling Suppressed (which itself takes k.mu)
// from inside the sink is a lock-reentrancy probe: sync.Mutex is not reentrant, so a
// regression back to notify-under-lock hangs this test rather than merely failing an
// assertion — which is exactly the "silent regression" the coordinator's review warned
// against missing.
func TestVictimKillerCallbacksRunUnlocked(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	var (
		sinkCalled             bool
		suppressedDuringSink   bool
		onKillCalled           bool
		suppressedDuringOnKill bool
	)
	f.killer.SetSink(func(ev ReactionEvent) {
		if ev.Amplifier == nil {
			return
		}
		sinkCalled = true
		suppressedDuringSink = f.killer.Suppressed(ev.Amplifier.SPID)
	})
	// newVictimFixture's onKill only appends to f.narrated, so probe onKill's locking
	// with a second, purpose-built killer whose onKill also calls Suppressed. Declared
	// before NewVictimKiller so the closure can capture the variable (assigned below,
	// but not invoked until consider() runs, well after k2 is set).
	var k2 *VictimKiller
	k2 = NewVictimKiller(
		func(_ context.Context, spid int) error { return nil },
		func(_ context.Context, hex string, step int) (mssql.AgentJob, error) {
			return mssql.AgentJob{}, nil
		},
		func(ev AmplifierKillEvent) {
			onKillCalled = true
			suppressedDuringOnKill = k2.Suppressed(ev.SPID)
		},
		f.clk,
		mssql.AppNamePrefix,
	)
	k2.Arm(defaultPolicy())

	snap := amplifierSnapshot(16)
	f.killer.consider(context.Background(), snap, 67, nil)
	k2.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(61 * time.Second)
	f.killer.consider(context.Background(), snap, 67, nil)
	k2.consider(context.Background(), snap, 67, nil)

	if !sinkCalled {
		t.Fatal("sink was never called with an Amplifier event")
	}
	if !suppressedDuringSink {
		t.Error("Suppressed(79) = false when called from inside the sink, want true — the kill was already recorded when the sink fired")
	}
	if !onKillCalled {
		t.Fatal("onKill was never called")
	}
	if !suppressedDuringOnKill {
		t.Error("Suppressed(79) = false when called from inside onKill, want true")
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
		"[dbo].[MEASUREMENT]", // the object, not just the verb
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

// TestVictimKillerExcludesTheConnectionsOwnApplicationName covers the whole-branch
// finding that self-exclusion used to key off the mssql.AppNamePrefix constant instead
// of the application name the connection actually presents. An operator DSN carrying
// `app name=DBAToolkit` makes every SqlGoPace session report program_name
// "DBAToolkit/<version>", and a constant-keyed check would let one instance of a
// size-split campaign KILL another's in-flight, non-resumable REBUILD.
func TestVictimKillerExcludesTheConnectionsOwnApplicationName(t *testing.T) {
	// The base is what (*mssql.Conn).AppNamePrefix returns; the full effective name is
	// what a caller passing the version-suffixed value would supply. Both must exclude.
	for _, selfProgram := range []string{"DBAToolkit", "DBAToolkit/0.13.0"} {
		t.Run(selfProgram, func(t *testing.T) {
			var killed []int
			k := NewVictimKiller(
				func(_ context.Context, spid int) error { killed = append(killed, spid); return nil },
				nil, nil,
				NewManualClock(time.Unix(1_800_000_000, 0)),
				selfProgram,
			)
			k.Arm(AmplifierPolicy{MinBlockedBehind: 1})
			k.consider(context.Background(), renamedInstanceSnapshot(), 67, nil)

			if len(killed) != 1 || killed[0] != 80 {
				t.Errorf("killed = %v, want [80] — the Agent job only, never the sibling instance", killed)
			}
		})
	}
}

// renamedInstanceSnapshot is the size-split campaign on a renamed DSN: our own DDL
// (67), a sibling SqlGoPace instance's REBUILD (79) that must never be killed, and a
// genuine Agent maintenance amplifier (80) that must be, each with a reader queued
// behind it. Zero dwell, so a single consider decides.
func renamedInstanceSnapshot() []mssql.Session {
	return []mssql.Session{
		{SPID: 67, Command: "ALTER INDEX", Program: "DBAToolkit/0.13.0"},
		{SPID: 79, Command: "ALTER INDEX", BlockingSPID: 67, Program: "DBAToolkit/0.13.0"},
		{SPID: 1001, Command: "SELECT", BlockingSPID: 79},
		{SPID: 80, Command: "UPDATE STATISTICS", BlockingSPID: 67,
			Program: "SQLAgent - TSQL JobStep (Job 0xAB : Step 1)"},
		{SPID: 2001, Command: "SELECT", BlockingSPID: 80},
	}
}

// TestVictimKillerDisarmDoesNotBlockOnAKillInFlight covers the whole-branch finding
// that k.kill was still issued under the killer's mutex. Disarm runs on the engine
// goroutine at manifest end; a KILL that does not return — the connection dies
// mid-statement and the pool waits to reconnect — would then block manifest
// finalization indefinitely.
func TestVictimKillerDisarmDoesNotBlockOnAKillInFlight(t *testing.T) {
	clk := NewManualClock(time.Unix(1_800_000_000, 0))
	killing, release := make(chan struct{}), make(chan struct{})
	k := NewVictimKiller(
		func(context.Context, int) error {
			close(killing)
			<-release
			return nil
		},
		nil, nil, clk, mssql.AppNamePrefix,
	)
	k.Arm(defaultPolicy())

	snap := amplifierSnapshot(16)
	k.consider(context.Background(), snap, 67, nil)
	clk.Advance(61 * time.Second)

	considered := make(chan struct{})
	go func() { defer close(considered); k.consider(context.Background(), snap, 67, nil) }()
	<-killing // the KILL is now in flight, and stays there until release

	disarmed := make(chan struct{})
	go func() { defer close(disarmed); k.Disarm() }()
	select {
	case <-disarmed:
	case <-time.After(5 * time.Second):
		t.Error("Disarm() blocked while a KILL was in flight — the mutex must not be held across the KILL")
	}
	close(release)
	<-considered
}

// TestVictimKillerBoundsAttributionAndReportsItsFailure covers the whole-branch finding
// that the msdb attribution lookup was unbounded on the poll path: it runs on the pump
// goroutine, so a hung msdb would stop sampling and neither the blocking timeout nor
// max_block_minutes would advance. The kill must still be fully reported from the raw
// program/login/host, and the failure must be visible (design §5).
func TestVictimKillerBoundsAttributionAndReportsItsFailure(t *testing.T) {
	clk := NewManualClock(time.Unix(1_800_000_000, 0))
	var (
		hadDeadline bool
		budget      time.Duration
		events      []ReactionEvent
	)
	k := NewVictimKiller(
		func(context.Context, int) error { return nil },
		func(ctx context.Context, _ string, _ int) (mssql.AgentJob, error) {
			dl, ok := ctx.Deadline()
			hadDeadline = ok
			if ok {
				budget = time.Until(dl)
			}
			return mssql.AgentJob{}, errors.New("msdb unavailable")
		},
		nil, clk, mssql.AppNamePrefix,
	)
	k.SetSink(func(ev ReactionEvent) { events = append(events, ev) })
	k.Arm(defaultPolicy())

	snap := amplifierSnapshot(16)
	k.consider(context.Background(), snap, 67, nil)
	clk.Advance(61 * time.Second)
	k.consider(context.Background(), snap, 67, nil)

	if !hadDeadline {
		t.Fatal("the attribution context carried no deadline — a hung msdb would stall the pump")
	}
	if budget <= 0 || budget > attributeTimeout {
		t.Errorf("attribution budget = %v, want a positive value no larger than %v", budget, attributeTimeout)
	}
	var kills, warns []ReactionEvent
	for _, ev := range events {
		switch ev.Kind {
		case "kill":
			kills = append(kills, ev)
		case "warn":
			warns = append(warns, ev)
		}
	}
	if len(kills) != 1 {
		t.Fatalf("kill events = %d, want 1 — failed attribution must not suppress the kill report", len(kills))
	}
	for _, want := range []string{"SPID 79", "SQLAgent - TSQL JobStep", "login=svc_agent", "host=SQLPROD01"} {
		if !strings.Contains(kills[0].Detail, want) {
			t.Errorf("kill Detail = %q, missing %q", kills[0].Detail, want)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("warn events = %d, want 1 naming the failed attribution", len(warns))
	}
	if !strings.Contains(warns[0].Detail, "msdb unavailable") {
		t.Errorf("warn Detail = %q, want the msdb error", warns[0].Detail)
	}
}

// TestAmplifierDetailTruncatesOnARuneBoundary covers the whole-branch finding that the
// statement excerpt was cut by bytes: SQL Server allows Unicode identifiers, and half a
// rune is invalid UTF-8 in the run report and in the TUI, where lipgloss then
// mis-measures the line.
func TestAmplifierDetailTruncatesOnARuneBoundary(t *testing.T) {
	// "UPDATE STATISTICS [dbo].[" is 25 bytes, so the 120-byte cut lands 95 bytes into
	// a run of two-byte runes — mid-rune.
	ev := AmplifierKillEvent{
		SPID: 79, Command: "UPDATE STATISTICS", BlockedBehind: 3,
		Statement: "UPDATE STATISTICS [dbo].[" + strings.Repeat("é", 200) + "]",
	}
	got := AmplifierDetail(ev)
	if !utf8.ValidString(got) {
		t.Errorf("AmplifierDetail() = %q, want valid UTF-8 — the excerpt split a rune", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("AmplifierDetail() = %q, want the truncation marker", got)
	}
}

func TestVictimKeyIdentifiesTheJobNotTheSession(t *testing.T) {
	// Two different sessions running two different statements for the same Agent job step
	// are one offender.
	a := mssql.Session{SPID: 79, Program: `SQLAgent - TSQL JobStep (Job 0x1A2B : Step 1)`,
		Command: "UPDATE STATISTICS", ActiveQuery: "UPDATE STATISTICS dbo.MEASUREMENT"}
	b := mssql.Session{SPID: 91, Program: `SQLAgent - TSQL JobStep (Job 0x1A2B : Step 1)`,
		Command: "ALTER INDEX", ActiveQuery: "ALTER INDEX ALL ON dbo.MEASUREMENT REORGANIZE"}
	if victimKey(a) != victimKey(b) {
		t.Errorf("one job step must be one identity: %q vs %q", victimKey(a), victimKey(b))
	}

	// A program name that is not a job step falls back to the connection triplet, and the
	// statement is still not part of the identity.
	c := mssql.Session{SPID: 54, Login: `CORP\svc_etl`, Host: "SQLPROD01", Program: "ETL",
		ActiveQuery: "UPDATE STATISTICS dbo.MEASUREMENT"}
	d := mssql.Session{SPID: 61, Login: `CORP\svc_etl`, Host: "SQLPROD01", Program: "ETL",
		ActiveQuery: "ALTER INDEX ALL ON dbo.OTHER REBUILD"}
	if victimKey(c) != victimKey(d) {
		t.Errorf("one connection identity must be one offender: %q vs %q", victimKey(c), victimKey(d))
	}
	if victimKey(a) == victimKey(c) {
		t.Error("a job step and a plain connection must not share an identity")
	}
	// A second step of the same job is a different offender: an operator disables a step,
	// not a session, so the step is the finest identity the advice can act on.
	e := mssql.Session{SPID: 80, Program: `SQLAgent - TSQL JobStep (Job 0x1A2B : Step 2)`}
	if victimKey(a) == victimKey(e) {
		t.Errorf("two steps of one job must not share an identity: %q", victimKey(a))
	}
}

func TestVictimKillerRecidivistDoesNotBuyAFreshDwell(t *testing.T) {
	killed := make(chan int, 8)
	clk := NewManualClock(time.Unix(0, 0))
	k := NewVictimKiller(
		func(_ context.Context, spid int) error { killed <- spid; return nil },
		nil, nil, clk, "SqlGoPace")
	k.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 60 * time.Second})

	// Our DDL is SPID 100. Victim 79 is directly blocked by us and has SPID 91 queued
	// behind it, which is what makes it an amplifier.
	snap := func(victim int) []mssql.Session {
		return []mssql.Session{
			{SPID: 100},
			{SPID: victim, BlockingSPID: 100, Command: "UPDATE STATISTICS",
				Program: `SQLAgent - TSQL JobStep (Job 0x1A2B : Step 1)`},
			{SPID: 91, BlockingSPID: victim, Command: "SELECT"},
		}
	}

	k.consider(context.Background(), snap(79), 100, nil) // banks 0
	clk.Advance(60 * time.Second)
	k.consider(context.Background(), snap(79), 100, nil) // banks 60s -> kill
	if got := <-killed; got != 79 {
		t.Fatalf("expected the first victim killed after its dwell, got %d", got)
	}

	// The job restarts a second later under a new SPID: the identity already paid.
	clk.Advance(time.Second)
	k.consider(context.Background(), snap(155), 100, nil)
	select {
	case got := <-killed:
		if got != 155 {
			t.Fatalf("expected the returning job killed, got %d", got)
		}
	default:
		t.Fatal("a returning offender must be killed at once, no kill was issued")
	}
}

// sharedIdentitySnapshot is ONE offender running two concurrent maintenance sessions:
// our DDL (67) blocks 79 and 80, which carry the same login/host/program triplet — the
// sess: fallback identity every connection of one application shares — and each has a
// reader queued behind it. The program is deliberately not an Agent job step, because
// the sess: key is where the shared-identity case is actually reachable.
func sharedIdentitySnapshot() []mssql.Session {
	out := []mssql.Session{{SPID: 67, Command: "ALTER INDEX", Program: "SqlGoPace"}}
	for i, spid := range []int{79, 80} {
		out = append(out,
			mssql.Session{SPID: spid, Command: "UPDATE STATISTICS", BlockingSPID: 67,
				Login: `CORP\svc_sqlagent`, Host: "SQLPROD01", Program: "MaintenanceSolution",
				Database: "PRODDB", ActiveQuery: "UPDATE STATISTICS [dbo].[MEASUREMENT]"},
			mssql.Session{SPID: 1001 + i, Command: "SELECT", BlockingSPID: spid})
	}
	return out
}

// TestVictimKillerBanksOneIdentityOncePerPoll covers the review finding that the scan
// accrued once per eligible victim: two sessions of one offender eligible on the same
// poll each banked the full inter-poll gap into the same bucket, so the identity crossed
// the dwell in roughly After/N wall-clock and Waited over-reported by up to N times.
// TestVictimKillerTracksIndependentDwellsForTwoVictims cannot catch this — its two
// victims are deliberately DIFFERENT offenders.
func TestVictimKillerBanksOneIdentityOncePerPoll(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	ctx := context.Background()
	snap := sharedIdentitySnapshot()

	f.killer.consider(ctx, snap, 67, nil) // both episodes' first poll: banks nothing
	f.clk.Advance(30 * time.Second)
	f.killer.consider(ctx, snap, 67, nil) // banks the 30s gap ONCE, not once per victim

	if len(f.killed) != 0 {
		t.Fatalf("killed = %v at half the dwell, want none — one identity is banked once per poll, not once per session", f.killed)
	}

	f.clk.Advance(30 * time.Second)
	f.killer.consider(ctx, snap, 67, nil)
	if len(f.killed) != 2 {
		t.Fatalf("killed = %v once the dwell was served, want both sessions of the offender", f.killed)
	}
	for _, ev := range f.events {
		if ev.Waited != 60*time.Second {
			t.Errorf("event for SPID %d reports Waited = %v, want the identity's real debt (60s)", ev.SPID, ev.Waited)
		}
	}
}

// returningJobSnapshot is the PRODDB amplifier under an arbitrary session id: one Agent
// job step (identity 0xAB step 1) blocked by our DDL (67), with one reader queued behind
// it, so the same offender can be replayed after a restart under a new SPID.
func returningJobSnapshot(spid int) []mssql.Session {
	return []mssql.Session{
		{SPID: 67, Command: "ALTER INDEX", Program: "SqlGoPace"},
		{SPID: spid, Command: "UPDATE STATISTICS", BlockingSPID: 67,
			Program: "SQLAgent - TSQL JobStep (Job 0xAB : Step 1)",
			Login:   "svc_agent", Host: "SQLPROD01", Database: "PRODDB"},
		{SPID: 1001, Command: "SELECT", BlockingSPID: spid},
	}
}

// TestVictimKillerReportsTheDebtAsWaited is the victim-side mirror of
// TestBlockerKillerReportsTheDebtAsWaited: a recidivist's event must report the
// IDENTITY's accumulated debt, not its own seconds-old episode — that is the headline
// meaning change of serving the dwell per offender. FirstEligible still dates the new
// episode, and both report routes must carry the same number.
func TestVictimKillerReportsTheDebtAsWaited(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	ctx := context.Background()

	f.killer.consider(ctx, returningJobSnapshot(79), 67, nil)
	f.clk.Advance(60 * time.Second)
	f.killer.consider(ctx, returningJobSnapshot(79), 67, nil)

	// The job restarts a second later under a new SPID: the identity already paid.
	f.clk.Advance(time.Second)
	f.killer.consider(ctx, returningJobSnapshot(155), 67, nil)

	if len(f.events) != 2 || len(f.narrated) != 2 {
		t.Fatalf("events = %d, narrated = %d, want two kills on both routes", len(f.events), len(f.narrated))
	}
	ev := f.events[1]
	if ev.SPID != 155 {
		t.Fatalf("second kill event SPID = %d, want 155", ev.SPID)
	}
	if ev.Waited != 60*time.Second {
		t.Errorf("Waited = %v, want the identity's debt (60s), not the returning episode's 0s", ev.Waited)
	}
	if want := f.clk.Now(); !ev.FirstEligible.Equal(want) {
		t.Errorf("FirstEligible = %v, want %v — it dates THIS episode", ev.FirstEligible, want)
	}
	if f.narrated[1].Waited != ev.Waited {
		t.Errorf("narrated Waited = %v, want the sink event's debt (%v)", f.narrated[1].Waited, ev.Waited)
	}
}

// TestVictimKillerQuietWindowRestoresTheFullDwell pins the victim-side effect of the
// prune at the top of the scan: debt is what an offender owes for THIS bout of trouble,
// so an identity that stays away longer than recidivismWindow is judged from zero again
// rather than being killed on sight forever.
func TestVictimKillerQuietWindowRestoresTheFullDwell(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	ctx := context.Background()

	f.killer.consider(ctx, returningJobSnapshot(79), 67, nil)
	f.clk.Advance(60 * time.Second)
	f.killer.consider(ctx, returningJobSnapshot(79), 67, nil)
	if len(f.killed) != 1 {
		t.Fatalf("setup: killed = %v, want [79] once the dwell elapsed", f.killed)
	}

	f.clk.Advance(recidivismWindow + time.Second)
	f.killer.consider(ctx, returningJobSnapshot(155), 67, nil)
	if len(f.killed) != 1 {
		t.Fatalf("killed = %v — after a quiet window the offender must serve the full dwell again", f.killed)
	}

	f.clk.Advance(60 * time.Second)
	f.killer.consider(ctx, returningJobSnapshot(155), 67, nil)
	if len(f.killed) != 2 || f.killed[1] != 155 {
		t.Fatalf("killed = %v, want the kill once the full dwell was served again", f.killed)
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

// TestVictimKillerSkipsKillWhenTheRunIsShuttingDown covers review finding 1: the pump's
// context is canceled the moment the DDL statement returns, and a victim can reach its
// dwell on that very poll. Starting the KILL there would fail instantly with
// "context canceled" and report a kill failure that never happened. Nothing is attempted
// and nothing is warned about; the episode stays open so the victim is reconsidered later.
func TestVictimKillerSkipsKillWhenTheRunIsShuttingDown(t *testing.T) {
	f := newVictimFixture(t, defaultPolicy())
	snap := amplifierSnapshot(16)
	done, cancel := context.WithCancel(context.Background())
	cancel()

	f.killer.consider(done, snap, 67, nil)
	f.clk.Advance(61 * time.Second)
	f.killer.consider(done, snap, 67, nil)

	if len(f.killed) != 0 {
		t.Errorf("killed = %v, want none — no KILL is started on a context that is already done", f.killed)
	}
	if len(f.raw) != 0 {
		t.Errorf("sink events = %v, want none — nothing failed, so nothing is reported", f.raw)
	}

	// Not written off: the same victim is still killable on a live context.
	f.killer.consider(context.Background(), snap, 67, nil)
	if len(f.killed) != 1 || f.killed[0] != 79 {
		t.Errorf("killed = %v after the run resumed polling, want [79]", f.killed)
	}
}

// TestVictimKillerDetachesTheKillFromThePumpContext covers review finding 1: the KILL and
// the msdb attribution behind it are control work about the statement that just ended, so
// they must survive the pump context dying mid-flight — exactly what runStatement does for
// its own fallback KILL. The KILL is still bounded (killTimeout), so it cannot stall the pump.
func TestVictimKillerDetachesTheKillFromThePumpContext(t *testing.T) {
	clk := NewManualClock(time.Unix(1_800_000_000, 0))
	ctx, cancel := context.WithCancel(context.Background())
	var (
		killErrDuring, attrErrDuring error
		killDeadline                 bool
	)
	k := NewVictimKiller(
		func(kctx context.Context, _ int) error {
			cancel() // the DDL statement returns while the KILL is in flight
			killErrDuring = kctx.Err()
			_, killDeadline = kctx.Deadline()
			return nil
		},
		func(actx context.Context, hex string, step int) (mssql.AgentJob, error) {
			attrErrDuring = actx.Err()
			return mssql.AgentJob{Resolved: true, JobID: hex, StepID: step}, nil
		},
		nil, clk, mssql.AppNamePrefix,
	)
	k.Arm(defaultPolicy())

	snap := amplifierSnapshot(16)
	k.consider(ctx, snap, 67, nil)
	clk.Advance(61 * time.Second)
	k.consider(ctx, snap, 67, nil)

	if killErrDuring != nil {
		t.Errorf("KILL context Err() = %v while the pump context was canceled, want nil (detached)", killErrDuring)
	}
	if !killDeadline {
		t.Error("KILL context has no deadline, want one bounded by killTimeout so a hung KILL cannot stall the pump")
	}
	if attrErrDuring != nil {
		t.Errorf("attribution context Err() = %v after the pump context was canceled, want nil (detached)", attrErrDuring)
	}
}
