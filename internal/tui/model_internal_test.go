package tui

import (
	"testing"
	"time"
)

func TestFormatElapsed(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "00:05"},
		{3*time.Minute + 20*time.Second, "03:20"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1:02:03"},
	}
	for _, tt := range tests {
		if got := formatElapsed(tt.d); got != tt.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestHumanizeMS(t *testing.T) {
	const h = int64(3_600_000)
	tests := []struct {
		ms   int64
		want string
	}{
		{500, "500ms"},
		{2800, "2.8s"},
		{6*60_000 + 35_000, "6m35s"},
		{1*h + 4*60_000, "1h04m"},
		// Past 72h the unit rolls from hours to days.
		{71*h + 59*60_000, "71h59m"},
		{72 * h, "3d00h"},
		{774*h + 58*60_000, "32d06h"}, // the reported ETA: 774h58m
	}
	for _, tt := range tests {
		if got := humanizeMS(tt.ms); got != tt.want {
			t.Errorf("humanizeMS(%d) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestOpStatusLabel(t *testing.T) {
	tests := map[string]string{
		"success": "DONE", "failed": "FAILED", "interrupted": "INTERRUPTED",
		"incomplete": "INCOMPLETE", "skipped": "SKIPPED", "weird": "WEIRD",
	}
	for outcome, want := range tests {
		if got := opStatusLabel(outcome); got != want {
			t.Errorf("opStatusLabel(%q) = %q, want %q", outcome, got, want)
		}
	}
}

func TestJoinRowStacksWhenNarrow(t *testing.T) {
	a, b := "AAA", "BBB"
	// Wide: side by side, so the two panels share lines (their content is not separated by a
	// bare newline join). Narrow: stacked, so a newline separates them.
	if got := joinRow(sideBySideMin, a, b); got == a+"\n"+b {
		t.Errorf("at width %d joinRow should place panels side by side, got stacked", sideBySideMin)
	}
	if got := joinRow(sideBySideMin-1, a, b); got != a+"\n"+b {
		t.Errorf("below the threshold joinRow should stack: got %q", got)
	}
}

func TestSetOpStatus(t *testing.T) {
	m := Model{ops: []OperationRow{{Index: 1, Status: "TO RUN"}, {Index: 2, Status: "TO RUN"}}}
	m.setOpStatus(2, "RUNNING")
	if m.ops[1].Status != "RUNNING" || m.ops[0].Status != "TO RUN" {
		t.Errorf("setOpStatus targeted the wrong row: %+v", m.ops)
	}
	m.setOpStatus(99, "DONE") // unknown index: no-op, no panic
}

func TestTickUpdatesElapsed(t *testing.T) {
	start := time.Unix(1000, 0)
	m := Model{startedAt: start}
	updated, cmd := m.Update(tickMsg(start.Add(90 * time.Second)))
	m = updated.(Model)
	if m.elapsed != 90*time.Second {
		t.Errorf("elapsed = %v, want 90s", m.elapsed)
	}
	if cmd == nil {
		t.Error("tick should reschedule itself (non-nil cmd)")
	}
}
