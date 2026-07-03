//go:build integration

package mssql_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestServerNowIntegration(t *testing.T) {
	dsn := os.Getenv("SQLGOPACE_TEST_DSN")
	if dsn == "" {
		t.Skip("SQLGOPACE_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := mssql.Open(ctx, dsn, "test")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	got, err := conn.ServerNow(ctx)
	if err != nil {
		t.Fatalf("ServerNow: %v", err)
	}
	if got.IsZero() || got.Year() < 2020 {
		t.Fatalf("ServerNow returned implausible time %v", got)
	}
	_ = time.Now()
}
