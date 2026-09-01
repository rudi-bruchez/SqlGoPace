package main

import (
	"fmt"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
)

// simpleRecovery is the only recovery model in which a CHECKPOINT releases log space.
const simpleRecovery = "SIMPLE"

// checkpointBetweenOperations reports whether the engine should issue a CHECKPOINT
// between a manifest's operations: the operator asked for it AND the database is in
// SIMPLE recovery, where a CHECKPOINT truncates the log. Under FULL or BULK_LOGGED the
// log is only released by a log backup, so a CHECKPOINT there costs a round trip and
// frees nothing — which is what the shipped config's own comment says.
//
// Pure over config plus the one server fact it needs, so the decision is tested without
// a server (as amplifierDwellWarning is). An unknown model is not assumed to be SIMPLE:
// issuing a pointless statement is better than nothing, but claiming a protection that
// was never established is what this key did for its whole existence.
func checkpointBetweenOperations(cfg *config.Config, recoveryModel string) bool {
	return cfg.Monitoring.CheckpointBetweenOperations &&
		strings.EqualFold(strings.TrimSpace(recoveryModel), simpleRecovery)
}

// checkpointIneffectiveWarning returns the startup warning owed to an operator who set
// checkpoint_between_operations on a database where it cannot work, or "" when none is
// due. The key was dead for its whole existence; a silently ignored setting is how it
// stayed unnoticed, so the one case that still does nothing says so out loud.
func checkpointIneffectiveWarning(cfg *config.Config, recoveryModel string) string {
	if !cfg.Monitoring.CheckpointBetweenOperations || checkpointBetweenOperations(cfg, recoveryModel) {
		return ""
	}
	model := strings.TrimSpace(recoveryModel)
	if model == "" {
		model = "an unreported"
	}
	return fmt.Sprintf("-- warning: monitoring.checkpoint_between_operations is set, but this database is in "+
		"%s recovery model, where a CHECKPOINT frees no log space (only a log backup does). No checkpoint will be issued",
		model)
}
