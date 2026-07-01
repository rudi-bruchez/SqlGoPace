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
