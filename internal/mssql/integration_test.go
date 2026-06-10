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

	conn, err := mssql.Open(ctx, dsn(t))
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
