# Amplifying Maintenance Victim Kill — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a SqlGoPace operation blocks a SQL Server maintenance statement that has other sessions queued behind it, terminate that statement instead of yielding our own operation, attribute it to its SQL Agent job, and report it.

**Architecture:** A `VictimKiller` mirrors the existing `BlockerKiller` (`internal/run/kill.go`), consulted from `ServerSampler.Blocking` on the `ActiveSessions` snapshot that poll already reads. A kill-eligible victim contributes to `BlockState.Any` but not `BlockState.Unignored`, so the yield timer does not fire while the kill is pending; `max_block_minutes` still backstops. Job attribution parses `program_name` and resolves the name from `msdb`. Everything decision-shaped is a pure function over data.

**Tech Stack:** Go 1.x, standard library only (`regexp`, `strings`, `sync`, `time`), `database/sql` with `github.com/microsoft/go-mssqldb`. Tests are stdlib `testing`, table-driven, no external assertion library.

**Spec:** `docs/superpowers/specs/2026-08-03-amplifying-maintenance-victim-design.md`. Read it before starting. Section references below (§1.2, §2.2, …) point into it.

## Global Constraints

- **English only** — all code, comments, identifiers, file names, and docs. US spelling.
- **Idiomatic Go, KISS.** No new interfaces, generics, or abstraction layers beyond what a task's tests require. Match the surrounding file's style.
- **Never add a query timeout.** Do not wrap DDL execution in `context.WithTimeout`.
- **Version file:** bump `internal/version/VERSION` to `0.13.0` in the final task only.
- **No reformatting.** The repo is CRLF; a newer `gofmt` flags every file. Do **not** run `gofmt -w` across the repo and do **not** run `make lint` as a gate. Gate on `make build`, `make vet`, and `make test`.
- **Test command:** `go test -race ./...` from the repo root. Per-package: `go test -race ./internal/run -run TestName -v`.
- **Windows binary lock:** `bin/sqlgopace.exe` is locked while running. Stop any running instance before `make build`.
- **Commit after every task.** Conventional Commits (`feat:`, `test:`, `docs:`, `refactor:`). End every commit message with:
  `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/mssql/maintenance.go` (modify) | Add `IsAmplifyingCommand` beside the existing shrink-facing `IsMaintenanceCommand`. Command classification only. |
| `internal/mssql/agent.go` (create) | `ParseJobStepProgram` (pure) + `AgentJob` lookup against msdb. SQL Agent attribution only. |
| `internal/mssql/server.go` (modify) | Read `ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY` from `sys.database_scoped_configurations`. |
| `internal/run/chain.go` (create) | Pure blocking-graph functions: `BlockedBehind` (transitive fan-out, cycle-safe) and `DirectVictims`. No state. |
| `internal/run/victim.go` (create) | `VictimKiller`: eligibility, per-SPID episode state, the kill, the grace window. |
| `internal/run/amplifier_capture.go` (create) | `amplifierCapture` accumulator (mutex-guarded) + `.amplifiers.yaml` rendering. |
| `internal/run/async_stats_advisory.go` (create) | Pure advisory decision function, shaped like `reorgRCSIWarning`. |
| `internal/run/executor.go` (modify) | `ServerSampler.SetVictimKiller`; the suppression branch in `Blocking`. |
| `internal/run/engine.go` (modify) | `WithVictimKiller` option; arm/disarm per manifest; emit the advisory; flush + relocate the sidecar. |
| `internal/run/capture.go` (modify) | One line in `relocateCaptures` for the new suffix. |
| `internal/config/config.go` (modify) | `KillAmplifyingMaintenanceConfig`, defaults, validation. |
| `internal/tui/model.go`, `view.go` (modify) | `ConflictingJobsMsg` with replace semantics; render it. |
| `cmd/sqlgopace/main.go` (modify) | Construct and wire the killer; forward events to stdout and the TUI. |

Tasks 1–4 are pure and independently testable with no database and no engine. Tasks 5–7 assemble them. Task 8 wires the binary. Task 9 documents.

---

### Task 1: Amplifying-command classifier

**Files:**
- Modify: `internal/mssql/maintenance.go`
- Test: `internal/mssql/maintenance_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func IsAmplifyingCommand(cmd string, allow []string) bool` — reports whether `cmd` (a `sys.dm_exec_requests.command` verb) is a maintenance statement whose blocked `Sch-M` request amplifies our block. `allow` overrides the built-in list; nil or empty means the built-in list. Also `func DefaultAmplifyingCommands() []string` returning a copy of the built-in list, for config validation and docs.

- [ ] **Step 1: Write the failing test**

Append to `internal/mssql/maintenance_test.go`:

```go
func TestIsAmplifyingCommand(t *testing.T) {
	tests := []struct {
		name  string
		cmd   string
		allow []string
		want  bool
	}{
		{name: "update statistics singular verb", cmd: "UPDATE STATISTIC", want: true},
		{name: "update statistics plural verb", cmd: "UPDATE STATISTICS", want: true},
		{name: "alter index", cmd: "ALTER INDEX", want: true},
		{name: "alter table", cmd: "ALTER TABLE", want: true},
		{name: "create index", cmd: "CREATE INDEX", want: true},
		{name: "create statistics", cmd: "CREATE STATISTICS", want: true},
		{name: "drop index", cmd: "DROP INDEX", want: true},
		{name: "drop table", cmd: "DROP TABLE", want: true},
		{name: "truncate table", cmd: "TRUNCATE TABLE", want: true},
		{name: "dbcc", cmd: "DBCC", want: true},
		{name: "lowercase and padded", cmd: "  alter index  ", want: true},
		{name: "select is not amplifying", cmd: "SELECT", want: false},
		{name: "insert is not amplifying", cmd: "INSERT", want: false},
		{name: "backup is not amplifying", cmd: "BACKUP DATABASE", want: false},
		{name: "empty is not amplifying", cmd: "", want: false},
		{name: "override narrows to stats only", cmd: "ALTER INDEX", allow: []string{"UPDATE STATISTIC"}, want: false},
		{name: "override matches its own entry", cmd: "UPDATE STATISTICS", allow: []string{"UPDATE STATISTIC"}, want: true},
		{name: "override entry is case folded", cmd: "UPDATE STATISTICS", allow: []string{"update statistic"}, want: true},
		{name: "empty override falls back to built-in", cmd: "ALTER INDEX", allow: []string{}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mssql.IsAmplifyingCommand(tt.cmd, tt.allow); got != tt.want {
				t.Errorf("IsAmplifyingCommand(%q, %v) = %v, want %v", tt.cmd, tt.allow, got, tt.want)
			}
		})
	}
}

func TestDefaultAmplifyingCommandsIsACopy(t *testing.T) {
	a := mssql.DefaultAmplifyingCommands()
	if len(a) == 0 {
		t.Fatal("DefaultAmplifyingCommands() is empty")
	}
	a[0] = "MUTATED"
	if b := mssql.DefaultAmplifyingCommands(); b[0] == "MUTATED" {
		t.Error("DefaultAmplifyingCommands() returned the backing array, not a copy")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/mssql -run TestIsAmplifyingCommand -v`
Expected: FAIL — `undefined: mssql.IsAmplifyingCommand`

- [ ] **Step 3: Write minimal implementation**

Append to `internal/mssql/maintenance.go`:

```go
// amplifyingCommands are the dm_exec_requests.command verbs whose blocked Sch-M
// request converts an online operation into a full-table outage: every reader
// arriving afterwards queues behind the waiting Sch-M rather than barging past it.
// Prefix-matched, so "UPDATE STATISTIC" covers both spellings SQL Server reports.
var amplifyingCommands = []string{
	"ALTER INDEX",
	"ALTER TABLE",
	"CREATE INDEX",
	"CREATE STATISTICS",
	"UPDATE STATISTIC",
	"DROP INDEX",
	"DROP TABLE",
	"TRUNCATE TABLE",
	"DBCC",
}

// DefaultAmplifyingCommands returns a copy of the built-in allow-list, for config
// validation and for documenting the effective set.
func DefaultAmplifyingCommands() []string {
	out := make([]string, len(amplifyingCommands))
	copy(out, amplifyingCommands)
	return out
}

// IsAmplifyingCommand reports whether cmd is a maintenance statement worth killing
// when it is blocked by our operation with other sessions queued behind it. allow
// replaces the built-in list when non-empty (never extends it); an absent or empty
// allow means the built-in list, never "match nothing". Matching is case-insensitive,
// space-trimmed, and by prefix. It is deliberately separate from IsMaintenanceCommand,
// which answers a different question for the shrink driver and must not change.
func IsAmplifyingCommand(cmd string, allow []string) bool {
	c := strings.ToUpper(strings.TrimSpace(cmd))
	if c == "" {
		return false
	}
	list := amplifyingCommands
	if len(allow) > 0 {
		list = allow
	}
	for _, want := range list {
		if strings.HasPrefix(c, strings.ToUpper(strings.TrimSpace(want))) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/mssql -run 'TestIsAmplifyingCommand|TestDefaultAmplifyingCommands' -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/mssql/maintenance.go internal/mssql/maintenance_test.go
git commit -m "feat(mssql): classify amplifying maintenance commands"
```

---

### Task 2: Blocking-chain fan-out

**Files:**
- Create: `internal/run/chain.go`
- Test: `internal/run/chain_test.go`

**Interfaces:**
- Consumes: `mssql.Session` (fields `SPID`, `BlockingSPID`), `mssql.Session.BlockedBy`.
- Produces:
  - `func BlockedBehind(sessions []mssql.Session, spid int) int` — count of sessions transitively blocked behind `spid`, excluding `spid` itself. Cycle-safe.
  - `func DirectVictims(sessions []mssql.Session, ddlSPID int) []mssql.Session` — sessions whose `BlockingSPID` is exactly `ddlSPID`, in snapshot order.

- [ ] **Step 1: Write the failing test**

Create `internal/run/chain_test.go`. Every test file in `internal/run` is an **internal** test (`package run`, not `run_test`) — follow that, and drop the `run.` qualifiers accordingly.

