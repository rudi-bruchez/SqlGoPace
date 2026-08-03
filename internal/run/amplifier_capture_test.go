package run

import (
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
