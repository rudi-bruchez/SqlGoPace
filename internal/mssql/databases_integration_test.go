//go:build integration

// Multi-database connection + enumeration integration tests (spec §17.2/§17.3).
// Run only with `-tags=integration` against a real SQL Server (see
// integration_test.go for the SQLGOPACE_TEST_DSN setup).
package mssql_test

import (
	"context"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// TestIntegrationOpenDatabase verifies the catalog override lands the connection in
// the requested database, reusing the base DSN's server and credentials.
func TestIntegrationOpenDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	for _, db := range []string{"master", "tempdb"} {
		conn, err := mssql.OpenDatabase(ctx, dsn(t), db, "test")
		if err != nil {
			t.Fatalf("OpenDatabase(%s) error = %v", db, err)
		}
		got, err := conn.CurrentDatabase(ctx)
		_ = conn.Close()
		if err != nil {
			t.Fatalf("CurrentDatabase() error = %v", err)
		}
		if got != db {
			t.Errorf("OpenDatabase(%s): CurrentDatabase() = %q, want %q", db, got, db)
		}
	}
}

// TestIntegrationUserDatabases checks the eligibility sweep: system databases are
// excluded entirely, and a fresh user database shows up as eligible.
func TestIntegrationUserDatabases(t *testing.T) {
	conn, ctx := openTestConn(t)
	const db = "sgp_m81_db"

	exec(t, conn, ctx, "IF DB_ID('"+db+"') IS NOT NULL DROP DATABASE ["+db+"]")
	exec(t, conn, ctx, "CREATE DATABASE ["+db+"]")
	t.Cleanup(func() {
		_ = conn.ExecDDL(context.Background(), "IF DB_ID('"+db+"') IS NOT NULL DROP DATABASE ["+db+"]")
	})

	dbs, err := conn.UserDatabases(ctx)
	if err != nil {
		t.Fatalf("UserDatabases() error = %v", err)
	}

	system := map[string]bool{"master": true, "tempdb": true, "model": true, "msdb": true}
	found := false
	for _, d := range dbs {
		if system[d.Name] {
			t.Errorf("system database %q must not appear in UserDatabases", d.Name)
		}
		if d.Name == db {
			found = true
			if !d.Eligible {
				t.Errorf("created database %q: Eligible = false, want true (reason %q)", db, d.IneligibleReason)
			}
		}
	}
	if !found {
		t.Errorf("created database %q not found in UserDatabases", db)
	}
}

// TestIntegrationUserDatabasesReadOnlyIneligible confirms a read-only database is
// reported ineligible with a reason rather than silently dropped.
func TestIntegrationUserDatabasesReadOnlyIneligible(t *testing.T) {
	conn, ctx := openTestConn(t)
	const db = "sgp_m81_ro"

	exec(t, conn, ctx, "IF DB_ID('"+db+"') IS NOT NULL DROP DATABASE ["+db+"]")
	exec(t, conn, ctx, "CREATE DATABASE ["+db+"]")
	t.Cleanup(func() {
		// Take it out of read-only before dropping, to be safe.
		_ = conn.ExecDDL(context.Background(), "IF DB_ID('"+db+"') IS NOT NULL ALTER DATABASE ["+db+"] SET READ_WRITE WITH NO_WAIT")
		_ = conn.ExecDDL(context.Background(), "IF DB_ID('"+db+"') IS NOT NULL DROP DATABASE ["+db+"]")
	})
	exec(t, conn, ctx, "ALTER DATABASE ["+db+"] SET READ_ONLY WITH NO_WAIT")

	dbs, err := conn.UserDatabases(ctx)
	if err != nil {
		t.Fatalf("UserDatabases() error = %v", err)
	}
	for _, d := range dbs {
		if d.Name == db {
			if d.Eligible || d.IneligibleReason == "" {
				t.Errorf("read-only %q = %+v, want ineligible with a reason", db, d)
			}
			return
		}
	}
	t.Errorf("read-only database %q not found in UserDatabases", db)
}
