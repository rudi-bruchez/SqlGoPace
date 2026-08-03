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
		// Without the trailing anchor this parsed, and the kill would be attributed to
		// a real but wrong job — sp_update_job line included.
		{name: "trailing content fails closed", program: "SQLAgent - TSQL JobStep (Job 0x9B3C : Step 1) and more", wantOK: false},
		{name: "trailing parenthesis fails closed", program: "SQLAgent - TSQL JobStep (Job 0x9B3C : Step 1))", wantOK: false},
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
