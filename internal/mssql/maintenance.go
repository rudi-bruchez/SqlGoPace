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
