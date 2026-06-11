// Command sqlexec is a throwaway helper: it connects with the project's config
// and runs the SQL statement given as its single argument. Usage:
//
//	go run ./tmp/sqlexec "ALTER INDEX [IX] ON [dbo].[T] ABORT;"
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sqlexec <sql>")
		os.Exit(2)
	}
	stmt := os.Args[1]

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

	if err := conn.ExecDDL(ctx, stmt); err != nil {
		fmt.Fprintln(os.Stderr, "exec:", err)
		os.Exit(1)
	}
	fmt.Println("OK:", stmt)
}
