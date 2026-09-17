package run

import "fmt"

// RunnablePerSchedulerThreshold is the number of runnable tasks per online scheduler at
// which the console narrates CPU pressure: one task waiting per CPU means every scheduler
// has work queued behind the task it is running. Like LogFullThresholdPercent it is a
// constant, not a config key — it does not change what the engine does, it only says out
// loud why the operation (and everything else on the server) is slow.
const RunnablePerSchedulerThreshold = 1.0

// RunnablePerSchedulerRearm is the ratio the alarm must drop back below before it can fire
// again. The gap is hysteresis: a server hovering at the threshold narrates the episode
// once instead of on every poll.
const RunnablePerSchedulerRearm = 0.5

// RunnablePerScheduler is the runnable-tasks-per-online-scheduler ratio the threshold and
// the re-arm level are expressed in. A scheduler count of zero — the read failed, or the
// server did not answer — is a reading nobody made, so it is reported as no pressure rather
// than as a division by zero. Both the alarm and the console's styling flag go through here,
// so they cannot drift apart.
func RunnablePerScheduler(runnable, schedulers int) float64 {
	if schedulers <= 0 {
		return 0
	}
	return float64(runnable) / float64(schedulers)
}

// CPUPressureAlarm is a hysteresis latch on runnable tasks per scheduler, the same shape
// as LogFullAlarm. It starts armed, fires once when an observation reaches the threshold,
// and re-arms only once a later observation drops strictly below the re-arm ratio.
type CPUPressureAlarm struct {
	armed bool
}

// NewCPUPressureAlarm returns an alarm armed and ready to fire on its first observation at
// or above the threshold.
func NewCPUPressureAlarm() *CPUPressureAlarm {
	return &CPUPressureAlarm{armed: true}
}

// Observe reports whether this reading should narrate CPU pressure right now. Call it once
// per poll; it is not idempotent. A scheduler count of zero (the read failed, or the server
// did not answer) never fires: there is no ratio to judge.
func (a *CPUPressureAlarm) Observe(runnable, schedulers int) bool {
	if schedulers <= 0 {
		return false // no ratio to judge: never fire, and never re-arm on a zero
	}
	ratio := RunnablePerScheduler(runnable, schedulers)
	if !a.armed {
		if ratio < RunnablePerSchedulerRearm {
			a.armed = true
		}
		return false
	}
	if ratio >= RunnablePerSchedulerThreshold {
		a.armed = false
		return true
	}
	return false
}

// CPUPressureMessage is the narration line for a fired CPUPressureAlarm.
func CPUPressureMessage(runnable, schedulers int) string {
	return fmt.Sprintf("CPU pressure: %d tasks runnable across %d schedulers (%.1f per scheduler)",
		runnable, schedulers, RunnablePerScheduler(runnable, schedulers))
}