```go
package run

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// chainSess builds a session blocked by blockedBy (0 = not blocked). Named to avoid
// shadowing the `sess` range variable ServerSampler.Blocking uses.
func chainSess(spid, blockedBy int) mssql.Session {
	return mssql.Session{SPID: spid, BlockingSPID: blockedBy}
}

func TestBlockedBehind(t *testing.T) {
	tests := []struct {
		name     string
		sessions []mssql.Session
		spid     int
		want     int
	}{
		{
			name:     "nothing queued behind",
			sessions: []mssql.Session{chainSess(67, 0), chainSess(79, 67)},
			spid:     79,
			want:     0,
		},
		{
			name: "the PRODDB shape: one victim, sixteen readers",
			sessions: append([]mssql.Session{chainSess(67, 0), chainSess(79, 67), chainSess(119, 0)},
				func() []mssql.Session {
					var out []mssql.Session
					for _, s := range []int{91, 54, 109, 64, 176, 110, 103, 93, 104, 150, 161, 69, 182, 147, 180, 130} {
						out = append(out, chainSess(s, 79))
					}
					return out
				}()...),
			spid: 79,
			want: 16,
		},
		{
			name:     "transitive depth is counted",
			sessions: []mssql.Session{chainSess(1, 0), chainSess(2, 1), chainSess(3, 2), chainSess(4, 3)},
			spid:     1,
			want:     3,
		},
		{
			name:     "unrelated blocked sessions are not counted",
			sessions: []mssql.Session{chainSess(1, 0), chainSess(2, 1), chainSess(8, 0), chainSess(9, 8)},
			spid:     1,
			want:     1,
		},
		{
			name:     "a two-session cycle terminates",
			sessions: []mssql.Session{chainSess(1, 2), chainSess(2, 1)},
			spid:     1,
			want:     1,
		},
		{
			name:     "a three-session cycle terminates",
			sessions: []mssql.Session{chainSess(1, 3), chainSess(2, 1), chainSess(3, 2)},
			spid:     1,
			want:     2,
		},
		{
			name:     "unknown spid has nothing behind it",
			sessions: []mssql.Session{chainSess(1, 0)},
			spid:     999,
			want:     0,
		},
		{
			name:     "empty snapshot",
			sessions: nil,
			spid:     1,
			want:     0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BlockedBehind(tt.sessions, tt.spid); got != tt.want {
				t.Errorf("BlockedBehind(_, %d) = %d, want %d", tt.spid, got, tt.want)
			}
		})
	}
}

func TestDirectVictims(t *testing.T) {
	sessions := []mssql.Session{chainSess(67, 0), chainSess(79, 67), chainSess(91, 79), chainSess(80, 67), chainSess(5, 0)}
	got := DirectVictims(sessions, 67)
	if len(got) != 2 {
		t.Fatalf("DirectVictims returned %d sessions, want 2", len(got))
	}
	if got[0].SPID != 79 || got[1].SPID != 80 {
		t.Errorf("DirectVictims = [%d %d], want [79 80] in snapshot order", got[0].SPID, got[1].SPID)
	}
}

func TestDirectVictimsZeroSPIDMatchesNothing(t *testing.T) {
	sessions := []mssql.Session{chainSess(1, 0), chainSess(2, 0)}
	if got := DirectVictims(sessions, 0); len(got) != 0 {
		t.Errorf("DirectVictims(_, 0) = %d sessions, want 0 — an unknown SPID must never match an idle session", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/run -run 'TestBlockedBehind|TestDirectVictims' -v`
Expected: FAIL — `undefined: BlockedBehind`

- [ ] **Step 3: Write minimal implementation**

Create `internal/run/chain.go`:

```go
package run

import "github.com/rudi-bruchez/SqlGoPace/internal/mssql"

// DirectVictims returns the sessions blocked directly by ddlSPID, in snapshot order.
// A zero ddlSPID matches nothing, so an unknown session id can never be mistaken for
// one our operation is blocking.
func DirectVictims(sessions []mssql.Session, ddlSPID int) []mssql.Session {
	var out []mssql.Session
	for _, s := range sessions {
		if s.BlockedBy(ddlSPID) {
			out = append(out, s)
		}
	}
	return out
}

// BlockedBehind counts the sessions transitively blocked behind spid, excluding spid
// itself: the fan-out that makes a blocked Sch-M request an amplifier rather than a
// lone waiter.
//
// The walk carries a visited set. A blocking graph assembled row by row from a DMV
// under concurrency is not guaranteed acyclic, and an unguarded walk would not
// terminate.
func BlockedBehind(sessions []mssql.Session, spid int) int {
	if spid == 0 {
		return 0
	}
	behind := make(map[int][]int, len(sessions))
	for _, s := range sessions {
		if s.BlockingSPID != 0 {
			behind[s.BlockingSPID] = append(behind[s.BlockingSPID], s.SPID)
		}
	}
	visited := map[int]bool{spid: true}
	queue, count := []int{spid}, 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range behind[cur] {
			if visited[next] {
				continue
			}
			visited[next] = true
			count++
			queue = append(queue, next)
		}
	}
	return count
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/run -run 'TestBlockedBehind|TestDirectVictims' -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/run/chain.go internal/run/chain_test.go
git commit -m "feat(run): count sessions blocked behind a victim, cycle-safe"
```

---

### Task 3: SQL Agent job attribution

**Files:**
- Create: `internal/mssql/agent.go`
- Test: `internal/mssql/agent_test.go`
- Test (integration): `internal/mssql/agent_integration_test.go`

**Interfaces:**
- Consumes: `*Conn` and its `pool` field (see `internal/mssql/conn.go` for how other reads use `c.pool.QueryRowContext`).
- Produces:
  - `type AgentJob struct { Resolved bool; JobID string; JobName string; StepID int; StepName string }`
  - `func ParseJobStepProgram(program string) (jobIDHex string, stepID int, ok bool)` — pure.
  - `func (c *Conn) LookupAgentJob(ctx context.Context, jobIDHex string, stepID int) (AgentJob, error)`
  - `func (j AgentJob) DisableStatement() string` — the ready-to-paste `sp_update_job` call, `""` when unresolved.

- [ ] **Step 1: Write the failing test**

Create `internal/mssql/agent_test.go`:

```go
package mssql_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestParseJobStepProgram(t *testing.T) {
	tests := []struct {
		name    string
		program string
		wantHex string
		wantStp int
		wantOK  bool
	}{
		{
			name:    "canonical form",
			program: "SQLAgent - TSQL JobStep (Job 0x9B3C1A2D4E5F60718293A4B5C6D7E8F9 : Step 1)",
			wantHex: "0x9B3C1A2D4E5F60718293A4B5C6D7E8F9",
			wantStp: 1,
			wantOK:  true,
		},
		{
			name:    "multi-digit step",
			program: "SQLAgent - TSQL JobStep (Job 0xAB : Step 12)",
			wantHex: "0xAB",
			wantStp: 12,
			wantOK:  true,
		},
		{
			name:    "extra internal whitespace is tolerated",
			program: "SQLAgent  -  TSQL JobStep (Job  0xAB  :  Step  3)",
			wantHex: "0xAB",
			wantStp: 3,
			wantOK:  true,
		},
		{
			name:    "lowercase hex",
			program: "SQLAgent - TSQL JobStep (Job 0xab3c : Step 2)",
			wantHex: "0xab3c",
			wantStp: 2,
			wantOK:  true,
		},
		{name: "cmdexec step does not match", program: "SQLAgent - Job invocation engine", wantOK: false},
		{name: "ordinary application", program: "Microsoft SQL Server Management Studio - Query", wantOK: false},
		{name: "empty", program: "", wantOK: false},
		{name: "truncated fails closed", program: "SQLAgent - TSQL JobStep (Job 0x9B3C", wantOK: false},
		{name: "missing step fails closed", program: "SQLAgent - TSQL JobStep (Job 0x9B3C : Step )", wantOK: false},
		{name: "non-hex job id fails closed", program: "SQLAgent - TSQL JobStep (Job 0xZZZZ : Step 1)", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hex, step, ok := mssql.ParseJobStepProgram(tt.program)
			if ok != tt.wantOK {
				t.Fatalf("ParseJobStepProgram(%q) ok = %v, want %v", tt.program, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if hex != tt.wantHex || step != tt.wantStp {
				t.Errorf("ParseJobStepProgram(%q) = (%q, %d), want (%q, %d)", tt.program, hex, step, tt.wantHex, tt.wantStp)
			}
		})
	}
}

func TestAgentJobDisableStatement(t *testing.T) {
	j := mssql.AgentJob{Resolved: true, JobName: "IndexOptimize - USER_DATABASES"}
	want := `EXEC msdb.dbo.sp_update_job @job_name = N'IndexOptimize - USER_DATABASES', @enabled = 0;`
	if got := j.DisableStatement(); got != want {
		t.Errorf("DisableStatement() = %q, want %q", got, want)
	}
}

func TestAgentJobDisableStatementEscapesQuote(t *testing.T) {
	j := mssql.AgentJob{Resolved: true, JobName: "Bob's job"}
	want := `EXEC msdb.dbo.sp_update_job @job_name = N'Bob''s job', @enabled = 0;`
	if got := j.DisableStatement(); got != want {
		t.Errorf("DisableStatement() = %q, want %q", got, want)
	}
}

func TestAgentJobDisableStatementEmptyWhenUnresolved(t *testing.T) {
	if got := (mssql.AgentJob{}).DisableStatement(); got != "" {
		t.Errorf("DisableStatement() on an unresolved job = %q, want \"\"", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/mssql -run 'TestParseJobStepProgram|TestAgentJob' -v`
Expected: FAIL — `undefined: mssql.ParseJobStepProgram`

- [ ] **Step 3: Write minimal implementation**

Create `internal/mssql/agent.go`:

```go
package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// AgentJob identifies the SQL Agent job step a session was running. Resolved is false
// when the session is not an Agent T-SQL step, or when msdb could not be read; the
// caller then falls back to the raw program/login/host it already has.
type AgentJob struct {
	Resolved bool
	JobID    string // the 0x… literal from program_name
	JobName  string
	StepID   int
	StepName string
}

// jobStepProgram matches the program_name a SQL Agent T-SQL job step sets:
// "SQLAgent - TSQL JobStep (Job 0x<hex job_id> : Step <n>)". Internal runs of
// whitespace are tolerated because the exact spacing has varied across versions.
// Anything else — a CmdExec step, an application, a truncated string — fails to
// match, and attribution degrades rather than guessing.
var jobStepProgram = regexp.MustCompile(
	`^SQLAgent\s*-\s*TSQL\s+JobStep\s*\(\s*Job\s+(0[xX][0-9A-Fa-f]+)\s*:\s*Step\s+(\d+)\s*\)`)

// ParseJobStepProgram extracts the job id literal and step number from an Agent T-SQL
// step's program_name. ok is false for any other program name.
func ParseJobStepProgram(program string) (jobIDHex string, stepID int, ok bool) {
	m := jobStepProgram.FindStringSubmatch(strings.TrimSpace(program))
	if m == nil {
		return "", 0, false
	}
	step, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return m[1], step, true
}

// DisableStatement renders the ready-to-paste call that disables the job, or "" when
// the job was never resolved. Advisory only: SqlGoPace never executes it.
func (j AgentJob) DisableStatement() string {
	if !j.Resolved || j.JobName == "" {
		return ""
	}
	return fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @enabled = 0;",
		strings.ReplaceAll(j.JobName, "'", "''"))
}

// The uniqueidentifier conversion is done in T-SQL on purpose: the binary layout of a
// GUID is mixed-endian, and reimplementing it in Go is a needless source of bugs.
const agentJobSQL = `
SELECT j.name, s.step_name
FROM msdb.dbo.sysjobs j
LEFT JOIN msdb.dbo.sysjobsteps s ON s.job_id = j.job_id AND s.step_id = @step
WHERE j.job_id = CONVERT(uniqueidentifier, CONVERT(varbinary(16), @hex, 1));`

// LookupAgentJob resolves an Agent job id literal and step number to their names.
// A missing job (deleted between the read and the lookup) returns an unresolved
// AgentJob and no error; only a genuine query failure is an error, and the caller
// treats that as "attribution unavailable".
func (c *Conn) LookupAgentJob(ctx context.Context, jobIDHex string, stepID int) (AgentJob, error) {
	var name, stepName sql.NullString
	err := c.pool.QueryRowContext(ctx, agentJobSQL,
		sql.Named("hex", jobIDHex), sql.Named("step", stepID)).Scan(&name, &stepName)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AgentJob{JobID: jobIDHex, StepID: stepID}, nil
	case err != nil:
		return AgentJob{}, fmt.Errorf("lookup agent job %s step %d: %w", jobIDHex, stepID, err)
	}
	return AgentJob{
		Resolved: true,
		JobID:    jobIDHex,
		JobName:  name.String,
		StepID:   stepID,
		StepName: stepName.String,
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/mssql -run 'TestParseJobStepProgram|TestAgentJob' -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Add the integration test**

Create `internal/mssql/agent_integration_test.go`. It reuses `openTestConn(t) (*mssql.Conn, context.Context)` from `internal/mssql/integration_test.go`, which already handles the `SQLGOPACE_TEST_DSN` skip and the connection cleanup.

```go
//go:build integration

