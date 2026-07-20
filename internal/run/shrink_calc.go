package run

import (
	"math"
	"time"
)

// estimateShrink projects the chunks and time left from the reduction achieved so far.
// chunksRemaining scales the remaining MB by the average MB per completed chunk;
// etaSeconds scales the remaining MB by the achieved reduction rate. Both are zero until
// there is signal (a completed chunk, some elapsed time, and MB still to reclaim), so a
// just-started or finished shrink reports no estimate rather than a misleading one.
func estimateShrink(start, current, final, chunksDone int, elapsed time.Duration) (chunksRemaining, etaSeconds int) {
	doneMB := start - current
	remainingMB := current - final
	if doneMB <= 0 || remainingMB <= 0 {
		return 0, 0
	}
	if chunksDone > 0 {
		chunksRemaining = int(math.Ceil(float64(remainingMB) / (float64(doneMB) / float64(chunksDone))))
	}
	if secs := elapsed.Seconds(); secs > 0 {
		if rate := float64(doneMB) / secs; rate > 0 {
			etaSeconds = int(math.Ceil(float64(remainingMB) / rate))
		}
	}
	return chunksRemaining, etaSeconds
}

// ShrinkTuning is the run-side carrier of the DBCC SHRINKFILE driver parameters.
// It mirrors config.ShrinkConfig but lives here so internal/run stays free of the
// config package: the engine consumes primitives (durations/ints), exactly as it
// consumes ddl.Policy rather than the raw config. cmd maps config.ShrinkConfig
// into a ShrinkTuning when wiring the engine.
type ShrinkTuning struct {
	InitialStepSmallMB   int           // reclaim < 5 GB
	InitialStepMediumMB  int           // reclaim 5–50 GB
	InitialStepLargeMB   int           // reclaim > 50 GB
	MinStepMB            int           // step floor
	MaxStepMB            int           // step ceiling
	TargetBatch          time.Duration // ideal per-chunk duration
	MaxNoProgress        int           // consecutive no-gain chunks before clean stop
	NoProgressBackoff    time.Duration // initial backoff after a no-progress chunk
	NoProgressBackoffMax time.Duration // backoff ceiling
	SelfWaitTimeout      time.Duration // max wait while blocked (Sch-M/snapshot)
	LogReuseWaitTimeout  time.Duration // max wait for a scheduled log backup
}

// WaitDeltas captures the per-chunk change in the waits that gate shrink stepsize:
// average WRITELOG and PAGEIOLATCH_EX latency (milliseconds) and how long this
// shrink blocked other sessions (seconds). These are deltas measured over a single
// chunk — never the cumulative values from sys.dm_os_wait_stats, which would bias
// every decision toward the server's lifetime history.
type WaitDeltas struct {
	WriteLogAvgMs      float64
	PageIOLatchExAvgMs float64
	BlockingSeconds    float64
}

// Stepsize thresholds (documented constants, not exposed in config): they classify
// the volume to reclaim into the small/medium/large initial-step tiers.
const (
	reclaimSmallCeilingMB  = 5 * 1024  // < 5 GB
	reclaimMediumCeilingMB = 50 * 1024 // 5–50 GB
)

// Wait thresholds that gate the step adjustment (design §7.2).
const (
	writeLogReduceMs      = 10 // WRITELOG avg above this → I/O is the bottleneck
	pageIOLatchReduceMs   = 20 // PAGEIOLATCH_EX avg above this → read I/O is the bottleneck
	ioLatchGrowMs         = 5  // both latencies below this → headroom to grow
	blockingReduceSeconds = 30 // blocking others longer than this → back off
)

// InitialStepMB picks the starting chunk size in megabytes from the volume to
// reclaim. The tiers are deliberately conservative starting points; AdjustStepMB
// raises the step from here when the I/O keeps up.
func InitialStepMB(reclaimMB int, t ShrinkTuning) int {
	switch {
	case reclaimMB < reclaimSmallCeilingMB:
		return t.InitialStepSmallMB
	case reclaimMB <= reclaimMediumCeilingMB:
		return t.InitialStepMediumMB
	default:
		return t.InitialStepLargeMB
	}
}

// AdjustStepMB returns the next chunk size given the last chunk's duration and wait
// deltas. It halves the step under I/O pressure or sustained blocking, doubles it
// when I/O is light and the chunk finished within the target batch duration, and
// clamps the result to [MinStepMB, MaxStepMB]. Reduction takes precedence: the two
// conditions are mutually exclusive in practice (one needs high latency, the other
// low), but the order makes the safe choice explicit.
func AdjustStepMB(step int, elapsed time.Duration, w WaitDeltas, t ShrinkTuning) int {
	reduce := w.WriteLogAvgMs > writeLogReduceMs ||
		w.PageIOLatchExAvgMs > pageIOLatchReduceMs ||
		w.BlockingSeconds > blockingReduceSeconds
	grow := w.WriteLogAvgMs < ioLatchGrowMs &&
		w.PageIOLatchExAvgMs < ioLatchGrowMs &&
		w.BlockingSeconds == 0 &&
		elapsed < t.TargetBatch

	switch {
	case reduce:
		step /= 2
	case grow:
		step *= 2
	}
	return clampStep(step, t.MinStepMB, t.MaxStepMB)
}

// clampStep bounds step to [lo, hi].
func clampStep(step, lo, hi int) int {
	return min(max(step, lo), hi)
}

// NextTargetMB is the target size for the next chunk: one step below current, but
// never past the final target. The last chunk therefore lands exactly on final.
func NextTargetMB(current, step, final int) int {
	next := current - step
	if next < final {
		return final
	}
	return next
}
