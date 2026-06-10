package run

import (
	"context"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
)

// preflightChecker adapts preflight.Run to the Engine's Preflighter interface,
// capturing the prober, detected server info, and log thresholds.
type preflightChecker struct {
	prober     preflight.Prober
	info       mssql.ServerInfo
	thresholds preflight.Thresholds
}

// NewPreflightChecker builds a Preflighter from a prober, detected server info,
// and log thresholds.
func NewPreflightChecker(prober preflight.Prober, info mssql.ServerInfo, thresholds preflight.Thresholds) Preflighter {
	return preflightChecker{prober: prober, info: info, thresholds: thresholds}
}

func (p preflightChecker) Check(ctx context.Context, m *ddl.Manifest) (preflight.Report, error) {
	return preflight.Run(ctx, p.prober, p.info, m, p.thresholds)
}
