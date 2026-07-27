package run

import (
	"math"
	"time"
)

// estimateShrink projects the chunks and time left from the reduction achieved so far.
// chunksRemaining scales the remaining MB by the average MB per completed chunk; etaSeconds
// scales the remaining MB by the achieved reduction rate over the full elapsed time (the
// honest "with blocking" figure); etaNoBlockSeconds does the same over productive time only
// (elapsed minus blocked), the rate the shrink would sustain if it were never starved;
// avgChunkSeconds is the observed wall-clock cadence, elapsed per completed chunk (waits
// included, matching etaSeconds). The three projections are zero until there is signal (a
// completed chunk, some elapsed time, and MB still to reclaim), so a just-started or finished
// shrink reports no estimate rather than a misleading one; avgChunkSeconds needs only a
// completed chunk, so it survives to the last frame when the ETA has already vanished.
func estimateShrink(start, current, final, chunksDone int, elapsed, blocked time.Duration) (chunksRemaining, etaSeconds, etaNoBlockSeconds, avgChunkSeconds int) {
	doneMB := start - current
	remainingMB := current - final
	if chunksDone > 0 && elapsed > 0 {
		avgChunkSeconds = int(math.Round(elapsed.Seconds() / float64(chunksDone)))
	}
	if doneMB <= 0 || remainingMB <= 0 {
		return 0, 0, 0, avgChunkSeconds
	}
	if chunksDone > 0 {
		chunksRemaining = int(math.Ceil(float64(remainingMB) / (float64(doneMB) / float64(chunksDone))))
	}
	etaSeconds = etaFrom(doneMB, remainingMB, elapsed)
	etaNoBlockSeconds = etaFrom(doneMB, remainingMB, elapsed-blocked)
	if etaNoBlockSeconds == 0 {
		etaNoBlockSeconds = etaSeconds // no measurable productive time yet: fall back
	}
	return chunksRemaining, etaSeconds, etaNoBlockSeconds, avgChunkSeconds
}

// etaFrom scales remainingMB by the doneMB-per-window reduction rate. A non-positive window
// or rate yields 0 (no estimate).
func etaFrom(doneMB, remainingMB int, window time.Duration) int {
	secs := window.Seconds()
	if secs <= 0 {
		return 0
	}
	rate := float64(doneMB) / secs
	if rate <= 0 {
		return 0
	}
	return int(math.Ceil(float64(remainingMB) / rate))
}

// ShrinkTuning is the run-side carrier of the DBCC SHRINKFILE driver parameters.
// It mirrors config.ShrinkConfig but lives here so internal/run stays free of the
// config package: the engine consumes primitives (durations/ints), exactly as it
// consumes ddl.Policy rather than the raw config. cmd maps config.ShrinkConfig
// into a ShrinkTuning when wiring the engine.
type ShrinkTuning struct {
	InitialStepSmallMB   int           // legacy tier: reclaim < 5 GB (used only when TargetChunks <= 0)
	InitialStepMediumMB  int           // legacy tier: reclaim 5–50 GB
	InitialStepLargeMB   int           // legacy tier: reclaim > 50 GB
	TargetChunks         int           // aim to finish in ~this many chunks (initial step = reclaim / TargetChunks)
	MaxStepPctOfFile     int           // per-file step ceiling as a percent of file size (0 disables)
	MinStepMB            int           // step floor
	MaxStepMB            int           // absolute step ceiling
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

// InitialStepMB picks the starting chunk size in megabytes from the volume to reclaim and
// the file size. With TargetChunks set (the default), it sizes the first chunk to finish the
// whole shrink in about that many chunks — step = ceil(reclaimMB / TargetChunks) — so a
// multi-TB reclaim starts with large chunks and completes in ~TargetChunks moves instead of
// tens of thousands. Without it, it falls back to the reclaim-volume tiers. The result is
// clamped to [MinStepMB, effectiveMaxStepMB(fileSizeMB, t)]; AdjustStepMB adapts from here.
func InitialStepMB(reclaimMB, fileSizeMB int, t ShrinkTuning) int {
	var step int
	switch {
	case t.TargetChunks > 0:
		step = (reclaimMB + t.TargetChunks - 1) / t.TargetChunks // ceil
	case reclaimMB < reclaimSmallCeilingMB:
		step = t.InitialStepSmallMB
	case reclaimMB <= reclaimMediumCeilingMB:
		step = t.InitialStepMediumMB
	default:
		step = t.InitialStepLargeMB
	}
	return clampStep(step, t.MinStepMB, effectiveMaxStepMB(fileSizeMB, t))
}

// effectiveMaxStepMB is the per-file step ceiling: MaxStepMB, further capped to
// MaxStepPctOfFile percent of the file so a small file uses proportionally smaller chunks. A
// zero percent disables the file-relative cap. The result never drops below MinStepMB.
func effectiveMaxStepMB(fileSizeMB int, t ShrinkTuning) int {
	m := t.MaxStepMB
	if t.MaxStepPctOfFile > 0 {
		if byFile := fileSizeMB * t.MaxStepPctOfFile / 100; byFile < m {
			m = byFile
		}
	}
	if m < t.MinStepMB {
		m = t.MinStepMB
	}
	return m
}

// AdjustStepMB returns the next chunk size given the last chunk's duration and wait
// deltas. It halves the step under I/O pressure or sustained blocking, doubles it
// when I/O is light and the chunk finished within the target batch duration, and
// clamps the result to [MinStepMB, maxStep] (the per-file effective ceiling). Reduction
// takes precedence: the two conditions are mutually exclusive in practice (one needs high
// latency, the other low), but the order makes the safe choice explicit.
func AdjustStepMB(step int, elapsed time.Duration, w WaitDeltas, t ShrinkTuning, maxStep int) int {
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
	return clampStep(step, t.MinStepMB, maxStep)
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