package mssql_test

import (
	"context"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// TestIntegrationLookupAgentJobUnknownID pins the uniqueidentifier conversion in
// agentJobSQL against a real server: a well-formed but unknown job id must round-trip
// through CONVERT(uniqueidentifier, CONVERT(varbinary(16), @hex, 1)) and return no
// rows, rather than raising a conversion error. A conversion bug surfaces as an error
// here, which is the thing worth catching.
func TestIntegrationLookupAgentJobUnknownID(t *testing.T) {
	conn, ctx := openTestConn(t)

	job, err := conn.LookupAgentJob(ctx, "0x00000000000000000000000000000000", 1)
	if err != nil {
		t.Fatalf("LookupAgentJob() error = %v, want nil for an unknown job id", err)
	}
	if job.Resolved {
		t.Errorf("LookupAgentJob() resolved an all-zero job id: %+v", job)
	}
}

// TestIntegrationUpdateStatisticsCommandVerb pins the exact dm_exec_requests.command
// verb this server reports for a running UPDATE STATISTICS. IsAmplifyingCommand
// prefix-matches it, and if a future version renames the verb the feature would go
// silently inert — so this fails loudly instead.
//
// The statement is only visible in dm_exec_requests WHILE it runs, so the probe
// samples in flight: a wide scratch table plus FULLSCAN keeps the update running long
// enough to catch across a tight poll.
func TestIntegrationUpdateStatisticsCommandVerb(t *testing.T) {
	conn, ctx := openTestConn(t)

	const table = "dbo.sqlgopace_stats_probe"
	setup := []string{
		`IF OBJECT_ID('` + table + `') IS NOT NULL DROP TABLE ` + table + `;`,
		`SELECT TOP (500000)
		     CAST(ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS bigint) AS id,
		     REPLICATE('x', 200) AS pad
		 INTO ` + table + `
		 FROM sys.all_objects a CROSS JOIN sys.all_objects b;`,
		`CREATE INDEX IX_sqlgopace_stats_probe ON ` + table + `(id);`,
	}
	for _, stmt := range setup {
		if err := conn.ExecDDL(ctx, stmt); err != nil {
			t.Fatalf("setup %q error = %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_ = conn.ExecDDL(context.Background(), `IF OBJECT_ID('`+table+`') IS NOT NULL DROP TABLE `+table+`;`)
	})

	// A second connection runs the UPDATE STATISTICS so the first can sample it.
	worker, err := mssql.Open(ctx, dsn(t), "test")
	if err != nil {
		t.Fatalf("Open() worker error = %v", err)
	}
	t.Cleanup(func() { _ = worker.Close() })

	done := make(chan error, 1)
	go func() {
		done <- worker.ExecDDL(ctx, `UPDATE STATISTICS `+table+` WITH FULLSCAN;`)
	}()

	verb := ""
	deadline := time.Now().Add(30 * time.Second)
	for verb == "" && time.Now().Before(deadline) {
		sessions, err := conn.ActiveSessions(ctx)
		if err != nil {
			t.Fatalf("ActiveSessions() error = %v", err)
		}
		for _, s := range sessions {
			if s.SPID == worker.SPID() && s.Command != "" {
				verb = s.Command
				break
			}
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("UPDATE STATISTICS error = %v", err)
			}
			deadline = time.Now() // the statement finished; stop polling
		default:
		}
	}
	if verb == "" {
		t.Skip("could not observe the UPDATE STATISTICS in flight; nothing to pin")
	}
	t.Logf("server reports command verb %q for UPDATE STATISTICS", verb)
	if !mssql.IsAmplifyingCommand(verb, nil) {
		t.Errorf("IsAmplifyingCommand(%q) = false — the built-in allow-list prefix no longer "+
			"matches the verb this server reports for UPDATE STATISTICS", verb)
	}
}
```

- [ ] **Step 6: Verify the integration test compiles**

Run: `go vet -tags integration ./internal/mssql`
Expected: no output. (The tests themselves only run with `SQLGOPACE_TEST_DSN` set; compiling them is the gate here.)

- [ ] **Step 7: Commit**

```bash
git add internal/mssql/agent.go internal/mssql/agent_test.go internal/mssql/agent_integration_test.go internal/mssql/integration_test.go
git commit -m "feat(mssql): attribute a session to its SQL Agent job step"
```

---

### Task 4: ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY advisory

**Files:**
- Create: `internal/run/async_stats_advisory.go`
- Test: `internal/run/async_stats_advisory_test.go`
- Modify: `internal/mssql/server.go`

**Interfaces:**
- Consumes: `ddl.Operation`, `ddl.ReorganizeIndex` (fields `Schema`, `Table`).
- Produces:
  - `type AsyncStatsSetting int` with constants `AsyncStatsAbsent`, `AsyncStatsOff`, `AsyncStatsOn`.
  - `func asyncStatsAdvisory(op ddl.Operation, database string, setting AsyncStatsSetting) (string, bool)` — unexported, tested from within the package.
  - `func (c *mssql.Conn) AsyncStatsWaitAtLowPriority(ctx context.Context) (bool, bool, error)` — returns `(on, present, error)`.

Read `internal/run/reorg_rcsi.go` and `internal/run/reorg_rcsi_test.go` first; this is the same shape and must match its style. Note the test file is in package `run` (internal test), not `run_test`.

- [ ] **Step 1: Write the failing test**

Create `internal/run/async_stats_advisory_test.go`:

```go
package run

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestAsyncStatsAdvisory(t *testing.T) {
	reorg := ddl.ReorganizeIndex{Schema: "dbo", Table: "MEASUREMENT"}

	tests := []struct {
		name        string
		op          ddl.Operation
		setting     AsyncStatsSetting
		wantEmit    bool
		mustContain []string
		mustNotHave []string
	}{
		{
			name:     "absent on an older server emits nothing",
			op:       reorg,
			setting:  AsyncStatsAbsent,
			wantEmit: false,
		},
		{
			name:        "off emits the recommendation and the limitation",
			op:          reorg,
			setting:     AsyncStatsOff,
			wantEmit:    true,
			mustContain: []string{"is OFF", "ALTER DATABASE SCOPED CONFIGURATION", "does NOT cover", "PRODDB", "dbo.MEASUREMENT"},
		},
		{
			name:        "on emits the limitation alone",
			op:          reorg,
			setting:     AsyncStatsOn,
			wantEmit:    true,
			mustContain: []string{"does NOT cover"},
			mustNotHave: []string{"ALTER DATABASE SCOPED CONFIGURATION"},
		},
		{
			name:     "not a reorganize emits nothing",
			op:       ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT"},
			setting:  AsyncStatsOff,
			wantEmit: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, ok := asyncStatsAdvisory(tt.op, "PRODDB", tt.setting)
			if ok != tt.wantEmit {
				t.Fatalf("asyncStatsAdvisory() emit = %v, want %v (msg = %q)", ok, tt.wantEmit, msg)
			}
			if !ok {
				return
			}
			for _, want := range tt.mustContain {
				if !strings.Contains(msg, want) {
					t.Errorf("advisory %q does not contain %q", msg, want)
				}
			}
			for _, bad := range tt.mustNotHave {
				if strings.Contains(msg, bad) {
					t.Errorf("advisory %q unexpectedly contains %q", msg, bad)
				}
			}
		})
	}
}
```

`ddl.RebuildIndex` and `ddl.ReorganizeIndex` are both declared in `internal/ddl/manifest.go` (lines 611 and 787) with `Schema`/`Table` fields, so the literals above compile as written.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/run -run TestAsyncStatsAdvisory -v`
Expected: FAIL — `undefined: asyncStatsAdvisory`

- [ ] **Step 3: Write minimal implementation**

Create `internal/run/async_stats_advisory.go`:

```go
package run

import (
	"fmt"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// AsyncStatsSetting is the state of the ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY
// database-scoped configuration, which exists from SQL Server 2022 (major 16).
type AsyncStatsSetting int

const (
	// AsyncStatsAbsent means the setting does not exist on this server.
	AsyncStatsAbsent AsyncStatsSetting = iota
	// AsyncStatsOff means it exists and is off.
	AsyncStatsOff
	// AsyncStatsOn means it exists and is on.
	AsyncStatsOn
)

const asyncStatsLimitation = "It does NOT cover an explicit UPDATE STATISTICS run by a job or by hand; " +
	"those still block and can queue readers behind them."

// asyncStatsAdvisory returns the advisory to emit before op, and whether to emit it.
// Like reorgRCSIWarning it self-gates to REORGANIZE and takes the database name so the
// message is complete and the helper stays pure.
//
// The limitation is stated in every emitted variant, including when the setting is
// already on: the configuration covers only asynchronous automatic statistics updates,
// and an operator who enables it and assumes explicit UPDATE STATISTICS is handled
// will be surprised by exactly the incident this feature exists for.
func asyncStatsAdvisory(op ddl.Operation, database string, setting AsyncStatsSetting) (string, bool) {
	reorg, ok := op.(ddl.ReorganizeIndex)
	if !ok || setting == AsyncStatsAbsent {
		return "", false
	}
	if setting == AsyncStatsOn {
		return fmt.Sprintf(
			"%s.%s: ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY is on for %s. %s",
			reorg.Schema, reorg.Table, database, asyncStatsLimitation), true
	}
	return fmt.Sprintf(
		"%s.%s: ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY is OFF on %s — enabling it "+
			"(ALTER DATABASE SCOPED CONFIGURATION SET ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY = ON) "+
			"lets automatic statistics updates queue at low priority instead of blocking this "+
			"REORGANIZE. %s",
		reorg.Schema, reorg.Table, database, asyncStatsLimitation), true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/run -run TestAsyncStatsAdvisory -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Add the server read**

Append to `internal/mssql/server.go`:

```go
const asyncStatsWALPSQL = `
SELECT CONVERT(bit, value)
FROM sys.database_scoped_configurations
WHERE name = 'ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY';`

