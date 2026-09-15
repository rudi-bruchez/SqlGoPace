package run_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

func TestLogFullAlarmFiresAtThreshold(t *testing.T) {
	a := run.NewLogFullAlarm()
	if a.Observe(50) {
		t.Fatal("must not fire below threshold")
	}
	if a.Observe(89.9) {
		t.Fatal("must not fire just under threshold")
	}
	if !a.Observe(90) {
		t.Fatal("must fire at exactly the threshold")
	}
}

func TestLogFullAlarmFiresOnceUntilRearmed(t *testing.T) {
	a := run.NewLogFullAlarm()
	if !a.Observe(95) {
		t.Fatal("first crossing must fire")
	}
	// Sustained high usage: must not re-fire without dropping below the re-arm level.
	if a.Observe(96) {
		t.Fatal("must not re-fire while sustained above threshold")
	}
	if a.Observe(90) {
		t.Fatal("must not re-fire while still at/above threshold")
	}
	// Dipping between the re-arm level and the threshold does not re-arm.
	if a.Observe(87) {
		t.Fatal("87 must not fire (still above re-arm level)")
	}
	if a.Observe(95) {
		t.Fatal("must not fire: never dropped below the re-arm level")
	}
	// Dropping below the re-arm level re-arms the alarm.
	if a.Observe(84) {
		t.Fatal("dropping below re-arm level must not itself fire")
	}
	if !a.Observe(90) {
		t.Fatal("must fire again after re-arming")
	}
}

func TestLogFullAlarmRearmExactBoundary(t *testing.T) {
	a := run.NewLogFullAlarm()
	a.Observe(95) // fires, disarms
	if a.Observe(85) {
		t.Fatal("85 (the re-arm level itself) must not yet re-arm")
	}
	if a.Observe(90) {
		t.Fatal("must still be disarmed: 85 does not count as 'below' the re-arm level")
	}
}
