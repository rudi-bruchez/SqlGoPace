package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// BatchExecutor is an Executor that can also report rows affected (to detect when a
// predicate loop is exhausted) and read a scalar integer (the next key_range
// watermark). *mssql.Conn satisfies it; the rest of the engine uses the narrower
// Executor.
type BatchExecutor interface {
	Executor
	ExecRows(ctx context.Context, sql string) (int64, error)
	QueryInt(ctx context.Context, sql string) (int64, bool, error)
}

// BatchDMLReader is the narrow set of runtime reads the batch-DML driver needs.
// The production implementation is *mssql.Conn; tests supply a fake.
type BatchDMLReader interface {
	TableRowEstimate(ctx context.Context, schema, table string) (int64, error)
	SessionWaits(ctx context.Context, spid int) ([]mssql.SessionWait, error)
	ClusteringKeyColumns(ctx context.Context, schema, table string) ([]mssql.KeyColumn, error)
}

var _ BatchDMLReader = (*mssql.Conn)(nil)

// WatermarkStore persists the key_range walk position so a crashed batch-DML run
// resumes mid-table instead of restarting. The predicate strategy never touches it.
// Save is best-effort: a failure only means a resume redoes from the last saved
// point, which is safe for the idempotent literal update key_range requires.
type WatermarkStore interface {
	Load(ctx context.Context) (watermark int64, ok bool, err error)
	Save(ctx context.Context, watermark int64) error
}

// noWatermark is the store passed for predicate-strategy operations (which never use
// it) and in tests that do not exercise resume.
type noWatermark struct{}

func (noWatermark) Load(context.Context) (int64, bool, error) { return 0, false, nil }
func (noWatermark) Save(context.Context, int64) error         { return nil }

// BatchDMLProgress is the progress of one batch-DML operation, fed to the TUI. It is
// emitted after each committed batch, so BatchRows is the size just used and
// RowsPerSec is that batch's throughput (rows / batch elapsed).
type BatchDMLProgress struct {
	Schema, Table string
	Verb          string
	RowsDone      int64
	EstRows       int64
	BatchRows     int
	RowsPerSec    float64
}

// Percent returns the fraction of the estimated rows processed, in [0,1]. The
// estimate is approximate, so the value is clamped and is 1 when the estimate is
// unavailable.
func (p BatchDMLProgress) Percent() float64 {
	if p.EstRows <= 0 {
		return 1
	}
	done := min(p.RowsDone, p.EstRows)
	return float64(done) / float64(p.EstRows)
}

// BatchDMLResult is the outcome of one batch-DML operation, for the run report.
type BatchDMLResult struct {
	Schema    string
	Table     string
	Verb      string // "update" | "delete"
	Rows      int64  // total rows affected across committed batches
	Batches   int
	FinalRows int    // the last batch size used (after adaptive sizing)
	Reason    string // why it stopped early; empty on normal completion
}

// BatchDMLRunnerConfig configures a BatchDMLRunner. The reaction timings are the
// same MonitoringConfig values the rest of the engine uses; the batch-specific
// tuning lives in Tuning. RCSI is the target's READ_COMMITTED_SNAPSHOT state, which
// gates the batch-size ceiling (with RCSI off, a too-large batch escalates to a
// table lock that freezes readers).
type BatchDMLRunnerConfig struct {
	Tuning          BatchTuning
	RCSI            bool
	PollInterval    time.Duration
	LogPollInterval time.Duration
	BlockingTimeout time.Duration
	LogDrainTimeout time.Duration
	KillGrace       time.Duration
}

// BatchDMLRunner drives a chunked UPDATE/DELETE: it sizes the initial batch, then
// loops the predicate statement until it affects no rows, sampling between batches
// and reacting with the least destructive mechanism (clean stop with committed
// batches preserved). All sizing logic is pure (batch_calc.go); the I/O is injected
// so the loop is unit-testable without a database. Like ShrinkRunner it reuses
// Executor so a stopped batch's KILL fallback comes from the monitoring pool.
type BatchDMLRunner struct {
	exec     BatchExecutor
	reader   BatchDMLReader
	sampler  Sampler
	clk      Clock
	tuning   BatchTuning
	rcsi     bool
	pollIntv time.Duration
	logPoll  time.Duration
	blockTO  time.Duration
	logDrain time.Duration
	killGr   time.Duration

	progress func(BatchDMLProgress)
	stop     func() bool // graceful stop (cancellable): finish the current batch, then stop
}

// BatchDMLOption customizes a BatchDMLRunner.
type BatchDMLOption func(*BatchDMLRunner)

// WithBatchDMLProgress sets the progress callback (fed to the TUI by the engine).
func WithBatchDMLProgress(f func(BatchDMLProgress)) BatchDMLOption {
	return func(r *BatchDMLRunner) { r.progress = f }
}

