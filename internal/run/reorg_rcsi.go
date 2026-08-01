package run

import (
	"fmt"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// reorgRCSIWarning returns the warning to emit before running op, and whether to emit
// it: only for a REORGANIZE against a database with RCSI (READ_COMMITTED_SNAPSHOT) off,
// where readers take shared locks and block on the operation's short-term page X locks.
// The database name is passed in so the returned message is complete and the helper
// stays a pure, testable decision function. Returns ("", false) for any other operation
// or when RCSI is on.
func reorgRCSIWarning(op ddl.Operation, database string, rcsi bool) (string, bool) {
	if rcsi {
		return "", false
	}
	reorg, ok := op.(ddl.ReorganizeIndex)
	if !ok {
		return "", false
	}
	return fmt.Sprintf(
		"%s.%s: RCSI is OFF on %s — readers may block on this REORGANIZE's page locks; the pacing loop will still yield on blocking.",
		reorg.Schema, reorg.Table, database,
	), true
}
