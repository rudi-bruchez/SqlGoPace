package run

import (
	"errors"
	"testing"
)

// TestWindowStopReason pins the stop-reason messages: a clock-read failure must not
// be reported as "window closed", and the entry-check stop must not print a per-op
// count (which was pre-expansion and could read nonsensically, e.g. "4/1").
func TestWindowStopReason(t *testing.T) {
	if got := windowStopReason(nil, "after operation 2/5"); got != "window closed after operation 2/5" {
		t.Errorf("mid-run closed = %q, want %q", got, "window closed after operation 2/5")
	}
	if got := windowStopReason(nil, "before this run's operations"); got != "window closed before this run's operations" {
		t.Errorf("entry closed = %q, want %q", got, "window closed before this run's operations")
	}
	if got := windowStopReason(errors.New("boom"), "after operation 2/5"); got != "could not read server time (boom)" {
		t.Errorf("clock error = %q, must name the clock read, not claim the window closed", got)
	}
}