// AsyncStatsWaitAtLowPriority reads the ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY
// database-scoped configuration. present is false on a server where the setting does
// not exist (before SQL Server 2022), which is not an error — the caller emits no
// advisory in that case.
func (c *Conn) AsyncStatsWaitAtLowPriority(ctx context.Context) (on bool, present bool, err error) {
	switch err := c.pool.QueryRowContext(ctx, asyncStatsWALPSQL).Scan(&on); {
	case errors.Is(err, sql.ErrNoRows):
		return false, false, nil
	case err != nil:
		return false, false, fmt.Errorf("read ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY: %w", err)
	default:
		return on, true, nil
	}
}
```

Add `"database/sql"` and `"errors"` to the file's imports if they are not already present.

- [ ] **Step 6: Verify the package builds and all tests still pass**

Run: `go build ./... && go test -race ./internal/mssql ./internal/run`
Expected: build succeeds, all tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/run/async_stats_advisory.go internal/run/async_stats_advisory_test.go internal/mssql/server.go
git commit -m "feat(run): advise on ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY before a reorganize"
```

---

### Task 5: VictimKiller

**Files:**
- Create: `internal/run/victim.go`
- Test: `internal/run/victim_test.go`

**Interfaces:**
- Consumes: `run.BlockedBehind`, `run.DirectVictims` (Task 2); `mssql.IsAmplifyingCommand` (Task 1); `run.IgnoredSessions.ignores` (existing, `executor.go`); `run.Clock`, `run.NewManualClock` (existing, `clock.go`).
- Produces:
  - `type AmplifierPolicy struct { MinBlockedBehind int; After time.Duration; Commands []string }`
  - `type AmplifierKillEvent struct { SPID int; Command string; Statement string; Database string; Login string; Host string; Program string; BlockedBehind int; WaitedMS int64; FirstEligible time.Time; Waited time.Duration; Job mssql.AgentJob }`
  - `type VictimKiller struct { ... }`
  - `func NewVictimKiller(kill func(context.Context, int) error, resolve func(context.Context, string, int) (mssql.AgentJob, error), onKill func(AmplifierKillEvent), clk Clock, selfProgram string) *VictimKiller`
  - `func (k *VictimKiller) Arm(p AmplifierPolicy)` / `func (k *VictimKiller) Disarm()`
  - `func (k *VictimKiller) SetSink(sink ReactionSink)` — the engine sets this per operation, where it builds `sink`; `nil` between operations.
  - `func (k *VictimKiller) consider(ctx context.Context, sessions []mssql.Session, ddlSPID int, ignore IgnoredSessions)`
  - `func (k *VictimKiller) Suppressed(spid int) bool` — reports whether `spid` currently must not count toward `BlockState.Unignored`.
- Also modifies `internal/run/reaction.go`: `ReactionEvent` gains `Amplifier *AmplifierKillEvent`.

Read `internal/run/kill.go` end to end first. This type is its mirror and must match its conventions: mutex-guarded state, `nil`-receiver tolerance, episode reset.

**Two output paths, both needed.** The kill has two consumers with different lifetimes.

*Engine state* — the run report and the per-manifest `.amplifiers.yaml` — is reached by emitting a `ReactionEvent` carrying the structured event in a new `Amplifier` field. That is exactly how `TailFinding` already rides `ReactionEvent.Tail` from the shrink driver into `engine.go`'s sink (`engine.go:606`), and that sink is already documented and mutex-guarded for being called from a sibling goroutine — which is what the pump goroutine is.

*Presentation* is a separate `onKill` callback at construction, mirroring `BlockerKiller` (`main.go:395`). This is **not** redundant: in TUI mode `engineOut` is `io.Discard` (`main.go:206`), so the line the engine sink prints goes nowhere and the operator would see no per-kill narration at all. The callback sends a `tui.LogMsg` and prints to the run output.

Add to `internal/run/reaction.go`, beside the existing `Tail` field:

```go
	// Amplifier is non-nil only on a kill of an amplifying maintenance victim: the
	// session this run terminated, for the engine sink to record in the
	// .amplifiers.yaml sidecar and the TUI's conflicting-jobs line. It rides the event
	// for the same reason Tail does — the killer runs on the pump goroutine and has no
	// other route to per-manifest engine state.
	Amplifier *AmplifierKillEvent
```

- [ ] **Step 1: Export the application-name prefix first**

The tests below reference `mssql.AppNamePrefix`, so it must exist before the red step, or Step 3 fails with `undefined: mssql.AppNamePrefix` rather than the failure this task is about.

Self-exclusion needs the same constant `internal/mssql/conn.go` uses for the connection's application name. In `conn.go`, replace the two literal `"SqlGoPace"` occurrences in `appNameWithVersion` with a named constant and export it:

```go
// AppNamePrefix is the application name SqlGoPace connects with, before the version
// suffix appNameWithVersion appends ("SqlGoPace/0.13.0"). It is exported because the
// victim killer matches program_name against it by prefix to avoid ever terminating
// another SqlGoPace session — including one running a different build.
const AppNamePrefix = "SqlGoPace"
```

Run: `go build ./... && go test -race ./internal/mssql`
Expected: builds, tests PASS.

- [ ] **Step 2: Write the failing test**

Create `internal/run/victim_test.go`. Like every test file in this package it is an **internal** test (`package run`), so the identifiers are unqualified and `consider`/`Suppressed` are directly reachable — no `export_test.go` entry is needed.

```go
package run

import (
	"context"
	"errors"
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
```

`amplifierSnapshot` is reused by Task 6's sampler tests, so it must stay at package scope in this file.

Add one more test for the narration, which is the operator-facing artefact and easy to let drift:

```go
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
```

Add `"strings"` to the test imports.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test -race ./internal/run -run 'TestVictimKiller|TestAmplifierDetail' -v`
Expected: FAIL — `undefined: NewVictimKiller`

- [ ] **Step 4: Write minimal implementation**

Create `internal/run/victim.go`:

```go
package run

import (
	"context"
	"sync"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// killGraceWindow is how long a killed victim stays suppressed after a successful
// KILL, so a rollback still visible in the snapshot does not instantly trip the yield
// timer. Time-based rather than a poll count: the blocking poll interval is
// configurable, so "two polls" would mean anything from two seconds to a minute. Not
// configurable — a killed lock request clears from the wait queue almost immediately,
// and this window exists only to absorb one stale snapshot.
const killGraceWindow = 15 * time.Second

// AmplifierPolicy is the armed kill policy for one manifest.
type AmplifierPolicy struct {
	MinBlockedBehind int           // sessions that must be queued behind the victim
	After            time.Duration // how long eligibility must persist before the kill
	Commands         []string      // replaces the built-in allow-list when non-empty
}

// AmplifierKillEvent describes a maintenance victim the run terminated because it was
// amplifying our block. Login/Host/Program/Statement are always populated, whether or
// not Job resolved: when attribution fails they are all the operator has.
type AmplifierKillEvent struct {
	SPID          int
	Command       string
	Statement     string
	Database      string
	Login         string
	Host          string
	Program       string
	BlockedBehind int
	WaitedMS      int64         // the victim's own wait_time, from the DMV
	FirstEligible time.Time     // when it first met every eligibility condition
	Waited        time.Duration // how long it was kill-eligible before we killed it
	Job           mssql.AgentJob
}

// victimEpisode is the per-SPID state of one blocking episode.
type victimEpisode struct {
	since      time.Time // when this victim first became kill-eligible
	killed     bool      // a KILL was issued successfully
	killedAt   time.Time // when, for the grace window
	killFailed bool      // the KILL errored: stop suppressing, fall back to yielding
}

// VictimKiller terminates maintenance statements our operation blocks that have other
// sessions queued behind them, turning an online operation into a full-table outage.
// It is the mirror of BlockerKiller: that one kills sessions blocking us, this one
// kills sessions we block. The two are disjoint by construction — a session appearing
// as our own direct blocker is never a candidate here (see eligible) — so they share
// no episode state and the order they are consulted in does not matter.
//
// It is consulted from ServerSampler.Blocking on the snapshot that poll already reads.
// All state is guarded by mu: consider runs on the pump goroutine while Arm/Disarm run
// on the engine goroutine between manifests.
type VictimKiller struct {
	kill        func(context.Context, int) error
	resolve     func(context.Context, string, int) (mssql.AgentJob, error)
	onKill      func(AmplifierKillEvent) // presentation only (console + TUI); see notify
	clk         Clock
	selfProgram string // our own application name prefix; never kill a session running it

	mu       sync.Mutex
	policy   AmplifierPolicy
	armed    bool
	sink     ReactionSink
	episodes map[int]*victimEpisode
	jobs     map[string]mssql.AgentJob // attribution cache, keyed by "hex:step"
}

// NewVictimKiller builds a killer. kill terminates a SPID (mssql.Conn.Kill on the
// monitoring pool); resolve attributes a program_name to an Agent job (may be nil);
// onKill narrates a successful kill to the console and TUI (may be nil) — a separate
// route from the sink, because in TUI mode the engine's run output is io.Discard;
// clk defaults to System; selfProgram is our own application-name prefix
// (mssql.AppNamePrefix), matched by prefix because program_name carries the build
// version — which also excludes a different SqlGoPace version running concurrently.
func NewVictimKiller(
	kill func(context.Context, int) error,
	resolve func(context.Context, string, int) (mssql.AgentJob, error),
	onKill func(AmplifierKillEvent),
	clk Clock,
	selfProgram string,
) *VictimKiller {
	if clk == nil {
		clk = System
	}
	return &VictimKiller{
		kill: kill, resolve: resolve, onKill: onKill, clk: clk, selfProgram: selfProgram,
		episodes: make(map[int]*victimEpisode),
		// jobs deliberately outlives Arm/Disarm: Agent job ids are globally unique, so
		// an attribution cached under one manifest is still correct under the next, and
		// re-reading msdb per manifest would buy nothing.
		jobs: make(map[string]mssql.AgentJob),
	}
}

// SetSink installs the reaction sink kills are emitted on. The engine sets it per
// operation, where it builds the sink, and clears it (nil) between operations so a
// late kill cannot be attributed to the next operation's report.
func (k *VictimKiller) SetSink(sink ReactionSink) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.sink = sink
	k.mu.Unlock()
}

