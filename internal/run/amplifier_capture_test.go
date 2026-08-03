package run

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func amplifierEvent(spid int, job mssql.AgentJob) AmplifierKillEvent {
	return AmplifierKillEvent{
		SPID:          spid,
		Command:       "UPDATE STATISTICS",
		Statement:     "UPDATE STATISTICS [dbo].[MEASUREMENT] [PK_MEASUREMENT] WITH MAXDOP 2",
		Database:      "PRODDB",
		Login:         "svc_agent",
		Host:          "SQLPROD01",
		Program:       "SQLAgent - TSQL JobStep (Job 0xAB : Step 1)",
		BlockedBehind: 16,
		WaitedMS:      19_280_023,
		FirstEligible: time.Date(2026, 8, 3, 13, 41, 11, 0, time.UTC),
		Job:           job,
	}
}

func resolvedJob() mssql.AgentJob {
	return mssql.AgentJob{Resolved: true, JobID: "0xAB", JobName: "IndexOptimize - USER_DATABASES", StepID: 1, StepName: "Update statistics"}
}

func TestRenderAmplifiersIncludesVictimAndJob(t *testing.T) {
	acc := &amplifierCapture{}
	acc.add(amplifierEvent(79, resolvedJob()), "2026-08-03T13:42:11Z")
	out := string(renderAmplifiers("032-compress-small.yaml", acc))

	for _, want := range []string{
		"session_id: 79",
		`command: "UPDATE STATISTICS"`,
		"blocked_behind: 16",
		`first_eligible: "2026-08-03T13:41:11Z"`,
		`killed_at: "2026-08-03T13:42:11Z"`,
		`login_name: "svc_agent"`,
		`host_name: "SQLPROD01"`,
		`job_name: "IndexOptimize - USER_DATABASES"`,
		"step_id: 1",
		"resolved: true",
		"sp_update_job",
		"Advisory only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered capture does not contain %q\n---\n%s", want, out)
		}
	}
}

