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

// Cumulative-row ceilings for one predicate-strategy operation. They are the
// backstop against a predicate that never exhausts, not a quota: no correct batch
// loop comes close to them.
const (
	// unknownTableRowCeiling applies when the row estimate is unavailable. With
	// nothing to scale against it is deliberately generous.
	unknownTableRowCeiling = 1_000_000
	// smallTableRowCeiling keeps the bound sane for a tiny or stale estimate, where
	// twice the estimate would be a handful of rows.
	smallTableRowCeiling = 10_000
)

// predicateRowCeiling is the cumulative-row bound for one predicate-strategy
// operation. A terminating predicate affects each row at most once — the self-limit
// clause excludes rows already at their target value, and a DELETE removes what it
// matches — so cumulative rows cannot exceed the table's row count. Twice that is
// the slack for a stale estimate and for rows inserted while the loop runs.
//
// Crossing it proves the predicate is not self-consuming. The usual cause is a
// set_raw that does not consume its own filter ("Counter = Counter + 1" WHERE
// "Status = 'A'"), for which selfLimitClause can emit nothing and validation can
// only check that *a* predicate exists. Every batch autocommits, so without this the
// loop churns rows at full write rate until a human notices.
//
// A false trip is cheap: each batch is committed and the self-limit makes a re-run
// continue, so erring tight costs a re-run while erring loose costs write volume.
func predicateRowCeiling(est int64) int64 {
	if est <= 0 {
		return unknownTableRowCeiling
	}
	return max(2*est, smallTableRowCeiling)
}

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

// Thresholds specific to the batch-DML law. The shrink stepsize no longer consults either:
// AdjustStepMB grows on the absence of pressure rather than on near-idle storage, and takes its
// blocking signal from whether the supervisor stopped the batch. See the note on AdjustBatchRows.
const (
	ioLatchGrowMs         = 5  // both latencies below this → headroom to grow
	blockingReduceSeconds = 30 // blocking others longer than this → back off
)

// AdjustBatchRows returns the next batch size given the last batch's duration and
// wait deltas. It halves the size under I/O pressure or sustained blocking, doubles
// it when I/O is light and the batch finished within the target duration, and clamps
// to [lo, hi]. Reduction takes precedence.
//
// It shares the reduce thresholds with the shrink stepsize but not the law: AdjustStepMB moved
// to AIMD in v0.17.0 because a duration-gated growth condition a multi-GB chunk can never meet
// made every reduction permanent. A DML batch is not in that position — it holds locks for its
// whole duration and has no per-invocation restart cost, so a duration objective is the right
// one here. Note w.BlockingSeconds is never populated by waitDeltas, so that clause is inert.
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
