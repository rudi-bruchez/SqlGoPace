package run

import (
	"context"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
)

// preflightChecker adapts preflight.Run to the Engine's Preflighter interface,
// capturing the prober, detected server info, log thresholds, and whether the blocker-kill
// policy is armed (so the ALTER ANY CONNECTION advisory runs).
type preflightChecker struct {
	prober     preflight.Prober
	info       mssql.ServerInfo
	thresholds preflight.Thresholds
	killArmed  bool
}

// NewPreflightChecker builds a Preflighter from a prober, detected server info, log
// thresholds, and whether blocker-killing is armed (kill_blockers or allow_abort_blockers).
func NewPreflightChecker(prober preflight.Prober, info mssql.ServerInfo, thresholds preflight.Thresholds, killArmed bool) Preflighter {
	return preflightChecker{prober: prober, info: info, thresholds: thresholds, killArmed: killArmed}
}

func (p preflightChecker) Check(ctx context.Context, m *ddl.Manifest) (preflight.Report, error) {
	return preflight.Run(ctx, p.prober, p.info, m, p.thresholds, p.killArmed)
}