// Arm enables the killer with p for the current manifest and clears episode state.
func (k *VictimKiller) Arm(p AmplifierPolicy) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.policy, k.armed = p, true
	k.episodes = make(map[int]*victimEpisode)
	k.mu.Unlock()
}

// Disarm disables the killer between manifests and clears episode state and the sink.
func (k *VictimKiller) Disarm() {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.armed, k.sink = false, nil
	k.episodes = make(map[int]*victimEpisode)
	k.mu.Unlock()
}

// Suppressed reports whether spid must not count toward BlockState.Unignored right
// now: a kill is pending, or it was killed within the grace window. A failed KILL
// withdraws suppression immediately, so the feature can never make us block longer
// than it would without it.
func (k *VictimKiller) Suppressed(spid int) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.armed {
		return false
	}
	ep, ok := k.episodes[spid]
	switch {
	case !ok, ep.killFailed:
		return false
	case ep.killed:
		return k.clk.Since(ep.killedAt) < killGraceWindow
	default:
		return true
	}
}

// consider inspects one snapshot, updates episode state for every direct victim, and
// kills the ones that have been eligible for the policy's dwell. A no-op when
// disarmed. Victims absent from this snapshot have their episodes dropped, so the
// dwell restarts if they come back.
//
// It runs on the PUMP goroutine, from ServerSampler.Blocking. A kill (and the msdb
// attribution lookup behind it) is synchronous server I/O on that path, so on a loaded
// server it can delay the next blocking sample past its interval. Acceptable for an
// opt-in feature that fires rarely; do not add per-poll work here casually.
func (k *VictimKiller) consider(ctx context.Context, sessions []mssql.Session, ddlSPID int, ignore IgnoredSessions) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.armed {
		return
	}

	now := k.clk.Now()
	seen := make(map[int]bool)
	for _, v := range DirectVictims(sessions, ddlSPID) {
		if !k.eligible(v, sessions, ddlSPID, ignore) {
			continue
		}
		seen[v.SPID] = true
		ep, ok := k.episodes[v.SPID]
		if !ok {
			ep = &victimEpisode{since: now}
			k.episodes[v.SPID] = ep
		}
		if ep.killed || ep.killFailed || k.clk.Since(ep.since) < k.policy.After {
			continue
		}
		if err := k.kill(ctx, v.SPID); err != nil {
			ep.killFailed = true
			continue
		}
		ep.killed, ep.killedAt = true, now
		k.notify(ctx, v, sessions, ep.since, now.Sub(ep.since))
	}
	// Drop episodes for victims no longer eligible, except those inside their grace
	// window — a killed victim leaves the snapshot, and dropping it early would end
	// the grace suppression the moment it worked.
	for spid, ep := range k.episodes {
		if seen[spid] || (ep.killed && k.clk.Since(ep.killedAt) < killGraceWindow) {
			continue
		}
		delete(k.episodes, spid)
	}
}

// eligible applies the six conditions of the design's §1.3, in cheapest-first order.
func (k *VictimKiller) eligible(v mssql.Session, sessions []mssql.Session, ddlSPID int, ignore IgnoredSessions) bool {
	switch {
	case k.selfProgram != "" && strings.HasPrefix(v.Program, k.selfProgram):
		return false // never kill another SqlGoPace session, whatever its version
	case !mssql.IsAmplifyingCommand(v.Command, k.policy.Commands):
		return false
	case ignore.ignores(v):
		return false // an explicitly ignored victim is never killed
	case k.blocksUs(v.SPID, sessions, ddlSPID):
		return false // our own direct blocker belongs to BlockerKiller
	case BlockedBehind(sessions, v.SPID) < k.policy.MinBlockedBehind:
		return false
	}
	return true
}

// blocksUs reports whether spid is the session our own DDL is waiting on, which makes
// it BlockerKiller's to handle and never this killer's.
func (k *VictimKiller) blocksUs(spid int, sessions []mssql.Session, ddlSPID int) bool {
	return mssql.FindSelfBlock(sessions, ddlSPID).SPID == spid
}

// notify resolves the victim's Agent job (cached) and delivers the kill on both
// routes: the sink (engine — run report and sidecar) and onKill (presentation —
// console and TUI). Called with mu held.
func (k *VictimKiller) notify(ctx context.Context, v mssql.Session, sessions []mssql.Session, firstEligible time.Time, waited time.Duration) {
	stmt := v.ActiveQuery
	if stmt == "" {
		stmt = v.ParentQuery
	}
	ev := AmplifierKillEvent{
		SPID:          v.SPID,
		Command:       v.Command,
		Statement:     stmt,
		Database:      v.Database,
		Login:         v.Login,
		Host:          v.Host,
		Program:       v.Program,
		BlockedBehind: BlockedBehind(sessions, v.SPID),
		WaitedMS:      v.WaitMS,
		FirstEligible: firstEligible,
		Waited:        waited,
		Job:           k.attribute(ctx, v.Program),
	}
	if k.sink != nil {
		k.sink(ReactionEvent{Kind: "kill", Detail: AmplifierDetail(ev), Amplifier: &ev})
	}
	if k.onKill != nil {
		k.onKill(ev)
	}
}

// statementExcerpt trims a statement to a single readable line for the narration.
const statementExcerpt = 120

// AmplifierDetail narrates one kill for the console, the run report and the TUI feed.
// It names the statement, not just the command verb, because "UPDATE STATISTICS" alone
// does not tell the operator which object was involved — and the object is what they
// act on. The source clause names the Agent job when attribution resolved and falls
// back to the raw program/login/host when it did not; that fallback is the whole point
// of recording those fields unconditionally.
//
// Exported so the CLI's presentation callback renders exactly the same line as the
// run report, rather than a second, drifting copy.
func AmplifierDetail(ev AmplifierKillEvent) string {
	subject := ev.Command
	if stmt := strings.Join(strings.Fields(ev.Statement), " "); stmt != "" {
		if len(stmt) > statementExcerpt {
			stmt = stmt[:statementExcerpt] + "…"
		}
		subject = fmt.Sprintf("%s: %s", ev.Command, stmt)
	}
	d := fmt.Sprintf("killed amplifying maintenance session SPID %d (%s) — %d session(s) queued behind it",
		ev.SPID, subject, ev.BlockedBehind)
	switch {
	case ev.Job.Resolved:
		d += fmt.Sprintf("; source: SQL Agent job %q step %d", ev.Job.JobName, ev.Job.StepID)
		if stmt := ev.Job.DisableStatement(); stmt != "" {
			d += fmt.Sprintf(" — consider disabling it during maintenance: %s", stmt)
		}
	case ev.Program != "":
		d += fmt.Sprintf("; source: %s (login=%s host=%s)", ev.Program, ev.Login, ev.Host)
	}
	return d
}

// attribute resolves an Agent job from a program_name, caching per (job, step).
// An unparseable program name, a nil resolver, or an msdb failure all yield an
// unresolved AgentJob — the caller falls back to the raw program/login/host.
func (k *VictimKiller) attribute(ctx context.Context, program string) mssql.AgentJob {
	hex, step, ok := mssql.ParseJobStepProgram(program)
	if !ok || k.resolve == nil {
		return mssql.AgentJob{}
	}
	key := hex + ":" + strconv.Itoa(step)
	if job, hit := k.jobs[key]; hit {
		return job
	}
	job, err := k.resolve(ctx, hex, step)
	if err != nil {
		return mssql.AgentJob{}
	}
	k.jobs[key] = job
	return job
}
```

Add `"fmt"`, `"strconv"` and `"strings"` to the imports.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -race ./internal/run -run 'TestVictimKiller|TestAmplifierDetail' -v`
Expected: PASS, all subtests.

- [ ] **Step 6: Run the whole run package to check for regressions**

Run: `go test -race ./internal/run ./internal/mssql`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/run/victim.go internal/run/victim_test.go internal/run/reaction.go internal/mssql/conn.go
git commit -m "feat(run): kill amplifying maintenance victims of a blocking operation"
```

`export_test.go` is deliberately not in that list: `victim_test.go` is an internal test, and `ManualClock`, `NewManualClock` and `CompileIgnoredSessions` are already exported, so nothing needs re-exporting for tests.

---

### Task 6: Sampler suppression

**Files:**
- Modify: `internal/run/executor.go:274-318` (the `ServerSampler` struct and its `Blocking` method)
- Test: `internal/run/executor_test.go`

**Interfaces:**
- Consumes: `VictimKiller.consider`, `VictimKiller.Suppressed` (Task 5).
- Produces: `func (s *ServerSampler) SetVictimKiller(k *VictimKiller)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/run/executor_test.go`. Read the file first — it already has a fake `sampleProbe`; reuse it rather than writing a second one.

```go
func TestServerSamplerSuppressesPendingVictim(t *testing.T) {
	snap := amplifierSnapshot(16)
	probe := fakeProbe{sessions: snap} // the existing fake in this file; a value type, not a pointer
	sampler := NewServerSampler(probe, 67, 1<<40, 100)

	clk := NewManualClock(time.Unix(1_800_000_000, 0))
	killer := NewVictimKiller(
		func(context.Context, int) error { return nil },
		nil, nil, clk, mssql.AppNamePrefix,
	)
	killer.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 60 * time.Second})
	sampler.SetVictimKiller(killer)

	st, err := sampler.Blocking(context.Background(), nil)
	if err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	if !st.Any {
		t.Error("BlockState.Any = false, want true — a pending victim still counts toward max_block")
	}
	if st.Unignored {
		t.Error("BlockState.Unignored = true for a pending kill, want false — the yield timer must not fire")
	}
}

func TestServerSamplerCountsVictimWhenKillFails(t *testing.T) {
	snap := amplifierSnapshot(16)
	probe := &fakeProbe{sessions: snap}
	sampler := NewServerSampler(probe, 67, 1<<40, 100)

	clk := NewManualClock(time.Unix(1_800_000_000, 0))
	killer := NewVictimKiller(
		func(context.Context, int) error { return errors.New("permission denied") },
		nil, nil, clk, mssql.AppNamePrefix,
	)
	killer.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 60 * time.Second})
	sampler.SetVictimKiller(killer)

	if _, err := sampler.Blocking(context.Background(), nil); err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	clk.Advance(61 * time.Second)
	st, err := sampler.Blocking(context.Background(), nil)
	if err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	if !st.Unignored {
		t.Error("BlockState.Unignored = false after a failed KILL, want true — we must fall back to yielding")
	}
}

