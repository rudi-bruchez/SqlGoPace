package run

import (
	"context"
	"errors"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// BatchExecutor is an Executor that can also report rows affected, which the
// batch-DML driver needs to detect when a predicate loop is exhausted. *mssql.Conn
// satisfies it; the rest of the engine uses the narrower Executor.
type BatchExecutor interface {
	Executor
	ExecRows(ctx context.Context, sql string) (int64, error)
}

// BatchDMLReader is the narrow set of runtime reads the batch-DML driver needs.
// The production implementation is *mssql.Conn; tests supply a fake.
type BatchDMLReader interface {
	TableRowEstimate(ctx context.Context, schema, table string) (int64, error)
	SessionWaits(ctx context.Context, spid int) ([]mssql.SessionWait, error)
}

var _ BatchDMLReader = (*mssql.Conn)(nil)

// BatchDMLProgress is the progress of one batch-DML operation, fed to the TUI.
type BatchDMLProgress struct {
	Schema, Table string
	Verb          string
	RowsDone      int64
	EstRows       int64
	BatchRows     int
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
}

// BatchDMLOption customizes a BatchDMLRunner.
type BatchDMLOption func(*BatchDMLRunner)

// WithBatchDMLProgress sets the progress callback (fed to the TUI by the engine).
func WithBatchDMLProgress(f func(BatchDMLProgress)) BatchDMLOption {
	return func(r *BatchDMLRunner) { r.progress = f }
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

// Run executes the batch-DML operation: it loops the predicate statement, each
// batch committed individually, until it affects no rows (the predicate is
// exhausted). It returns a single BatchDMLResult.
func (r *BatchDMLRunner) Run(ctx context.Context, op ddl.BatchDML, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) (BatchDMLResult, error) {
	result := BatchDMLResult{Schema: op.Schema, Table: op.Table, Verb: op.Verb}

	// Best-effort estimate: 0 when unavailable just means the smallest initial tier.
	est, _ := r.reader.TableRowEstimate(ctx, op.Schema, op.Table)

	lo, hi := r.batchBounds()
	size := InitialBatchRows(est, r.tuning)
	if op.Batch.InitialRows != nil {
		size = *op.Batch.InitialRows
	}
	size = clampRows(size, lo, hi)

	// A batch is cancel-safe: a single UPDATE/DELETE TOP commits atomically, so a stop
	// rolls it back cleanly and the already-committed batches survive. ignore_blocking
	// and max_block_minutes flow through exactly as for monitored DDL.
	caps := Capabilities{
		CancelSafe:     true,
		Ignore:         ignore,
		IgnoreBlocking: res.IgnoreBlocking,
		MaxBlock:       blockCap(res.MaxBlockMinutes),
	}

	var stallWaited time.Duration
	for {
		stmt := ddl.BatchDMLChunkSQL(op, size, res)
		before, _ := r.reader.SessionWaits(ctx, r.exec.SPID())
		t0 := r.clk.Now()
		stopped, rows, err := r.runBatch(ctx, stmt, caps, sink)
		if err != nil {
			return result, err
		}
		elapsed := r.clk.Since(t0)
		after, _ := r.reader.SessionWaits(ctx, r.exec.SPID())

		// A batch stopped under pressure: its work rolled back (nothing committed).
		// Wait for relief, then retry the same batch. A log-drain timeout is a clean
		// stop; bounded total wait avoids spinning if we keep getting blocked.
		if stopped {
			t1 := r.clk.Now()
			if err := r.awaitRelief(ctx, ignore, sink); err != nil {
				if errors.Is(err, ErrLogDrainTimeout) {
					result.Reason = "stopped: log did not drain before timeout (committed batches preserved)"
					return result, nil
				}
				return result, err
			}
			stallWaited += r.clk.Since(t1)
			if r.tuning.SelfWaitTimeout > 0 && stallWaited >= r.tuning.SelfWaitTimeout {
				result.Reason = "stopped: blocked longer than the self-wait timeout (committed batches preserved)"
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
		r.emitProgress(op, result.Rows, est)

		size = AdjustBatchRows(size, elapsed, waitDeltas(before, after), r.tuning.TargetBatch, lo, hi)
	}
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

func (r *BatchDMLRunner) emitProgress(op ddl.BatchDML, rowsDone, estRows int64) {
	if r.progress != nil {
		r.progress(BatchDMLProgress{
			Schema: op.Schema, Table: op.Table, Verb: op.Verb,
			RowsDone: rowsDone, EstRows: estRows,
		})
	}
}
