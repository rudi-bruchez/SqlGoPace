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
