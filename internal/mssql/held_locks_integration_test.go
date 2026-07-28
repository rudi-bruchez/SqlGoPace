//go:build integration

package mssql_test

import (
	"testing"
)

// TestHeldObjectLocksEmptyForIdleSession verifies the read runs and returns nothing for
// a session holding no Sch-M lock (the shape/permission smoke test; the populated case
// is exercised by the shrink e2e).
func TestHeldObjectLocksEmptyForIdleSession(t *testing.T) {
	conn, ctx := openTestConn(t)

	got, err := conn.HeldObjectLocks(ctx, conn.SPID())
	if err != nil {
		t.Fatalf("HeldObjectLocks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("idle session holds %d Sch-M object locks, want 0", len(got))
	}
}
