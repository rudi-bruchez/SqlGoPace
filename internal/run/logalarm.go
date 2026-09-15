package run

// LogFullThresholdPercent is the used-log-space percentage at which the transaction log
// is considered full enough to alert the operator. It is a constant, not a config key: by
// the time the log reaches it, the reaction hierarchy has already had its say at the
// (lower, configurable) log_max_percent — so a LogFullAlarm firing means the log kept
// filling despite the reaction, which is worth a human's attention regardless of what
// log_max_percent is set to.
const LogFullThresholdPercent = 90.0

// LogFullRearmPercent is the used-log-space percentage the alarm must drop back below
// before it re-arms. The gap between it and LogFullThresholdPercent is hysteresis: a log
// hovering around the threshold (e.g. a log backup nibbling it back down to 91% before it
// climbs again) fires once, not on every poll.
const LogFullRearmPercent = 85.0

// LogFullAlarm is a hysteresis latch on transaction-log fullness. It starts armed, fires
// once when an observed used percent reaches LogFullThresholdPercent, and re-arms only
// once a later observation drops strictly below LogFullRearmPercent.
type LogFullAlarm struct {
	armed bool
}

// NewLogFullAlarm returns an alarm armed and ready to fire on its first observation at or
// above the threshold.
func NewLogFullAlarm() *LogFullAlarm {
	return &LogFullAlarm{armed: true}
}

// Observe reports whether usedPercent should fire the alarm right now. Call it once per
// poll; it is not idempotent — a repeated call with the same value will not fire twice.
func (a *LogFullAlarm) Observe(usedPercent float64) bool {
	if !a.armed {
		if usedPercent < LogFullRearmPercent {
			a.armed = true
		}
		return false
	}
	if usedPercent >= LogFullThresholdPercent {
		a.armed = false
		return true
	}
	return false
}
