//go:build integration

// Integration tests run only with `-tags=integration` against a real SQL Server.
// Set SQLGOPACE_TEST_DSN to a connection string, e.g.
//
//	SQLGOPACE_TEST_DSN="sqlserver://sa:Pass@word@localhost?database=tempdb&encrypt=disable"
//
// A throwaway Docker server works:
//
//	docker run -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD=Pass@word1 -p 1433:1433 \
//	    -d mcr.microsoft.com/mssql/server:2022-latest
package mssql_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv("SQLGOPACE_TEST_DSN")
	if v == "" {
		t.Skip("SQLGOPACE_TEST_DSN not set; skipping integration test")
	}
	return v
}

func openTestConn(t *testing.T) (*mssql.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	conn, err := mssql.Open(ctx, dsn(t), "test")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, ctx
}

func TestIntegrationDetectServer(t *testing.T) {
	conn, ctx := openTestConn(t)

	info, err := conn.Detect(ctx)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if !info.Supported() {
		t.Errorf("Supported() = false for EngineEdition %d", info.EngineEdition)
	}
	t.Logf("detected: edition=%d major=%d recovery=%s adr=%t",
		info.EngineEdition, info.MajorVersion, info.RecoveryModel, info.ADREnabled)
}

func TestIntegrationLogSpaceAndSPID(t *testing.T) {
	conn, ctx := openTestConn(t)

	if conn.SPID() <= 0 {
		t.Errorf("SPID() = %d, want a positive session id", conn.SPID())
	}
	ls, err := conn.LogSpace(ctx)
	if err != nil {
		t.Fatalf("LogSpace() error = %v", err)
	}
	if ls.TotalBytes <= 0 {
		t.Errorf("LogSpace().TotalBytes = %d, want > 0", ls.TotalBytes)
	}
}

// The driver renders a server error as "mssql: <text>" with no number. Against a real
// server, a failed statement's message must carry it, so a report can tell 1105 from 1205.
func TestIntegrationExecDDLErrorCarriesTheNumber(t *testing.T) {
	conn, ctx := openTestConn(t)
	err := conn.ExecDDL(ctx, "SELECT 1/0;")
	if err == nil || !strings.Contains(err.Error(), "Msg 8134") {
		t.Fatalf("ExecDDL() error = %v, want it to name Msg 8134", err)
	}
}

// The reuse wait is read in the same statement as the log space now.
func TestIntegrationLogSpaceCarriesTheReuseWait(t *testing.T) {
	conn, ctx := openTestConn(t)
	ls, err := conn.LogSpace(ctx)
	if err != nil {
		t.Fatalf("LogSpace() error = %v", err)
	}
	if ls.ReuseWait == "" {
		t.Errorf("LogSpace().ReuseWait is empty, want log_reuse_wait_desc (NOTHING at least)")
	}
}

// An open transaction in the database shows up as the oldest one, and the read works on
// the supported versions (sys.dm_db_log_stats is 2016 SP2+).
func TestIntegrationLogHistorySeesAnOpenTransaction(t *testing.T) {
	conn, ctx := openTestConn(t)
	other, err := mssql.Open(ctx, dsn(t), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if err := other.ExecDDL(ctx, "IF OBJECT_ID('dbo.sqlgopace_lh') IS NULL CREATE TABLE dbo.sqlgopace_lh (i int);"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.ExecDDL(ctx, "DROP TABLE IF EXISTS dbo.sqlgopace_lh;") })
	if err := other.ExecDDL(ctx, "BEGIN TRAN; INSERT dbo.sqlgopace_lh VALUES (1); WAITFOR DELAY '00:00:02';"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.ExecDDL(ctx, "IF @@TRANCOUNT > 0 ROLLBACK;") })

	lh, err := conn.LogHistory(ctx)
	if err != nil {
		t.Fatalf("LogHistory() error = %v", err)
	}
	if !lh.HasOldestTxn || lh.OldestTxnSec < 1 {
		t.Errorf("LogHistory() = %+v, want an open transaction at least 1 s old", lh)
	}
}
