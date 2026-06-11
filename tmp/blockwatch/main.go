// Command blockwatch is a throwaway observer: it connects with the project's
// config and prints sessions blocked by our DDL's SPID, using the very same
// ActiveSessions DMV query the monitoring uses. Usage: go run ./tmp/blockwatch <ddlSPID>
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func main() {
	ddlSPID := 0
	if len(os.Args) > 1 {
		ddlSPID, _ = strconv.Atoi(os.Args[1])
	}

	cfg, err := config.Load("config-local.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
	ctx := context.Background()
	conn, err := mssql.Open(ctx, cfg.Database.ConnectionString, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	sessions, err := conn.ActiveSessions(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "active sessions:", err)
		os.Exit(1)
	}

	fmt.Printf("watching for sessions blocked by DDL SPID %d (%d active user sessions)\n", ddlSPID, len(sessions))
	found := 0
	for _, s := range sessions {
		if s.BlockingSPID == 0 {
			continue
		}
		mark := ""
		if ddlSPID != 0 && s.BlockingSPID == ddlSPID {
			mark = "  <== BLOCKED BY OUR DDL"
			found++
		}
		fmt.Printf("  spid=%d blocked_by=%d status=%s wait=%s wait_ms=%d login=%s host=%s program=%s%s\n",
			s.SPID, s.BlockingSPID, s.Status, s.WaitType, s.WaitMS, s.Login, s.Host, s.Program, mark)
		if q := strings.TrimSpace(s.ActiveQuery); q != "" {
			fmt.Printf("      query: %s\n", truncate(q, 160))
		}
	}
	if ddlSPID != 0 {
		fmt.Printf("=> %d session(s) currently blocked by our DDL (SPID %d)\n", found, ddlSPID)
	}

	ops, err := conn.ResumableOps(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resumable ops:", err)
		return
	}
	fmt.Printf("resumable index operations: %d\n", len(ops))
	for _, op := range ops {
		fmt.Printf("  index=%s state=%s percent=%.1f%%\n", op.Name, op.StateDesc, op.PercentComplete)
	}
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
