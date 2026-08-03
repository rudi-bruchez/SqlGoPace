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
// match, and attribution degrades rather than guessing. Anchored at both ends: without
// the trailing $, a crafted program_name carrying extra content after the closing
// parenthesis would still parse, and the kill would be attributed to a real but wrong
// job — down to an sp_update_job line naming it in the advisory sidecar.
var jobStepProgram = regexp.MustCompile(
	`^SQLAgent\s*-\s*TSQL\s+JobStep\s*\(\s*Job\s+(0[xX][0-9A-Fa-f]+)\s*:\s*Step\s+(\d+)\s*\)$`)

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
