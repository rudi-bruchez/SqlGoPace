package run_test

import (
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

func TestManualClock(t *testing.T) {
	start := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	clk := run.NewManualClock(start)

	if !clk.Now().Equal(start) {
		t.Errorf("Now() = %v, want %v", clk.Now(), start)
	}
	clk.Advance(90 * time.Second)
	if got := clk.Since(start); got != 90*time.Second {
		t.Errorf("Since(start) = %v, want 90s", got)
	}
}
