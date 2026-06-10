// Command sqlgopace runs demanding DDL operations against Microsoft SQL Server
// while monitoring their impact on locking and the transaction log.
//
// See specs/SPECS.md for the full behaviour and specs/IMPLEMENTATION.md for the
// implementation plan.
package main

import (
	"fmt"
	"os"
)

// version is the build version, overridden at release time via -ldflags.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sqlgopace:", err)
		os.Exit(1)
	}
}

// run is the testable entry point; main only wires it to the process.
func run(args []string) error {
	// Wiring is added phase by phase (see specs/IMPLEMENTATION.md).
	fmt.Printf("sqlgopace %s\n", version)
	return nil
}