func TestRenderAmplifiersRecordsIdentityWhenUnattributed(t *testing.T) {
	acc := &amplifierCapture{}
	ev := amplifierEvent(79, mssql.AgentJob{})
	ev.Program = "SQLAgent - Job invocation engine"
	acc.add(ev, "2026-08-03T13:42:11Z")
	out := string(renderAmplifiers("m.yaml", acc))

	if !strings.Contains(out, "resolved: false") {
		t.Errorf("want resolved: false for an unattributed kill\n---\n%s", out)
	}
	if strings.Contains(out, "job_name:") {
		t.Errorf("job_name must be omitted when unresolved\n---\n%s", out)
	}
	// The identity fields are what the operator has left; they must never be conditional.
	for _, want := range []string{`login_name: "svc_agent"`, `host_name: "SQLPROD01"`, `app_name: "SQLAgent - Job invocation engine"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered capture does not contain %q\n---\n%s", want, out)
		}
	}
}

func TestRenderAmplifiersDeduplicatesDisableStatements(t *testing.T) {
	acc := &amplifierCapture{}
	acc.add(amplifierEvent(79, resolvedJob()), "2026-08-03T13:42:11Z")
	acc.add(amplifierEvent(84, resolvedJob()), "2026-08-03T14:10:02Z")
	out := string(renderAmplifiers("m.yaml", acc))

	if got := strings.Count(out, "sp_update_job"); got != 1 {
		t.Errorf("sp_update_job appears %d times, want 1 — jobs are deduplicated", got)
	}
	if got := strings.Count(out, "session_id:"); got != 2 {
		t.Errorf("session_id appears %d times, want 2 — each kill is its own entry", got)
	}
}

func TestAmplifierCaptureJobsAreDistinctAndOrdered(t *testing.T) {
	acc := &amplifierCapture{}
	acc.add(amplifierEvent(79, resolvedJob()), "t1")
	acc.add(amplifierEvent(84, resolvedJob()), "t2")
	other := mssql.AgentJob{Resolved: true, JobID: "0xCD", JobName: "Nightly stats", StepID: 2}
	acc.add(amplifierEvent(90, other), "t3")

	jobs := acc.jobs()
	if len(jobs) != 2 {
		t.Fatalf("jobs() = %v, want 2 distinct entries", jobs)
	}
	if !strings.Contains(jobs[0], "IndexOptimize - USER_DATABASES") || !strings.Contains(jobs[1], "Nightly stats") {
		t.Errorf("jobs() = %v, want first-seen order", jobs)
	}
}

func TestAmplifierCaptureJobsFallsBackToProgram(t *testing.T) {
	acc := &amplifierCapture{}
	ev := amplifierEvent(79, mssql.AgentJob{})
	ev.Program = "SQLAgent - Job invocation engine"
	acc.add(ev, "t1")
	acc.add(ev, "t2")

	jobs := acc.jobs()
	if len(jobs) != 1 || !strings.Contains(jobs[0], "SQLAgent - Job invocation engine") {
		t.Errorf("jobs() = %v, want one entry keyed on the raw program name", jobs)
	}
}

// amplifierEngine builds an Engine over a fresh queue layout with the manifest already
// in processing, plus the per-operation sink processOne installs on the victim killer.
func amplifierEngine(t *testing.T, name string) (*Engine, *amplifierCapture, ReactionSink) {
	t.Helper()
	root := t.TempDir()
	dirs := Dirs{
		ToRun:      filepath.Join(root, "01.to_run"),
		Processing: filepath.Join(root, "02.processing"),
		Done:       filepath.Join(root, "03.done"),
		Failed:     filepath.Join(root, "04.failed"),
	}
	if err := NewQueue(dirs).EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirs.Processing, name), []byte("operations: []\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	e := &Engine{dirs: dirs, queue: NewQueue(dirs), clk: System, out: io.Discard}
	acc := &amplifierCapture{}
	sink := func(ev ReactionEvent) {
		if ev.Amplifier == nil {
			return
		}
		acc.add(*ev.Amplifier, e.now())
		e.flushAmplifiers(name, acc)
	}
	return e, acc, sink
}

// TestStopVictimsPreventsSidecarResurrection covers review finding 2: processOne's
// `defer e.victims.Disarm()` runs AFTER finalize has relocated the sidecars, so a kill
// landing in that window reached the still-installed sink and re-created
// .amplifiers.yaml in processing — a file nothing ever cleans up. Every finalize path
// now stops the killer before any relocation.
func TestStopVictimsPreventsSidecarResurrection(t *testing.T) {
	const name = "032-compress-small.yaml"
	e, acc, sink := amplifierEngine(t, name)
	f := newVictimFixture(t, defaultPolicy())
	f.killer.SetSink(sink)
	e.victims = f.killer

	// A kill during the run writes the sidecar next to the manifest in processing.
	snap := amplifierSnapshot(16)
	f.killer.consider(context.Background(), snap, 67, nil)
	f.clk.Advance(61 * time.Second)
	f.killer.consider(context.Background(), snap, 67, nil)
	sidecar := filepath.Join(e.dirs.Processing, name+amplifierCaptureSuffix)
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar not written during the run: %v", err)
	}

	// Finalize: the killer stops first, then the manifest and its sidecars are moved.
	e.stopVictims()
	if err := e.queue.Fail(name); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	e.relocateCaptures(name, e.dirs.Failed)
	if _, err := os.Stat(filepath.Join(e.dirs.Failed, name+amplifierCaptureSuffix)); err != nil {
		t.Fatalf("sidecar not relocated to failed: %v", err)
	}

	// A late poll that would have killed another amplifier must reach nothing.
	// A second amplifier appears (SPID 84, with its own readers queued behind it).
	late := amplifierSnapshot(16)
	for i := range late {
		if late[i].SPID == 79 {
			late[i].SPID = 84
		}
		if late[i].BlockingSPID == 79 {
			late[i].BlockingSPID = 84
		}
	}
	f.killer.consider(context.Background(), late, 67, nil)
	f.clk.Advance(61 * time.Second)
	f.killer.consider(context.Background(), late, 67, nil)
	if len(f.killed) != 1 {
		t.Errorf("killed = %v after finalize, want only the one kill from the run — the killer is disarmed", f.killed)
	}
	if acc.len() != 1 {
		t.Errorf("capture holds %d kills after finalize, want 1 — the sink is cleared", acc.len())
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) err = %v, want not-exist — finalize already relocated it", sidecar, err)
	}
}

// TestFlushAmplifiersSkipsWhenTheManifestLeftProcessing covers the other half of review
// finding 2: a KILL already in flight when the manifest finalized still calls the sink it
// snapshotted, so ordering alone cannot close the window. The write is anchored to the
// manifest still being in processing.
func TestFlushAmplifiersSkipsWhenTheManifestLeftProcessing(t *testing.T) {
	const name = "032-compress-small.yaml"
	e, acc, _ := amplifierEngine(t, name)
	acc.add(amplifierEvent(79, resolvedJob()), "2026-08-03T13:42:11Z")

	e.flushAmplifiers(name, acc)
	sidecar := filepath.Join(e.dirs.Processing, name+amplifierCaptureSuffix)
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar not written while the manifest is in processing: %v", err)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	// The manifest has finalized: it left processing and its sidecar went with it.
	if err := e.queue.Fail(name); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	e.flushAmplifiers(name, acc)
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) err = %v, want not-exist — the manifest is no longer in processing", sidecar, err)
	}
}

// blockedSessionsReader serves a fixed session snapshot to captureBlockers; the
// HeldObjectLocks half of BlockerReader is unused here.
type blockedSessionsReader struct{ sessions []mssql.Session }

func (r blockedSessionsReader) ActiveSessions(context.Context) ([]mssql.Session, error) {
	return r.sessions, nil
}
func (r blockedSessionsReader) HeldObjectLocks(context.Context, int) ([]mssql.LockedObject, error) {
	return nil, nil
}

// ddlSession is the executing session captureBlockers reads its SPID from.
type ddlSession struct{ spid int }

func (s ddlSession) SPID() int                                 { return s.spid }
func (s ddlSession) LoginTime(context.Context) (string, error) { return "", nil }
func (s ddlSession) SetMarker(context.Context, [16]byte) error { return nil }

// TestCaptureBlockersSkipsSuppressedVictim covers review finding 5: a victim pending a
// kill was still written into .blocked.yaml — whose stated purpose is "paste this into
// ignore_blocked_sessions" — and counted in peakBlocked, moments before the same session
// was killed and written into .amplifiers.yaml. The two files carry opposite
// instructions, so the capture now mirrors ServerSampler.Blocking and skips it.
func TestCaptureBlockersSkipsSuppressedVictim(t *testing.T) {
	const name = "032-compress-small.yaml"
	e, _, _ := amplifierEngine(t, name)
	snap := amplifierSnapshot(5)
	e.blockers = blockedSessionsReader{sessions: snap}
	e.session = ddlSession{spid: 67}

	f := newVictimFixture(t, defaultPolicy())
	e.victims = f.killer
	f.killer.consider(context.Background(), snap, 67, nil) // 79 is now kill-pending

	acc := &blockerCapture{}
	blocked := e.captureBlockers(context.Background(), nil, acc, name)
	if blocked != 0 {
		t.Errorf("captureBlockers() = %d, want 0 — the only session we block is a suppressed victim", blocked)
	}
	if acc.len() != 0 {
		t.Errorf("capture holds %d session(s), want 0 — a pending-kill victim belongs in .amplifiers.yaml, not .blocked.yaml", acc.len())
	}
	if _, err := os.Stat(filepath.Join(e.dirs.Processing, name+blockedCaptureSuffix)); !os.IsNotExist(err) {
		t.Errorf("os.Stat(.blocked.yaml) err = %v, want not-exist", err)
	}

	// A session we block that is NOT a victim is still captured, unchanged.
	app := mssql.Session{SPID: 91, Command: "SELECT", BlockingSPID: 67, Login: "app_user", Host: "SQLPROD01"}
	e.blockers = blockedSessionsReader{sessions: append(snap, app)}
	if blocked := e.captureBlockers(context.Background(), nil, acc, name); blocked != 1 {
		t.Errorf("captureBlockers() = %d with an application session blocked, want 1", blocked)
	}
}
