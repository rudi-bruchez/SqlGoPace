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
	InitialStepSmallMB    int           // legacy tier: reclaim < 5 GB (used only when TargetChunks <= 0)
	InitialStepMediumMB   int           // legacy tier: reclaim 5–50 GB
	InitialStepLargeMB    int           // legacy tier: reclaim > 50 GB
	TargetChunks          int           // aim to finish in ~this many chunks (initial step = reclaim / TargetChunks)
	MaxStepPctOfFile      int           // per-file step ceiling as a percent of file size (0 disables)
	MinStepMB             int           // step floor
	MaxStepMB             int           // absolute step ceiling
	MaxChunkDuration      time.Duration // ceiling on chunk duration: stops growth, never reduces
	MaxNoProgress         int           // consecutive no-gain chunks before clean stop
	NoProgressBeforeFlush int           // no-progress events before the tempdb cache flush (tempdb path only)
	NoProgressBackoff     time.Duration // initial backoff after a no-progress chunk
	NoProgressBackoffMax  time.Duration // backoff ceiling
	SelfWaitTimeout       time.Duration // max wait while blocked (Sch-M/snapshot)
	LogReuseWaitTimeout   time.Duration // max wait for a scheduled log backup
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

// Tail-object walk bounds (see the tail-object design spec §1). The walk skips the
// file's trailing unallocated pages until it reaches the last allocated page; that run
// can never exceed the file's total free-page count, so the cap is derived from free
// space, with an absolute backstop for a mostly-free file whose last allocated page sits
// near the front.
const (
	tailWalkMargin = 512    // extra pages absorbing concurrent allocation churn
	tailWalkAbsCap = 262144 // ~2 GB at 8 KB/page: hard ceiling on the backward loop
)

// Wait thresholds that gate the step adjustment (design §7.2). Shared with AdjustBatchRows,
// so DML and shrink back off on the same latencies.
const (
	writeLogReduceMs    = 10 // WRITELOG avg above this → I/O is the bottleneck
	pageIOLatchReduceMs = 20 // PAGEIOLATCH_EX avg above this → read I/O is the bottleneck
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

// AdjustStepMB returns the next chunk size, clamped to [MinStepMB, maxStep] (the per-file
// effective ceiling). It is an AIMD loop, and the two rates are load-bearing: /2 against *5/4
// means recovering one halving costs log2/log1.25 ≈ 3.1 clean chunks, so the step trends upward
// while pressure is rarer than about one chunk in three. Changing either rate means re-deriving
// that ratio.
//
// Pressure is either a wait the shrink itself suffered (WRITELOG or PAGEIOLATCH_EX past the
// thresholds) or stopped — the supervisor cut the chunk short because it was blocking others or
// filling the log. Growth is simply the absence of pressure, deliberately *not* also gated on
// near-idle storage: a shrink is itself an I/O generator, so a "quiet enough to grow" test is one
// it can rarely pass, and pairing it with a duration test it can never pass (a multi-GB chunk
// takes minutes) made every reduction permanent — the step then walked down to MinStepMB, where
// the fixed per-invocation cost of DBCC SHRINKFILE dominates the work done.
//
// t.MaxChunkDuration is a ceiling, not a target: it stops growth, it never forces a reduction. So a
// reduction is recoverable only while the resulting chunk still finishes inside the ceiling;
// above it the step holds where it landed. That bound is intended — the ceiling means "no chunk
// longer than this" — and it replaces an unbounded descent with one an operator sets.
func AdjustStepMB(step int, elapsed time.Duration, w WaitDeltas, stopped bool, t ShrinkTuning, maxStep int) int {
	switch {
	case stopped || w.WriteLogAvgMs > writeLogReduceMs || w.PageIOLatchExAvgMs > pageIOLatchReduceMs:
		return halveStep(step, t, maxStep)
	case elapsed < t.MaxChunkDuration: // at or past the ceiling, hold rather than push further
		step = max(step*5/4, step+1) // +1 so integer truncation cannot stall a tiny MinStepMB
	}
	return clampStep(step, t.MinStepMB, maxStep)
}

// halveStep is the multiplicative-decrease half of the AIMD law. It lives here, rather than
// inline, because the driver halves the step on a second path too — a chunk that errored without
// moving a page (see ShrinkRunner.shrinkData). Both go through this so the decrease has one home:
// anyone re-deriving the stability ratio in AdjustStepMB has to account for both callers.
func halveStep(step int, t ShrinkTuning, maxStep int) int {
	return clampStep(step/2, t.MinStepMB, maxStep)
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

// tailWalkPages returns how many pages back the tail-object walk may scan for a data file
// with freeMB megabytes free: min(free pages + margin, absolute cap), floored at the
// margin so a full/near-full file still gets a small look-back window.
func tailWalkPages(freeMB int) int {
	pages := freeMB*128 + tailWalkMargin
	if pages < tailWalkMargin {
		pages = tailWalkMargin
	}
	return min(pages, tailWalkAbsCap)
}
