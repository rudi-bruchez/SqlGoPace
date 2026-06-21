package run

import "time"

// BatchTuning is the run-side carrier of the batch-DML driver parameters. Like
// ShrinkTuning it mirrors a config block but lives here so internal/run stays free
// of the config package; cmd maps config.BatchDMLConfig into a BatchTuning.
type BatchTuning struct {
	InitialSmallRows  int           // estimated table < 100k rows
	InitialMediumRows int           // 100k–1M rows
	InitialLargeRows  int           // > 1M rows
	MinRows           int           // batch floor
	MaxRows           int           // batch ceiling
	EscalationCapRows int           // batch ceiling when RCSI is off (avoid a table-lock escalation that freezes readers)
	TargetBatch       time.Duration // ideal per-batch duration
	SelfWaitTimeout   time.Duration // max cumulative wait while stopped under pressure before a clean stop
}

// Row-count thresholds (documented constants, not exposed in config) that classify
// the estimated table size into the small/medium/large initial-batch tiers.
const (
	batchSmallCeilingRows  = 100_000
	batchMediumCeilingRows = 1_000_000
)

// InitialBatchRows picks the starting batch size from the estimated row count. The
// tiers are conservative starting points; AdjustBatchRows grows the batch from here
// when the I/O keeps up. The result is clamped to [MinRows, MaxRows]; the driver
// applies any RCSI escalation cap on top.
func InitialBatchRows(estRows int64, t BatchTuning) int {
	switch {
	case estRows < batchSmallCeilingRows:
		return clampRows(t.InitialSmallRows, t.MinRows, t.MaxRows)
	case estRows <= batchMediumCeilingRows:
		return clampRows(t.InitialMediumRows, t.MinRows, t.MaxRows)
	default:
		return clampRows(t.InitialLargeRows, t.MinRows, t.MaxRows)
	}
}

// AdjustBatchRows returns the next batch size given the last batch's duration and
// wait deltas. It halves the size under I/O pressure or sustained blocking, doubles
// it when I/O is light and the batch finished within the target duration, and clamps
// to [lo, hi]. It reuses the same wait thresholds as the shrink stepsize, so DML and
// shrink throttle on the same signals. Reduction takes precedence.
func AdjustBatchRows(size int, elapsed time.Duration, w WaitDeltas, target time.Duration, lo, hi int) int {
	reduce := w.WriteLogAvgMs > writeLogReduceMs ||
		w.PageIOLatchExAvgMs > pageIOLatchReduceMs ||
		w.BlockingSeconds > blockingReduceSeconds
	grow := w.WriteLogAvgMs < ioLatchGrowMs &&
		w.PageIOLatchExAvgMs < ioLatchGrowMs &&
		w.BlockingSeconds == 0 &&
		elapsed < target

	switch {
	case reduce:
		size /= 2
	case grow:
		size *= 2
	}
	return clampRows(size, lo, hi)
}

// clampRows bounds n to [lo, hi].
func clampRows(n, lo, hi int) int {
	return min(max(n, lo), hi)
}
