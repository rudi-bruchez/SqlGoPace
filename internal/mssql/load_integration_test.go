//go:build integration

// Server-load read integration test. It runs only with `-tags=integration` against a real
// SQL Server (see integration_test.go for the SQLGOPACE_TEST_DSN setup). It reads DMVs
// only, so it neither creates objects nor mutates the database.
package mssql_test

import (
	"testing"
)

func TestIntegrationServerLoad(t *testing.T) {
	conn, ctx := openTestConn(t)

	load, err := conn.ServerLoad(ctx, true)
	if err != nil {
		t.Fatalf("ServerLoad(withCPU) error = %v", err)
	}

	// The paced form runs on most polls, so its query shape has to be exercised too: it
	// must come back with the live scheduler figures and no record.
	paced, err := conn.ServerLoad(ctx, false)
	if err != nil {
		t.Fatalf("ServerLoad(without CPU) error = %v", err)
	}
	if paced.CPUKnown {
		t.Error("the paced read returned CPU figures; it must not ask for the record")
	}
	if paced.Schedulers != load.Schedulers {
		t.Errorf("paced Schedulers = %d, want %d: both forms read the same schedulers",
			paced.Schedulers, load.Schedulers)
	}

	// The scheduler read is the part that must always work: every supported target has
	// online schedulers, so a zero here means the query shape is wrong, not that the
	// server is quiet.
	if load.Schedulers <= 0 {
		t.Errorf("Schedulers = %d, want at least one online scheduler", load.Schedulers)
	}
	if load.RunnableTasks < 0 {
		t.Errorf("RunnableTasks = %d, want >= 0", load.RunnableTasks)
	}

	// The CPU half is allowed to be missing (the ring buffer is not readable everywhere),
	// but when it is present the percentages must be coherent.
	if !load.CPUKnown {
		t.Logf("scheduler-monitor ring buffer unreadable on this target: CPU percentages omitted")
		return
	}
	if load.BusyPercent < 0 || load.BusyPercent > 100 {
		t.Errorf("BusyPercent = %d, want 0..100", load.BusyPercent)
	}
	if load.SQLPercent > load.BusyPercent {
		t.Errorf("SQLPercent %d > BusyPercent %d: the SQL share cannot exceed the machine's",
			load.SQLPercent, load.BusyPercent)
	}
	t.Logf("server load: cpu %d%% (sql %d, other %d), runnable %d/%d",
		load.BusyPercent, load.SQLPercent, load.BusyPercent-load.SQLPercent,
		load.RunnableTasks, load.Schedulers)
}