func TestServerSamplerWithoutVictimKillerIsUnchanged(t *testing.T) {
	probe := fakeProbe{sessions: amplifierSnapshot(16)}
	sampler := NewServerSampler(probe, 67, 1<<40, 100)

	st, err := sampler.Blocking(context.Background(), nil)
	if err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	if !st.Any || !st.Unignored {
		t.Errorf("BlockState = %+v, want both true — without a killer the behavior is today's", st)
	}
}

// A suppressed victim must not mask an ordinary application session we are also
// blocking: the yield timer still has to fire for that one. This is what the dropped
// `break` in the rewritten loop buys, so it needs an explicit assertion.
func TestServerSamplerSuppressionDoesNotMaskAnotherBlockedSession(t *testing.T) {
	snap := append(amplifierSnapshot(16),
		mssql.Session{SPID: 500, Command: "SELECT", BlockingSPID: 67, Login: "app", Program: "PayrollApp"})
	probe := fakeProbe{sessions: snap}
	sampler := NewServerSampler(probe, 67, 1<<40, 100)

	clk := NewManualClock(time.Unix(1_800_000_000, 0))
	killer := NewVictimKiller(
		func(context.Context, int) error { return nil },
		nil, nil, clk, mssql.AppNamePrefix,
	)
	killer.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 60 * time.Second})
	sampler.SetVictimKiller(killer)

	st, err := sampler.Blocking(context.Background(), nil)
	if err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	if !st.Unignored {
		t.Error("BlockState.Unignored = false, want true — SPID 500 is an ordinary blocked " +
			"session and must still drive the yield even while the amplifier kill is pending")
	}
}

// Spec §1.6: with both killers armed on a snapshot showing a mutual block, exactly one
// KILL must be issued, whichever order they are consulted in. VictimKiller declines its
// own direct blocker, so BlockerKiller owns it.
func TestSamplerMutualBlockIssuesExactlyOneKill(t *testing.T) {
	snap := amplifierSnapshot(16)
	snap[0].BlockingSPID = 79 // 79 blocks us and we block 79
	probe := fakeProbe{sessions: snap}
	sampler := NewServerSampler(probe, 67, 1<<40, 100)

	var killed []int
	clk := NewManualClock(time.Unix(1_800_000_000, 0))

	victims := NewVictimKiller(
		func(_ context.Context, spid int) error { killed = append(killed, spid); return nil },
		nil, nil, clk, mssql.AppNamePrefix,
	)
	victims.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 0})
	sampler.SetVictimKiller(victims)

	blockers := NewBlockerKiller(
		func(_ context.Context, spid int) error { killed = append(killed, spid); return nil },
		nil, clk.Now)
	blockers.SetSource(staticKill{rules: []killRule{{match: sessionRule{sessionID: 79}, after: 0}}})
	sampler.SetKiller(blockers)

	if _, err := sampler.Blocking(context.Background(), nil); err != nil {
		t.Fatalf("Blocking() error = %v", err)
	}
	if len(killed) != 1 || killed[0] != 79 {
		t.Errorf("killed = %v, want exactly one KILL of SPID 79 — the two killers must be disjoint", killed)
	}
}
```

Add `"time"` and the `mssql` import to `executor_test.go` if they are not already there. `staticKill`, `killRule` and `sessionRule` are unexported types in this package, reachable because the test is internal.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/run -run TestServerSampler -v`
Expected: FAIL — `sampler.SetVictimKiller undefined`

- [ ] **Step 3: Write minimal implementation**

In `internal/run/executor.go`, add a field to `ServerSampler`:

```go
	victims       *VictimKiller  // optional: kills amplifying maintenance victims we block
```

Add the setter next to `SetKiller`:

```go
// SetVictimKiller attaches the amplifying-victim killer, consulted on every Blocking
// poll using the same session snapshot. A nil killer (the default) leaves the feature
// off and Blocking behaves exactly as it did before.
func (s *ServerSampler) SetVictimKiller(k *VictimKiller) { s.victims = k }
```

Replace the body of `Blocking` with:

```go
func (s *ServerSampler) Blocking(ctx context.Context, ignore IgnoredSessions) (BlockState, error) {
	sessions, err := s.probe.ActiveSessions(ctx)
	if err != nil {
		return BlockState{}, err
	}
	// Update victim episodes and kill anything eligible before reading suppression, so
	// a victim that becomes eligible on this very poll is suppressed on this poll too.
	s.victims.consider(ctx, sessions, s.spid, ignore)

	var st BlockState
	for _, sess := range sessions {
		if !sess.BlockedBy(s.spid) {
			continue
		}
		st.Any = true
		if ignore.ignores(sess) || s.victims.Suppressed(sess.SPID) {
			continue
		}
		st.Unignored = true
	}
	// Reuse the same snapshot to kill any session blocking our DDL that matches a kill
	// rule (the inverse direction: here we are the victim). No-op when no killer is set.
	s.killer.consider(ctx, sessions, s.spid)
	return st, nil
}
```

Note the loop no longer `break`s on the first unignored session: it must visit every victim so `Any` is correct even when an early session is suppressed. That is a behavior-preserving change for `Unignored` and a fix for nothing — `Any` was already set before the break — but the shape is clearer and the cost is one pass over a small slice.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/run -run 'TestServerSampler|TestSamplerMutualBlock' -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Run the whole package**

Run: `go test -race ./internal/run`
Expected: PASS — in particular the existing `max_block` and ignore-rule tests must be untouched.

- [ ] **Step 6: Commit**

```bash
git add internal/run/executor.go internal/run/executor_test.go
git commit -m "feat(run): suppress the yield timer while an amplifier kill is pending"
```

---

### Task 7: `.amplifiers.yaml` sidecar

**Files:**
- Create: `internal/run/amplifier_capture.go`
- Test: `internal/run/amplifier_capture_test.go`
- Modify: `internal/run/capture.go:157-160` (`relocateCaptures`)

**Interfaces:**
- Consumes: `AmplifierKillEvent` (Task 5); `mssql.AgentJob.DisableStatement` (Task 3).
- Produces:
  - `const amplifierCaptureSuffix = ".amplifiers.yaml"`
  - `type amplifierCapture struct { ... }` with `add(ev AmplifierKillEvent, now string)`, `len() int`, `jobs() []string` (distinct job labels for the TUI, in first-seen order).
  - `func renderAmplifiers(name string, acc *amplifierCapture) []byte`

Read `internal/run/capture.go` first: `renderCapture` and `writeYAMLString` are the model for this file's rendering, and `flushCapture` is the model for the engine-side write.

- [ ] **Step 1: Write the failing test**

Create `internal/run/amplifier_capture_test.go` (package `run`, internal — it tests unexported rendering):

```go
package run

import (
	"strings"
	"testing"

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/run -run 'TestRenderAmplifiers|TestAmplifierCapture' -v`
Expected: FAIL — `undefined: amplifierCapture`

- [ ] **Step 3: Write minimal implementation**

Create `internal/run/amplifier_capture.go`:

```go
package run

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// amplifierCaptureSuffix names the advisory capture file written next to a manifest.
const amplifierCaptureSuffix = ".amplifiers.yaml"

// capturedAmplifier is one maintenance victim this run killed.
type capturedAmplifier struct {
	event    AmplifierKillEvent
	killedAt string
}

// amplifierCapture accumulates the amplifying victims killed during one manifest, in
// kill order, and the distinct jobs they came from.
//
// Unlike blockerCapture, which is touched only from the engine goroutine, a kill
// happens on the pump goroutine inside Sampler.Blocking — so this accumulator carries
// its own mutex. It is small and append-mostly, which is exactly the case a mutex
// suits; a channel funnel would be more machinery for no gain.
type amplifierCapture struct {
	mu       sync.Mutex
	killed   []capturedAmplifier
	jobOrder []string
	jobSeen  map[string]bool
}

// jobKey identifies a distinct source: (job id, step) when attribution resolved,
// otherwise the raw program name — so an unattributed CmdExec step still collapses to
// one entry rather than one per kill.
func jobKey(ev AmplifierKillEvent) string {
	if ev.Job.Resolved {
		return fmt.Sprintf("%s:%d", ev.Job.JobID, ev.Job.StepID)
	}
	return "program:" + ev.Program
}

// jobLabel is the human-readable form of a distinct source, for the TUI line.
func jobLabel(ev AmplifierKillEvent) string {
	if ev.Job.Resolved {
		return fmt.Sprintf("%s (step %d)", ev.Job.JobName, ev.Job.StepID)
	}
	return ev.Program
}

func (c *amplifierCapture) add(ev AmplifierKillEvent, now string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jobSeen == nil {
		c.jobSeen = make(map[string]bool)
	}
	c.killed = append(c.killed, capturedAmplifier{event: ev, killedAt: now})
	if key := jobKey(ev); !c.jobSeen[key] {
		c.jobSeen[key] = true
		c.jobOrder = append(c.jobOrder, jobLabel(ev))
	}
}

func (c *amplifierCapture) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.killed)
}

// jobs returns the distinct sources seen, in first-seen order, for the TUI's sticky
// conflicting-jobs line.
func (c *amplifierCapture) jobs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.jobOrder))
	copy(out, c.jobOrder)
	return out
}

// renderAmplifiers builds the advisory capture file. SqlGoPace never reads it back —
// disabling a job is a deliberate operator step.
func renderAmplifiers(name string, acc *amplifierCapture) []byte {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "# Amplifying maintenance victims killed during %s\n", name)
	b.WriteString("# Advisory only — SqlGoPace never reads this file back.\n")
	b.WriteString("# Each entry is a session this run terminated because it requested a Sch-M lock\n")
	b.WriteString("# behind our operation while other sessions queued behind it.\n\n")

	b.WriteString("killed:\n")
	for _, ca := range acc.killed {
		ev := ca.event
		fmt.Fprintf(&b, "  - session_id: %d\n", ev.SPID)
		writeYAMLString(&b, "    command", ev.Command)
		writeYAMLString(&b, "    statement", ev.Statement)
		writeYAMLString(&b, "    database", ev.Database)
		writeYAMLString(&b, "    login_name", ev.Login)
		writeYAMLString(&b, "    host_name", ev.Host)
		writeYAMLString(&b, "    app_name", ev.Program)
		fmt.Fprintf(&b, "    blocked_behind: %d\n", ev.BlockedBehind)
		fmt.Fprintf(&b, "    waited_ms: %d\n", ev.WaitedMS)
		if !ev.FirstEligible.IsZero() {
			writeYAMLString(&b, "    first_eligible", ev.FirstEligible.UTC().Format(time.RFC3339))
		}
		writeYAMLString(&b, "    killed_at", ca.killedAt)
		b.WriteString("    agent_job:\n")
		fmt.Fprintf(&b, "      resolved: %t\n", ev.Job.Resolved)
		if !ev.Job.Resolved {
			continue
		}
		writeYAMLString(&b, "      job_id", ev.Job.JobID)
		writeYAMLString(&b, "      job_name", ev.Job.JobName)
		fmt.Fprintf(&b, "      step_id: %d\n", ev.Job.StepID)
		writeYAMLString(&b, "      step_name", ev.Job.StepName)
	}

	var stmts []string
	seen := make(map[string]bool)
	for _, ca := range acc.killed {
		s := ca.event.Job.DisableStatement()
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		stmts = append(stmts, s)
	}
	if len(stmts) > 0 {
		b.WriteString("\n# Distinct SQL Agent jobs terminated by this run. Review before disabling:\n")
		for _, s := range stmts {
			fmt.Fprintf(&b, "#   %s\n", s)
		}
	}
	return []byte(b.String())
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/run -run 'TestRenderAmplifiers|TestAmplifierCapture' -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Relocate the sidecar on finalize**

In `internal/run/capture.go`, add one line to `relocateCaptures`:

```go
func (e *Engine) relocateCaptures(name, dir string) {
	e.relocateSidecar(name, dir, blockedCaptureSuffix, "blocked-session capture")
	e.relocateSidecar(name, dir, contendedCaptureSuffix, "contended capture")
	e.relocateSidecar(name, dir, amplifierCaptureSuffix, "amplifier capture")
}
```

- [ ] **Step 6: Run the whole package**

Run: `go test -race ./internal/run`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/run/amplifier_capture.go internal/run/amplifier_capture_test.go internal/run/capture.go
git commit -m "feat(run): write an .amplifiers.yaml advisory sidecar"
```

