package run

import (
	"context"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// ExportWindowOpen exposes windowOpen to external tests.
func ExportWindowOpen(e *Engine, ctx context.Context, w *ddl.Window) (bool, error) {
	return e.windowOpen(ctx, w)
}
