package mssql

import "strings"

// IsMaintenanceCommand reports whether cmd (a sys.dm_exec_requests.command verb) is a
// known index-maintenance / file-compaction operation the shrink driver treats as a
// transient, self-clearing blocker rather than a structural tail blocker. Conservative
// allow-list; case-insensitive and space-trimmed. Every DBCC statement (INDEXDEFRAG,
// SHRINKFILE, SHRINKDATABASE) reports the verb "DBCC"; ALTER INDEX covers both REBUILD
// and REORGANIZE. Unknown verbs return false, preserving today's behavior for
// application locks, ETL, and reporting workloads.
func IsMaintenanceCommand(cmd string) bool {
	switch strings.ToUpper(strings.TrimSpace(cmd)) {
	case "ALTER INDEX", "DBCC":
		return true
	default:
		return false
	}
}

// amplifyingCommands are the dm_exec_requests.command verbs whose blocked Sch-M
// request converts an online operation into a full-table outage: every reader
// arriving afterwards queues behind the waiting Sch-M rather than barging past it.
// Prefix-matched, so "UPDATE STATISTIC" covers both spellings SQL Server reports.
// Verified 2026-08-04 against a live server: the verb is "UPDATE STATISTICS", with
// the trailing S. The entry keeps the shorter prefix deliberately, so a version that
// reports the truncated form still matches; do not "correct" it to the full verb.
var amplifyingCommands = []string{
	"ALTER INDEX",
	"ALTER TABLE",
	"CREATE INDEX",
	"CREATE STATISTICS",
	"UPDATE STATISTIC",
	"DROP INDEX",
	"DROP TABLE",
	"TRUNCATE TABLE",
	"DBCC",
}

// DefaultAmplifyingCommands returns a copy of the built-in allow-list, for config
// validation and for documenting the effective set.
func DefaultAmplifyingCommands() []string {
	out := make([]string, len(amplifyingCommands))
	copy(out, amplifyingCommands)
	return out
}

// IsAmplifyingCommand reports whether cmd is a maintenance statement worth killing
// when it is blocked by our operation with other sessions queued behind it. allow
// replaces the built-in list when non-empty (never extends it); an absent or empty
// allow means the built-in list, never "match nothing". Matching is case-insensitive,
// space-trimmed, and by prefix. It is deliberately separate from IsMaintenanceCommand,
// which answers a different question for the shrink driver and must not change.
//
// An entry that is empty after trimming is skipped rather than treated as a prefix:
// every string has the empty prefix, so a dangling YAML item would silently turn the
// allow-list into "kill anything we block". Config validation rejects such an entry
// outright; this is the second half of that guard, for any other caller.
func IsAmplifyingCommand(cmd string, allow []string) bool {
	c := strings.ToUpper(strings.TrimSpace(cmd))
	if c == "" {
		return false
	}
	list := amplifyingCommands
	if len(allow) > 0 {
		list = allow
	}
	for _, want := range list {
		want = strings.ToUpper(strings.TrimSpace(want))
		if want == "" {
			continue
		}
		if strings.HasPrefix(c, want) {
			return true
		}
	}
	return false
}