---

### Task 8: Config, engine wiring, TUI, and CLI

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`
- Modify: `internal/run/engine.go`
- Modify: `internal/tui/model.go`, `internal/tui/view.go`
- Test: `internal/tui/model_test.go`
- Modify: `cmd/sqlgopace/main.go`
- Modify: `config.yaml`

**Interfaces:**
- Consumes: everything from Tasks 1–7.
- Produces:
  - `type config.KillAmplifyingMaintenanceConfig struct { Enabled bool; MinBlockedBehind int; AfterSeconds int; Commands []string }` with `func (k KillAmplifyingMaintenanceConfig) After() time.Duration` and `func (k KillAmplifyingMaintenanceConfig) MinBehind() int`.
  - `func run.WithVictimKiller(k *VictimKiller, p AmplifierPolicy) EngineOption`
  - `type tui.ConflictingJobsMsg struct { Jobs []string }`

- [ ] **Step 1: Write the failing config test**

Append to `internal/config/config_test.go`, matching the existing table style in that file:

```go
func TestParseKillAmplifyingMaintenance(t *testing.T) {
	const yaml = `
database:
  connection_string: "server=h;database=d"
kill_amplifying_maintenance:
  enabled: true
  min_blocked_behind: 3
  after_seconds: 90
  commands: ["UPDATE STATISTIC"]
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	k := cfg.KillAmplifyingMaintenance
	if !k.Enabled {
		t.Error("Enabled = false, want true")
	}
	if got, want := k.MinBehind(), 3; got != want {
		t.Errorf("MinBehind() = %d, want %d", got, want)
	}
	if got, want := k.After(), 90*time.Second; got != want {
		t.Errorf("After() = %v, want %v", got, want)
	}
	if len(k.Commands) != 1 || k.Commands[0] != "UPDATE STATISTIC" {
		t.Errorf("Commands = %v, want [UPDATE STATISTIC]", k.Commands)
	}
}

func TestKillAmplifyingMaintenanceDefaults(t *testing.T) {
	const yaml = `
database:
  connection_string: "server=h;database=d"
kill_amplifying_maintenance:
  enabled: true
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	k := cfg.KillAmplifyingMaintenance
	if got, want := k.MinBehind(), 1; got != want {
		t.Errorf("MinBehind() = %d, want %d — one queued reader means amplification has begun", got, want)
	}
	if got, want := k.After(), 60*time.Second; got != want {
		t.Errorf("After() = %v, want %v", got, want)
	}
	if len(k.Commands) != 0 {
		t.Errorf("Commands = %v, want empty — an empty list means the built-in allow-list", k.Commands)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/config -run TestKillAmplifying -v`
Expected: FAIL — `cfg.KillAmplifyingMaintenance undefined`

- [ ] **Step 3: Implement the config**

In `internal/config/config.go`, add to `Config` (as a sibling of `KillBlockers`, which is also top-level):

```go
	KillAmplifyingMaintenance KillAmplifyingMaintenanceConfig `yaml:"kill_amplifying_maintenance"`
```

And the type, next to `KillBlockersConfig`:

```go
// KillAmplifyingMaintenanceConfig arms the kill of maintenance statements our
// operation blocks that have other sessions queued behind them. Top-level, as a
// sibling of kill_blockers: that is the mirror-direction feature, and monitoring:
// holds cadences and thresholds rather than policy arming.
type KillAmplifyingMaintenanceConfig struct {
	Enabled          bool     `yaml:"enabled"`
	MinBlockedBehind int      `yaml:"min_blocked_behind"`
	AfterSeconds     int      `yaml:"after_seconds"`
	Commands         []string `yaml:"commands"`
}

// MinBehind is how many sessions must be queued behind a victim before it counts as an
// amplifier; defaults to 1, because one queued reader means the amplification has
// already begun and the fan only grows from there.
func (k KillAmplifyingMaintenanceConfig) MinBehind() int {
	if k.MinBlockedBehind <= 0 {
		return 1
	}
	return k.MinBlockedBehind
}

// After is how long a victim must stay eligible before it is killed; defaults to 60s.
func (k KillAmplifyingMaintenanceConfig) After() time.Duration {
	if k.AfterSeconds <= 0 {
		return 60 * time.Second
	}
	return time.Duration(k.AfterSeconds) * time.Second
}
```

- [ ] **Step 4: Run the config test**

Run: `go test -race ./internal/config -v`
Expected: PASS, including the existing tests.

- [ ] **Step 5: Write the failing TUI test**

Append to `internal/tui/model_test.go`, matching the style of `TestModelShowsAlertAndKeepsItSticky`:

```go
func TestModelShowsConflictingJobsAndReplacesThem(t *testing.T) {
	m := tui.New("reorganize_index dbo.MEASUREMENT", nil)
	m, _ = send(m, tui.ConflictingJobsMsg{Jobs: []string{"IndexOptimize - USER_DATABASES (step 1)"}})
	if got := m.View(); !strings.Contains(got, "IndexOptimize - USER_DATABASES (step 1)") {
		t.Errorf("view does not show the conflicting job\n---\n%s", got)
	}

	// Replace semantics, not append: a second message supersedes the first.
	m, _ = send(m, tui.ConflictingJobsMsg{Jobs: []string{"Nightly stats (step 2)"}})
	got := m.View()
	if strings.Contains(got, "IndexOptimize") {
		t.Errorf("view still shows the superseded job\n---\n%s", got)
	}
	if !strings.Contains(got, "Nightly stats (step 2)") {
		t.Errorf("view does not show the replacement job\n---\n%s", got)
	}

	// An empty set clears the line at the end of a manifest.
	m, _ = send(m, tui.ConflictingJobsMsg{Jobs: nil})
	if got := m.View(); strings.Contains(got, "Nightly stats") {
		t.Errorf("view still shows a job after the set was cleared\n---\n%s", got)
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test -race ./internal/tui -run TestModelShowsConflictingJobs -v`
Expected: FAIL — `undefined: tui.ConflictingJobsMsg`

- [ ] **Step 7: Implement the TUI message**

In `internal/tui/model.go`, add to the message type block next to `AlertMsg`:

```go
	// ConflictingJobsMsg carries the SQL Agent jobs whose maintenance statements this
	// run has terminated, for a sticky line above the dashboard. Unlike AlertMsg it
	// REPLACES the current set rather than appending: the jobs are manifest-scoped, and
	// the engine sends an empty set when it finishes a manifest. Reusing AlertMsg would
	// mean teaching a never-cleared slice to clear, changing every existing alert.
	ConflictingJobsMsg struct{ Jobs []string }
```

Add the field to `Model`:

```go
	conflictJobs    []string
```

Add the case to `Update`:

```go
	case ConflictingJobsMsg:
		m.conflictJobs = msg.Jobs
```

In `internal/tui/view.go`, render it inside `alertsBlock` so it shares the sticky area:

```go
// alertsBlock renders the sticky failure alerts and conflicting-job notices shown
// above the dashboard, or "" when there are none.
func (m Model) alertsBlock() string {
	if len(m.alerts) == 0 && len(m.conflictJobs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, a := range m.alerts {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(alertStyle.Render("⚠ " + a.Title))
		for _, line := range a.Lines {
			b.WriteString("\n" + alertStyle.Render("    "+line))
		}
	}
	if len(m.conflictJobs) > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(alertStyle.Render("⚠ conflicting SQL Agent jobs terminated this run"))
		for _, j := range m.conflictJobs {
			b.WriteString("\n" + alertStyle.Render("    " + j + " — consider disabling it during maintenance"))
		}
	}
	return b.String()
}
```

- [ ] **Step 8: Run the TUI tests**

Run: `go test -race ./internal/tui`
Expected: PASS, including `TestModelShowsAlertAndKeepsItSticky`.

- [ ] **Step 9: Wire the engine**

In `internal/run/engine.go`. The line numbers below are from the pre-change file and will shift once earlier tasks land — locate each site by the quoted surrounding code, not by line number.

Add fields to `Engine` next to `killer`:

```go
	victims       *VictimKiller     // when set, kills amplifying maintenance victims we block
	victimPolicy  AmplifierPolicy   // the armed policy for that killer
	asyncStats    AsyncStatsSetting // ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY on the target
	amplifierSink func([]string)    // notified with the distinct conflicting jobs (TUI)
```

Add the options next to `WithBlockerKiller`:

```go
// WithVictimKiller arms the amplifying-maintenance-victim kill: the killer terminates
// maintenance statements this run's operation blocks once other sessions have queued
// behind them for the policy's dwell. The same killer must be attached to the sampler
// (ServerSampler.SetVictimKiller) so it is consulted on each blocking poll.
func WithVictimKiller(k *VictimKiller, p AmplifierPolicy) EngineOption {
	return func(e *Engine) { e.victims = k; e.victimPolicy = p }
}

// WithAsyncStatsSetting supplies the target's ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY
// state, used for the pre-reorganize advisory.
func WithAsyncStatsSetting(s AsyncStatsSetting) EngineOption {
	return func(e *Engine) { e.asyncStats = s }
}

// WithAmplifierSink is notified with the distinct SQL Agent jobs whose statements this
// run has killed, whenever that set changes, and with nil at the end of each manifest.
func WithAmplifierSink(f func([]string)) EngineOption {
	return func(e *Engine) { e.amplifierSink = f }
}
```

Arm the killer next to the existing `e.killer.SetSource` block (around line 546), and create the capture accumulator beside `captured` and `contended` (around line 557):

```go
	// Arm the amplifying-victim killer for this manifest, if wired. Disarm when the
	// manifest is done so a later manifest does not inherit its episode state, and
	// clear the TUI's conflicting-jobs line at the same moment.
	amplifiers := &amplifierCapture{}
	if e.victims != nil {
		e.victims.Arm(e.victimPolicy)
		defer func() {
			e.victims.Disarm()
			if e.amplifierSink != nil {
				e.amplifierSink(nil)
			}
		}()
	}
```

Inside the per-operation loop, give the killer the operation's sink, right after `sink` is defined (`engine.go:638`):

```go
		// The victim killer emits kills on this operation's sink, from the pump
		// goroutine. Each iteration overwrites it before any kill can occur, and
		// Disarm clears it at the end of the manifest.
		e.victims.SetSink(sink)
```

`SetSink` is nil-receiver safe, so no `if e.victims != nil` guard is needed. Do **not** add a `defer e.victims.SetSink(nil)` here: a `defer` inside the loop would not run until `processOne` returns, so it would neither scope the sink per operation nor clear it earlier than `Disarm` already does. The manifest-level `Disarm` above is the single clearing point.

Record the kill in the sink itself, beside the existing `ev.Tail` branch at `engine.go:606`:

```go
		sink := func(ev ReactionEvent) {
			if ev.Tail != nil {
				e.captureTail(contended, name, manifest.Database, *ev.Tail)
			}
			if ev.Amplifier != nil {
				amplifiers.add(*ev.Amplifier, e.now())
				e.flushAmplifiers(name, amplifiers)
				if e.amplifierSink != nil {
					e.amplifierSink(amplifiers.jobs())
				}
			}
			// ... the rest of the existing sink body is unchanged ...
		}
```

The existing body already appends a `report.ReactionLine` and prints `-- kill <target>: <detail>` to `e.out`, so the run report comes for free. `Kind` is `"kill"`, which is not in the `capture` set (`pause`/`cancel`/`abort`) — correct, because we did not yield and there is nothing to snapshot.

Note what that also means: `e.notify` is only called for the `capture` kinds, so an amplifier kill does **not** reach the webhook or email notifier. That is pre-existing behavior, not something this task changes; Task 9 documents it so the run report is not mistaken for a notification.

Add the flush helper to `internal/run/amplifier_capture.go`, modelled on `flushCapture`:

```go
// flushAmplifiers writes the accumulated amplifier capture next to the manifest in
// processing, so it is available during the run; relocateCaptures moves it to the
// manifest's final directory on finalize.
func (e *Engine) flushAmplifiers(name string, acc *amplifierCapture) {
	if acc.len() == 0 {
		return
	}
	path := filepath.Join(e.dirs.Processing, name+amplifierCaptureSuffix)
	if err := os.WriteFile(path, renderAmplifiers(name, acc), 0o644); err != nil {
		fmt.Fprintf(e.out, "write amplifier capture %s: %v\n", name, err)
	}
}
```

Add `"os"` and `"path/filepath"` to that file's imports.

Emit the advisory next to the RCSI warning (around line 690):

```go
			if msg, ok := reorgRCSIWarning(step.Operation, db, e.rcsi); ok {
				sink(ReactionEvent{Kind: "warn", Detail: msg})
			}
			if msg, ok := asyncStatsAdvisory(step.Operation, db, e.asyncStats); ok {
				sink(ReactionEvent{Kind: "warn", Detail: msg})
			}
```

- [ ] **Step 10: Wire the CLI**

All of this goes in **`buildEngine`**, not `runEngine`. `buildEngine` is called once per database with a connection already scoped to that database (`main.go:383`); `runEngine` holds only the startup connection. Putting the reads there is what makes multi-database runs correct.

First, extend the preflight permission predicate at `main.go:388`. This feature issues `KILL` and so needs `ALTER ANY CONNECTION` exactly as `kill_blockers` does; without this an operator enables the feature and only learns about the missing grant when the first victim appears:

```go
	killArmed := cfg.KillBlockers.Enabled ||
		cfg.KillAmplifyingMaintenance.Enabled ||
		cfg.OptionsOverride.AllowAbortBlockers
```

Then, next to the `cfg.KillBlockers.Enabled` block (around line 395):

```go
	var victimOpt run.EngineOption = func(*run.Engine) {}
	if cfg.KillAmplifyingMaintenance.Enabled {
		policy := run.AmplifierPolicy{
			MinBlockedBehind: cfg.KillAmplifyingMaintenance.MinBehind(),
			After:            cfg.KillAmplifyingMaintenance.After(),
			Commands:         cfg.KillAmplifyingMaintenance.Commands,
		}
		// Presentation only. The engine's reaction sink records the same line in the
		// run report, but in TUI mode engineOut is io.Discard (main.go:206), so without
		// this forward the operator would see no per-kill narration at all.
		amplifierKilled := func(ev run.AmplifierKillEvent) {
			detail := run.AmplifierDetail(ev)
			fmt.Fprintf(engineOut, "-- %s\n", detail)
			if fwd != nil {
				fwd.send(tui.LogMsg{Line: detail})
			}
		}
		victims := run.NewVictimKiller(conn.Kill, conn.LookupAgentJob, amplifierKilled, run.System, mssql.AppNamePrefix)
		sampler.SetVictimKiller(victims)
		victimOpt = run.WithVictimKiller(victims, policy)
	}
```

`mssql.AppNamePrefix` is the constant added in Task 5 Step 1. `conn.Kill` and `conn.LookupAgentJob` both run on the monitoring pool, so a blocked execution session does not stop either.

Read the `ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY` state from the **same per-database `conn`**, in `buildEngine`. The setting is database-scoped, so reading it from the startup connection in `runEngine` would report the wrong database in a multi-database run:

```go
	asyncStats := run.AsyncStatsAbsent
	if on, present, err := conn.AsyncStatsWaitAtLowPriority(ctx); err == nil && present {
		asyncStats = run.AsyncStatsOff
		if on {
			asyncStats = run.AsyncStatsOn
		}
	}
```

Add to the `opts := []run.EngineOption{...}` slice (around line 476):

```go
		victimOpt,
		run.WithAsyncStatsSetting(asyncStats),
		run.WithAmplifierSink(func(jobs []string) {
			if fwd != nil {
				fwd.send(tui.ConflictingJobsMsg{Jobs: jobs})
			}
		}),
```

- [ ] **Step 11: Add the config.yaml sample block**

In `config.yaml`, after the `kill_blockers:` block:

```yaml
# Terminate maintenance statements this run's operation blocks once other sessions
# have queued behind them. An online REORGANIZE stops being online the moment a
# Sch-M request (UPDATE STATISTICS, ALTER INDEX, TRUNCATE TABLE, ...) queues behind
# it: every reader arriving afterwards queues behind that request rather than
# barging past. Off by default. Requires ALTER ANY CONNECTION, and SELECT on
# msdb.dbo.sysjobs to name the SQL Agent job (optional; attribution degrades without it).
kill_amplifying_maintenance:
  enabled: false
  min_blocked_behind: 1   # sessions queued behind the victim before it counts as an amplifier
  after_seconds: 60       # how long it must stay eligible before the KILL
  commands: []            # empty = the built-in allow-list; a non-empty list REPLACES it
```

- [ ] **Step 12: Build and run the full test suite**

Run: `make build && make vet && go test -race ./...`
Expected: build succeeds, vet clean, all tests PASS.

Do not run `make lint` — see Global Constraints.

- [ ] **Step 13: Commit**

```bash
git add internal/config/ internal/run/ internal/tui/ cmd/sqlgopace/main.go config.yaml
git commit -m "feat: wire the amplifying-maintenance-victim kill end to end"
```

---

### Task 9: Documentation and version bump

**Files:**
- Modify: `README.md`
- Modify: `specs/SPECS.md`
- Modify: `internal/version/VERSION`

- [ ] **Step 1: Document the config block in README.md**

Find the section documenting `kill_blockers` and add a sibling subsection for `kill_amplifying_maintenance`. It must state, in prose:

- what the feature does and the lock mechanic that makes it necessary (a queued Sch-M request stops readers barging past, so one waiter turns an online operation into a table outage);
- the six eligibility conditions from §1.3 of the spec;
- that `ignore_blocked_sessions` beats the kill, and that **`ignore_blocking` does not** — an operator running with `ignore_blocking: true` will still see maintenance victims killed, because that option only suppresses the yield reaction;
- that a failed `KILL` falls back to the normal yield;
- that `max_block_minutes` still backstops the whole thing;
- the required permissions (`ALTER ANY CONNECTION`; `SELECT` on `msdb.dbo.sysjobs` optional), and that enabling the feature makes preflight warn when the login lacks the former;
- that SqlGoPace never writes to msdb and never disables a job;
- the `.amplifiers.yaml` sidecar, and that it is advisory only and never read back;
- **where kills are and are not reported**: the run `.log`, the `.amplifiers.yaml` sidecar, stdout, and the TUI (feed line plus sticky job alert) — but **not** webhook or email, because the notifier only fires for `pause`/`cancel`/`abort`. Reaching those would require extending the `on_events` list and the `notify` branch in `engine.go`, which this feature deliberately does not do.

- [ ] **Step 2: Document the two advisories in README.md**

In the same area as the existing RCSI warning documentation, add the `ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY` advisory, stating that the setting covers only asynchronous automatic statistics updates and not an explicit `UPDATE STATISTICS` from a job.

- [ ] **Step 3: Record the engine semantics in specs/SPECS.md**

Add to the reaction-hierarchy section: a kill-eligible victim contributes to `BlockState.Any` but not `BlockState.Unignored`, so the yield timer does not fire while a kill is pending; `max_block_minutes` is unchanged and still backstops; `DecideReaction` gains no new `Action`.

- [ ] **Step 4: Bump the version**

Set `internal/version/VERSION` to `0.13.0`.

- [ ] **Step 5: Verify**

Run: `make build && go test -race ./...`
Expected: build succeeds, all tests PASS. Confirm `bin/sqlgopace --version` prints `0.13.0`.

- [ ] **Step 6: Commit**

```bash
git add README.md specs/SPECS.md internal/version/VERSION
git commit -m "docs: document the amplifying-victim kill and stats advisory; bump to 0.13.0"
```

---

## Post-implementation

Run a `/simplify` pass over the full diff before merging, per the repo convention in `CLAUDE.md`: collapse the duplication that accretes between `kill.go` and `victim.go`, and between `capture.go` and `amplifier_capture.go`. Do **not** pre-emptively factor those into a shared abstraction during implementation — the two killers answer different questions and the shape of any genuinely shared part is only visible once both exist.