// WithBatchDMLStop wires the engine's graceful-stop predicate (the DrainFlag's Draining
// method): once it reports true, the driver finishes the current batch (already committed)
// and stops, returning ErrStopped so the run is left in processing and the next run resumes
// (predicate self-limiting, key_range from the persisted watermark). A Cancel before the
// next batch resumes normally.
func WithBatchDMLStop(stop func() bool) BatchDMLOption {
	return func(r *BatchDMLRunner) { r.stop = stop }
}

// NewBatchDMLRunner builds a BatchDMLRunner. A zero LogPollInterval falls back to
// the blocking poll interval, mirroring NewShrinkRunner.
func NewBatchDMLRunner(exec BatchExecutor, reader BatchDMLReader, sampler Sampler, clk Clock, cfg BatchDMLRunnerConfig, opts ...BatchDMLOption) *BatchDMLRunner {
	logPoll := cfg.LogPollInterval
	if logPoll <= 0 {
		logPoll = cfg.PollInterval
	}
	r := &BatchDMLRunner{
		exec:     exec,
		reader:   reader,
		sampler:  sampler,
		clk:      clk,
		tuning:   cfg.Tuning,
		rcsi:     cfg.RCSI,
		pollIntv: cfg.PollInterval,
		logPoll:  logPoll,
		blockTO:  cfg.BlockingTimeout,
		logDrain: cfg.LogDrainTimeout,
		killGr:   cfg.KillGrace,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run executes the batch-DML operation, dispatching on the chunking strategy: the
// predicate loop (default) or the key_range walk (resumable via the watermark
// store). It returns a single BatchDMLResult.
func (r *BatchDMLRunner) Run(ctx context.Context, op ddl.BatchDML, res ddl.ResolvedOptions, ignore IgnoreSource, wm WatermarkStore, sink ReactionSink) (BatchDMLResult, error) {
	if op.Batch.IsKeyRange() {
		return r.runKeyRange(ctx, op, res, ignore, wm, sink)
	}
	return r.runPredicate(ctx, op, res, ignore, sink)
}

// runPredicate loops the self-limiting predicate statement, each batch committed
// individually, until it affects no rows (the predicate is exhausted).
func (r *BatchDMLRunner) runPredicate(ctx context.Context, op ddl.BatchDML, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) (BatchDMLResult, error) {
	result := BatchDMLResult{Schema: op.Schema, Table: op.Table, Verb: op.Verb}

	// Best-effort estimate: 0 when unavailable just means the smallest initial tier.
	est, _ := r.reader.TableRowEstimate(ctx, op.Schema, op.Table)
	lo, hi := r.batchBounds()
	size := r.initialSize(op, est, lo, hi)
	caps := r.opCaps(res, ignore)

	var stallWaited time.Duration
	for {
		// Graceful stop: each batch commits, so stopping between batches preserves work;
		// the self-limiting predicate makes the next run continue naturally.
		if stopRequested(r.stop) {
			result.Reason = "stopped: graceful stop (work committed per batch)"
			return result, ErrStopped
		}
		stmt := ddl.BatchDMLChunkSQL(op, size, res)
		before, _ := r.reader.SessionWaits(ctx, r.exec.SPID())
		t0 := r.clk.Now()
		stopped, rows, err := r.runBatch(ctx, stmt, caps, sink)
		if err != nil {
			return result, err
		}
		elapsed := r.clk.Since(t0)
		after, _ := r.reader.SessionWaits(ctx, r.exec.SPID())

		if stopped {
			stop, reason, err := r.handleStop(ctx, ignore, sink, &stallWaited)
			if err != nil {
				return result, err
			}
			if stop {
				result.Reason = reason
				return result, nil
			}
			continue
		}

		result.FinalRows = size
		if rows == 0 {
			// The predicate matched nothing: the loop is exhausted — done.
			return result, nil
		}
		result.Rows += rows
		result.Batches++
		stallWaited = 0
		r.emitProgress(op, result.Rows, est, size, perSec(rows, elapsed))
		size = AdjustBatchRows(size, elapsed, waitDeltas(before, after), r.tuning.TargetBatch, lo, hi)
	}
}

// runKeyRange walks the table in ascending key ranges, persisting the watermark after
// each committed batch so a crash resumes mid-table. It is restricted (by validation)
// to an idempotent literal UPDATE, so re-applying the boundary batch on resume is safe.
func (r *BatchDMLRunner) runKeyRange(ctx context.Context, op ddl.BatchDML, res ddl.ResolvedOptions, ignore IgnoreSource, wm WatermarkStore, sink ReactionSink) (BatchDMLResult, error) {
	result := BatchDMLResult{Schema: op.Schema, Table: op.Table, Verb: op.Verb}

	key, err := r.resolveKeyColumn(ctx, op)
	if err != nil {
		return result, err
	}
	est, _ := r.reader.TableRowEstimate(ctx, op.Schema, op.Table)
	lo, hi := r.batchBounds()
	size := r.initialSize(op, est, lo, hi)

	watermark, hasWM, err := wm.Load(ctx)
	if err != nil {
		return result, fmt.Errorf("batch_update key_range: load watermark: %w", err)
	}
	if hasWM {
		sink(ReactionEvent{Kind: "resume", Detail: fmt.Sprintf("key_range resuming %s.%s from %s > %d", op.Schema, op.Table, key, watermark)})
	}

	caps := r.opCaps(res, ignore)
	var stallWaited time.Duration
	for {
		// Graceful stop: the watermark is persisted after each committed batch (below), so
		// stopping between batches lets the next run resume from the last saved key.
		if stopRequested(r.stop) {
			result.Reason = "stopped: graceful stop (work committed per batch)"
			return result, ErrStopped
		}
		next, ok, err := r.exec.QueryInt(ctx, ddl.BatchKeyRangeNextSQL(op, key, size, watermark, hasWM))
		if err != nil {
			return result, err
		}
		if !ok {
			return result, nil // no key left above the watermark: the walk is done
		}

		stmt := ddl.BatchKeyRangeUpdateSQL(op, key, watermark, next, hasWM, res)
		before, _ := r.reader.SessionWaits(ctx, r.exec.SPID())
		t0 := r.clk.Now()
		stopped, rows, err := r.runBatch(ctx, stmt, caps, sink)
		if err != nil {
			return result, err
		}
		elapsed := r.clk.Since(t0)
		after, _ := r.reader.SessionWaits(ctx, r.exec.SPID())

		if stopped {
			stop, reason, err := r.handleStop(ctx, ignore, sink, &stallWaited)
			if err != nil {
				return result, err
			}
			if stop {
				result.Reason = reason
				return result, nil
			}
			continue // retry the same range
		}

		result.Rows += rows
		result.Batches++
		result.FinalRows = size
		watermark, hasWM = next, true
		_ = wm.Save(ctx, watermark) // best-effort; a resume redoes from the last saved point
		stallWaited = 0
		r.emitProgress(op, result.Rows, est, size, perSec(rows, elapsed))
		size = AdjustBatchRows(size, elapsed, waitDeltas(before, after), r.tuning.TargetBatch, lo, hi)
	}
}

// resolveKeyColumn determines the key_range key: the explicit batch.key, else the
// table's single clustered key column. This iteration supports only a single integer
// key (composite or non-integer keys use the predicate strategy instead).
func (r *BatchDMLRunner) resolveKeyColumn(ctx context.Context, op ddl.BatchDML) (string, error) {
	cols, err := r.reader.ClusteringKeyColumns(ctx, op.Schema, op.Table)
	if err != nil {
		return "", err
	}
	if op.Batch.Key != "" {
		for _, c := range cols {
			if strings.EqualFold(c.Name, op.Batch.Key) {
				if !c.IsInteger {
					return "", fmt.Errorf("key_range: key %q is not an integer column (this iteration supports integer keys only)", op.Batch.Key)
				}
				return c.Name, nil
			}
		}
		return "", fmt.Errorf("key_range: key %q is not the clustered key of %s.%s (this iteration requires the key to be the clustered/PK key)", op.Batch.Key, op.Schema, op.Table)
	}
	switch {
	case len(cols) == 0:
		return "", fmt.Errorf("key_range: %s.%s has no clustered key; specify batch.key or use the predicate strategy", op.Schema, op.Table)
	case len(cols) > 1:
		return "", fmt.Errorf("key_range: %s.%s has a composite clustered key; specify a single integer batch.key or use the predicate strategy", op.Schema, op.Table)
	case !cols[0].IsInteger:
		return "", fmt.Errorf("key_range: clustered key %q of %s.%s is not an integer (this iteration supports integer keys only)", cols[0].Name, op.Schema, op.Table)
	}
	return cols[0].Name, nil
}

// opCaps builds the reaction capabilities shared by both strategies. A batch is
// cancel-safe (a single UPDATE/DELETE TOP commits atomically, so a stop rolls it
// back cleanly and committed batches survive); ignore_blocking and max_block_minutes
// flow through exactly as for monitored DDL.
func (r *BatchDMLRunner) opCaps(res ddl.ResolvedOptions, ignore IgnoreSource) Capabilities {
	return Capabilities{
		CancelSafe:     true,
		Ignore:         ignore,
		IgnoreBlocking: res.IgnoreBlocking,
		MaxBlock:       blockCap(res.MaxBlockMinutes),
	}
}

// initialSize picks the starting batch size: the per-op override when set, else the
// estimate-driven tier, clamped to the (RCSI-aware) bounds.
func (r *BatchDMLRunner) initialSize(op ddl.BatchDML, est int64, lo, hi int) int {
	size := InitialBatchRows(est, r.tuning)
	if op.Batch.InitialRows != nil {
		size = *op.Batch.InitialRows
	}
	return clampRows(size, lo, hi)
}

// handleStop waits for the pressure that stopped a batch to clear, then reports
// whether the operation should stop cleanly (with a reason) rather than retry. A
// log-drain timeout or exceeding the self-wait budget both end the run cleanly with
// committed batches preserved.
func (r *BatchDMLRunner) handleStop(ctx context.Context, ignore IgnoreSource, sink ReactionSink, stallWaited *time.Duration) (stop bool, reason string, err error) {
	t1 := r.clk.Now()
	if err := r.awaitRelief(ctx, ignore, sink); err != nil {
		if errors.Is(err, ErrLogDrainTimeout) {
			return true, "stopped: log did not drain before timeout (committed batches preserved)", nil
		}
		return false, "", err
	}
	*stallWaited += r.clk.Since(t1)
	if r.tuning.SelfWaitTimeout > 0 && *stallWaited >= r.tuning.SelfWaitTimeout {
		return true, "stopped: blocked longer than the self-wait timeout (committed batches preserved)", nil
	}
	return false, "", nil
}

// batchBounds returns the [min, max] batch size. With RCSI off, the ceiling is
// lowered to the escalation cap so a batch never grows large enough to escalate to
// a table X lock that would freeze readers.
func (r *BatchDMLRunner) batchBounds() (lo, hi int) {
	hi = r.tuning.MaxRows
	if !r.rcsi && r.tuning.EscalationCapRows > 0 {
		hi = min(hi, r.tuning.EscalationCapRows)
	}
	return r.tuning.MinRows, hi
}

// runBatch runs one batch statement under monitoring. It returns stopped=true when
// sustained pressure made the driver stop the batch (its work rolled back), and
// (false, rowsAffected, nil) when the batch committed on its own. It mirrors
// ShrinkRunner.runChunk, but captures rows affected to detect loop exhaustion.
func (r *BatchDMLRunner) runBatch(ctx context.Context, stmt string, caps Capabilities, sink ReactionSink) (stopped bool, rows int64, err error) {
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	done := make(chan error, 1)
	rowsCh := make(chan int64, 1)
	go func() {
		n, e := r.exec.ExecRows(execCtx, stmt)
		rowsCh <- n // sent before done, so a Continue result can read it safely
		done <- e
	}()

	sampleCtx, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	samples := make(chan Sample)
	go pumpSamples(sampleCtx, samples, r.sampler, r.pollIntv, r.logPoll, caps.Ignore)

	action, pressure, serr := supervise(ctx, r.clk, caps, r.blockTO, samples, done)
	if action == Continue {
		if serr != nil {
			return false, 0, serr
		}
		return false, <-rowsCh, nil
	}

	sink(reactionEvent(action, pressure, caps))
	cancelExec()
	select {
	case <-done:
		// stopped on its own after the cancel (the batch rolled back)
	case <-time.After(r.killGr):
		sink(ReactionEvent{Kind: "kill", Detail: "abort did not stop the batch within the grace period"})
		_ = r.exec.Kill(context.Background(), r.exec.SPID())
		<-done
	}
	return true, 0, nil
}

// awaitRelief samples until the pressure that stopped a batch clears, enforcing the
// log-drain timeout. Identical in shape to ShrinkRunner.awaitRelief.
func (r *BatchDMLRunner) awaitRelief(ctx context.Context, ignore IgnoreSource, sink ReactionSink) error {
	sampleCtx, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	samples := make(chan Sample)
	go pumpSamples(sampleCtx, samples, r.sampler, r.pollIntv, r.logPoll, ignore)
	return waitForRelief(ctx, r.clk, r.logDrain, samples, sink)
}

func (r *BatchDMLRunner) emitProgress(op ddl.BatchDML, rowsDone, estRows int64, batchRows int, rate float64) {
	if r.progress == nil {
		return
	}
	r.progress(BatchDMLProgress{
		Schema: op.Schema, Table: op.Table, Verb: op.Verb,
		RowsDone: rowsDone, EstRows: estRows, BatchRows: batchRows, RowsPerSec: rate,
	})
}

// perSec is the throughput of a batch: rows affected divided by its elapsed time.
func perSec(rows int64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(rows) / elapsed.Seconds()
}
