package run_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

func TestCPUPressureAlarmFiresOncePerEpisode(t *testing.T) {
	a := run.NewCPUPressureAlarm()
	// 16 schedulers, 16 runnable tasks: one task waiting per CPU.
	if !a.Observe(16, 16) {
		t.Fatal("first observation at the threshold did not fire")
	}
	if a.Observe(24, 16) {
		t.Error("a worse observation fired again; the alarm must narrate an episode once")
	}
	if a.Observe(12, 16) {
		t.Error("an observation still above the re-arm ratio fired")
	}
}

func TestCPUPressureAlarmRearmsAfterRelief(t *testing.T) {
	a := run.NewCPUPressureAlarm()
	a.Observe(16, 16)
	if a.Observe(4, 16) { // 0.25 per scheduler: relief, re-arms but does not fire
		t.Fatal("the re-arming observation fired")
	}
	if !a.Observe(20, 16) {
		t.Error("a new episode after relief did not fire")
	}
}

func TestCPUPressureAlarmRearmExactBoundary(t *testing.T) {
	// The mirror of TestLogFullAlarmRearmExactBoundary: re-arming is on *strictly*
	// below the re-arm ratio, so the ratio itself does not re-arm.
	a := run.NewCPUPressureAlarm()
	a.Observe(24, 16) // fires, disarms
	if a.Observe(8, 16) {
		t.Fatal("0.5 per scheduler (the re-arm ratio itself) must not yet re-arm")
	}
	if a.Observe(24, 16) {
		t.Fatal("must still be disarmed: the re-arm ratio does not count as 'below' it")
	}
}

func TestCPUPressureAlarmStaysQuietBelowThreshold(t *testing.T) {
	a := run.NewCPUPressureAlarm()
	// A few tasks waiting on a big box is normal, not an incident.
	if a.Observe(8, 16) {
		t.Error("0.5 runnable per scheduler fired; the threshold is one per scheduler")
	}
}

func TestCPUPressureAlarmIgnoresUnknownSchedulerCount(t *testing.T) {
	a := run.NewCPUPressureAlarm()
	// A server that did not answer the scheduler read must not produce a division by
	// zero, nor an alarm built on a count nobody measured.
	if a.Observe(30, 0) {
		t.Error("fired with an unknown scheduler count")
	}
}

func TestRunnablePerScheduler(t *testing.T) {
	if got := run.RunnablePerScheduler(24, 16); got != 1.5 {
		t.Errorf("RunnablePerScheduler(24, 16) = %v, want 1.5", got)
	}
	// An unknown scheduler count is a reading nobody made, not a busy server: it must
	// read as zero everywhere the ratio is consumed, and never divide by zero.
	if got := run.RunnablePerScheduler(30, 0); got != 0 {
		t.Errorf("RunnablePerScheduler(30, 0) = %v, want 0", got)
	}
}

func TestCPUPressureMessage(t *testing.T) {
	got := run.CPUPressureMessage(24, 16)
	want := "CPU pressure: 24 tasks runnable across 16 schedulers (1.5 per scheduler)"
	if got != want {
		t.Errorf("CPUPressureMessage = %q, want %q", got, want)
	}
}
