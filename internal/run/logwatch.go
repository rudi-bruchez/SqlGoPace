package run

import (
	"context"
	"fmt"
)

// watchLog periodically polls the transaction log and emits a warn reaction through sink
// when alarm fires (LogFullAlarm.Observe crossing LogFullThresholdPercent). It runs for
// the duration of one operation, like narrateHeld, and is a no-op without a log watch
// reader or a positive poll interval — WithLogWatch must be wired for it to do anything.
func (e *Engine) watchLog(ctx context.Context, alarm *LogFullAlarm, sink ReactionSink) {
	if e.logWatch == nil {
		return
	}
	pollWhileRunning(ctx, e.logWatchEvery, func() {
		ls, err := e.logWatch.LogSpace(ctx)
		if err != nil {
			return
		}
		if !alarm.Observe(ls.UsedPercent) {
			return
		}
		reuseWait, _ := e.logWatch.LogReuseWait(ctx) // best effort: an empty reuse wait still names the percent
		sink(ReactionEvent{Kind: "warn", Detail: LogFullMessage(ls.UsedPercent, reuseWait)})
	})
}

// LogFullMessage is the wording of a log-full alarm, shared by the run's .log warning and
// the console alert so the two never drift apart.
func LogFullMessage(usedPercent float64, reuseWait string) string {
	return fmt.Sprintf("transaction log %.0f%% full (reuse_wait=%s)", usedPercent, reuseWait)
}
