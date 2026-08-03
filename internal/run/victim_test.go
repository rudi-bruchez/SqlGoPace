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
