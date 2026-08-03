package run

import (
	"fmt"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// AsyncStatsSetting is the state of the ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY
// database-scoped configuration, which exists from SQL Server 2022 (major 16).
type AsyncStatsSetting int

const (
	// AsyncStatsAbsent means the setting does not exist on this server.
	AsyncStatsAbsent AsyncStatsSetting = iota
	// AsyncStatsOff means it exists and is off.
	AsyncStatsOff
	// AsyncStatsOn means it exists and is on.
	AsyncStatsOn
)

const asyncStatsLimitation = "It does NOT cover an explicit UPDATE STATISTICS run by a job or by hand; " +
	"those still block and can queue readers behind them."

// asyncStatsAdvisory returns the advisory to emit before op, and whether to emit it.
// Like reorgRCSIWarning it self-gates to REORGANIZE and takes the database name so the
// message is complete and the helper stays pure.
//
// The limitation is stated in every emitted variant, including when the setting is
// already on: the configuration covers only asynchronous automatic statistics updates,
// and an operator who enables it and assumes explicit UPDATE STATISTICS is handled
// will be surprised by exactly the incident this feature exists for.
func asyncStatsAdvisory(op ddl.Operation, database string, setting AsyncStatsSetting) (string, bool) {
	reorg, ok := op.(ddl.ReorganizeIndex)
	if !ok || setting == AsyncStatsAbsent {
		return "", false
	}
	if setting == AsyncStatsOn {
		return fmt.Sprintf(
			"%s.%s: ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY is on for %s. %s",
			reorg.Schema, reorg.Table, database, asyncStatsLimitation), true
	}
	return fmt.Sprintf(
		"%s.%s: ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY is OFF on %s — enabling it "+
			"(ALTER DATABASE SCOPED CONFIGURATION SET ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY = ON) "+
			"lets automatic statistics updates queue at low priority instead of blocking this "+
			"REORGANIZE. %s",
		reorg.Schema, reorg.Table, database, asyncStatsLimitation), true
}
