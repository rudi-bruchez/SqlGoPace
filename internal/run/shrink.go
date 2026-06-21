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

// ShrinkReader is the narrow set of runtime reads the shrink driver needs. It is a
// package-local interface (like sampleProbe) so tests can supply a fake; the
// production implementation is *mssql.Conn.
type ShrinkReader interface {
	FileSpace(ctx context.Context, fileType string) ([]mssql.FileSpace, error)
	FileSizeMB(ctx context.Context, file string) (int, error)
	LogReuse(ctx context.Context) (recoveryModel, reuseWaitDesc string, err error)
	ActiveLogFloorMB(ctx context.Context) (int, error)
	SessionWaits(ctx context.Context, spid int) ([]mssql.SessionWait, error)
}

var _ ShrinkReader = (*mssql.Conn)(nil)

// errLogReuseTimeout signals the bounded wait for a scheduled log backup to free
// the log expired; the driver turns it into a clean abort (work preserved).
var errLogReuseTimeout = errors.New("log reuse wait timed out")

// ShrinkProgress is the deterministic progress of one file shrink, fed to the TUI
// in place of the fluctuating percent_complete of dm_exec_requests.
type ShrinkProgress struct {
	File      string
	StartMB   int
	CurrentMB int
	FinalMB   int
}

// Percent returns the fraction of the planned reduction achieved, in [0,1].
func (p ShrinkProgress) Percent() float64 {
	total := p.StartMB - p.FinalMB
	if total <= 0 {
		return 1
	}
	done := min(max(p.StartMB-p.CurrentMB, 0), total)
	return float64(done) / float64(total)
}

// ShrinkResult is the outcome of shrinking one file, for the run report.
type ShrinkResult struct {
	File      string
	Type      string // "data" | "log"
	InitialMB int
	FinalMB   int
	TargetMB  int
	Chunks    int
	NoOp      bool
	Reason    string // why a shrink was skipped or stopped early; empty on full success
}

// ShrinkRunnerConfig configures a ShrinkRunner. The cross-cutting reaction timings
// (blocking/log-drain/kill) are the same MonitoringConfig values the rest of the
// engine uses; the shrink-specific tuning lives in Tuning.
type ShrinkRunnerConfig struct {
	Tuning          ShrinkTuning
	PollInterval    time.Duration
	LogPollInterval time.Duration
	BlockingTimeout time.Duration
	LogDrainTimeout time.Duration
	KillGrace       time.Duration
}

// ShrinkRunner drives DBCC SHRINKFILE: it estimates and gates, runs a free
// TRUNCATEONLY pass, then a calibrated chunk loop under monitoring, reacting with
// the least destructive mechanism (free pause between chunks, no-progress backoff,
// clean stop). All decision logic is in pure functions (shrink_calc.go); the I/O is
// injected so the loop is unit-testable without a database. It reuses Executor so a
// stopped chunk's KILL fallback comes from the monitoring pool, never the execution
// connection (design §8.5).
type ShrinkRunner struct {
	exec     Executor
	reader   ShrinkReader
	sampler  Sampler
	clk      Clock
	tuning   ShrinkTuning
	pollIntv time.Duration
	logPoll  time.Duration
	blockTO  time.Duration
	logDrain time.Duration
	killGr   time.Duration

	progress func(ShrinkProgress)
	wait     func(ctx context.Context, d time.Duration) error
}

// ShrinkOption customizes a ShrinkRunner.
type ShrinkOption func(*ShrinkRunner)

// WithShrinkProgress sets the progress callback (fed to the TUI by the engine).
func WithShrinkProgress(f func(ShrinkProgress)) ShrinkOption {
	return func(r *ShrinkRunner) { r.progress = f }
}

// NewShrinkRunner builds a ShrinkRunner. A zero LogPollInterval falls back to the
// blocking poll interval, mirroring NewMonitoredRunner.
func NewShrinkRunner(exec Executor, reader ShrinkReader, sampler Sampler, clk Clock, cfg ShrinkRunnerConfig, opts ...ShrinkOption) *ShrinkRunner {
	logPoll := cfg.LogPollInterval
	if logPoll <= 0 {
		logPoll = cfg.PollInterval
	}
	r := &ShrinkRunner{
		exec:     exec,
		reader:   reader,
		sampler:  sampler,
		clk:      clk,
		tuning:   cfg.Tuning,
		pollIntv: cfg.PollInterval,
		logPoll:  logPoll,
		blockTO:  cfg.BlockingTimeout,
		logDrain: cfg.LogDrainTimeout,
		killGr:   cfg.KillGrace,
		wait:     sleep,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run shrinks the file(s) the operation targets, sequentially. files:all expands to
// every file of the operation's type (never two of a filegroup in parallel — the
// sequential loop guarantees it). It returns one ShrinkResult per file.
func (r *ShrinkRunner) Run(ctx context.Context, op ddl.Shrink, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) ([]ShrinkResult, error) {
	files, err := r.resolveFiles(ctx, op)
	if err != nil {
		return nil, err
	}
	isLog := strings.EqualFold(strings.TrimSpace(op.Type), "log")

	results := make([]ShrinkResult, 0, len(files))
	for _, f := range files {
		var (
			result ShrinkResult
			ferr   error
		)
		if isLog {
			result, ferr = r.shrinkLog(ctx, op, res, f, sink)
		} else {
			result, ferr = r.shrinkData(ctx, op, res, ignore, f, sink)
		}
		if ferr != nil {
			return results, ferr
		}
		results = append(results, result)
	}
	return results, nil
}

// resolveFiles returns the target files: one named file or, for files:all, every
// file of the operation's type, in file_id order.
func (r *ShrinkRunner) resolveFiles(ctx context.Context, op ddl.Shrink) ([]mssql.FileSpace, error) {
	fileType := mssql.FileTypeRows
	if strings.EqualFold(strings.TrimSpace(op.Type), "log") {
		fileType = mssql.FileTypeLog
	}
	all, err := r.reader.FileSpace(ctx, fileType)
	if err != nil {
		return nil, fmt.Errorf("shrink: read file space: %w", err)
	}
	if strings.EqualFold(op.FilesOrAll(), "all") {
		if len(all) == 0 {
			return nil, fmt.Errorf("shrink: no %s files found", fileType)
		}
		return all, nil
	}
	for _, f := range all {
		if strings.EqualFold(f.Name, op.Files) {
			return []mssql.FileSpace{f}, nil
		}
	}
	return nil, fmt.Errorf("shrink: file %q not found among %s files", op.Files, fileType)
}

// shrinkData runs the data-file algorithm (design §7.1): no-op gating, a free
// TRUNCATEONLY pass, then the chunk loop.
func (r *ShrinkRunner) shrinkData(ctx context.Context, op ddl.Shrink, res ddl.ResolvedOptions, ignore IgnoreSource, f mssql.FileSpace, sink ReactionSink) (ShrinkResult, error) {
	spec, err := ddl.ParseTargetFreeSpace(op.TargetFreeSpace)
	if err != nil {
		return ShrinkResult{}, err // already validated at parse time; defensive
	}
	final := ddl.FinalTargetMB(f.UsedMB, spec)
	result := ShrinkResult{File: f.Name, Type: "data", InitialMB: f.SizeMB, TargetMB: final, FinalMB: f.SizeMB}

	// No-op: nothing free to reclaim, or the target is not below the current size.
	if f.FreeMB <= 0 || final >= f.SizeMB {
		result.NoOp = true
		result.Reason = "nothing to reclaim"
		return result, nil
	}

	// Phase A — TRUNCATEONLY: free and instant, no page movement, no fragmentation.
	if err := r.exec.ExecDDL(ctx, ddl.ShrinkTruncateOnlySQL(f.Name)); err != nil {
		return result, fmt.Errorf("shrink %q: truncateonly: %w", f.Name, err)
	}
	size, err := r.reader.FileSizeMB(ctx, f.Name)
	if err != nil {
		return result, err
	}
	result.FinalMB = size
	if size <= final {
		return result, nil // truncate-only was enough
	}

	// Phase B — chunked page-moving shrink.
	start := size
	step := InitialStepMB(size-final, r.tuning)
	current := size
	noProgress := 0
	backoff := r.tuning.NoProgressBackoff
	var stallWaited time.Duration

	for current > final {
		next := NextTargetMB(current, step, final)

		before, _ := r.reader.SessionWaits(ctx, r.exec.SPID())
		t0 := r.clk.Now()
		stopped, err := r.runChunk(ctx, f.Name, next, res, ignore, sink)
		if err != nil {
			return result, err
		}
		elapsed := r.clk.Since(t0)
		after, _ := r.reader.SessionWaits(ctx, r.exec.SPID())

		newSize, err := r.reader.FileSizeMB(ctx, f.Name)
		if err != nil {
			return result, err
		}

		// A chunk stopped under pressure: wait for relief before trying again. A
		// log-drain timeout is a clean stop (work preserved); a canceled context or
		// any other error is propagated, not swallowed as success.
		if stopped {
			if err := r.awaitRelief(ctx, ignore, sink); err != nil {
				if errors.Is(err, ErrLogDrainTimeout) {
					result.FinalMB = current
					result.Reason = "stopped: log did not drain before timeout (work preserved)"
					return result, nil
				}
				return result, err
			}
		}

		if newSize >= current {
			// No gain: WALP timeout (49516) or data pinned at the file end, or we are
			// blocked by another session (self-wait, §8.2). Wait and retry, stopping
			// cleanly at whichever bound trips first — count or total wait time.
			noProgress++
			if noProgress >= r.tuning.MaxNoProgress || stallWaited >= r.tuning.SelfWaitTimeout {
				result.FinalMB = current
				result.Reason = "no further progress (work preserved)"
				return result, nil
			}
			sink(ReactionEvent{Kind: "pause", Detail: fmt.Sprintf("shrink %q made no progress; backing off %s", f.Name, backoff)})
			if err := r.wait(ctx, backoff); err != nil {
				return result, err
			}
			stallWaited += backoff
			backoff = nextBackoff(backoff, r.tuning.NoProgressBackoffMax)
			continue
		}

		// Progress: adjust the step from this chunk's cost and advance.
		noProgress = 0
		backoff = r.tuning.NoProgressBackoff
		stallWaited = 0
		step = AdjustStepMB(step, elapsed, waitDeltas(before, after), r.tuning)
		current = newSize
		result.Chunks++
		result.FinalMB = current
		r.emitProgress(f.Name, start, current, final)
	}
	return result, nil
}

// shrinkLog runs the log-file algorithm (design §5.2): gate on the recovery model,
// then a single DBCC SHRINKFILE (no chunking — the log is truncated to a VLF
// boundary). SqlGoPace never issues BACKUP LOG; in FULL/BULK_LOGGED it waits, with
// a bound, for a scheduled backup to free the log.
func (r *ShrinkRunner) shrinkLog(ctx context.Context, op ddl.Shrink, res ddl.ResolvedOptions, f mssql.FileSpace, sink ReactionSink) (ShrinkResult, error) {
	spec, err := ddl.ParseTargetFreeSpace(op.TargetFreeSpace)
	if err != nil {
		return ShrinkResult{}, err
	}
	final := ddl.FinalTargetMB(f.UsedMB, spec)
	if floor, err := r.reader.ActiveLogFloorMB(ctx); err != nil {
		return ShrinkResult{}, err
	} else if final < floor {
		final = floor // cannot truncate below the active VLFs
	}
	result := ShrinkResult{File: f.Name, Type: "log", InitialMB: f.SizeMB, TargetMB: final, FinalMB: f.SizeMB}

	model, reuse, err := r.reader.LogReuse(ctx)
	if err != nil {
		return result, err
	}
	switch {
	case strings.EqualFold(model, "SIMPLE"):
		// A CHECKPOINT is harmless and frees the VLFs in SIMPLE recovery.
		if err := r.exec.ExecDDL(ctx, "CHECKPOINT;"); err != nil {
			return result, fmt.Errorf("shrink %q: checkpoint: %w", f.Name, err)
		}
	default: // FULL / BULK_LOGGED
		if !strings.EqualFold(reuse, "NOTHING") {
			if err := r.awaitLogReuse(ctx, sink); err != nil {
				if errors.Is(err, errLogReuseTimeout) {
					result.Reason = "log not truncatable before timeout (no BACKUP LOG issued)"
					return result, nil // clean abort, work preserved
				}
				return result, err
			}
		}
	}

	if err := r.exec.ExecDDL(ctx, ddl.ShrinkChunkSQL(f.Name, final, res)); err != nil {
		return result, fmt.Errorf("shrink %q: %w", f.Name, err)
	}
	size, err := r.reader.FileSizeMB(ctx, f.Name)
	if err != nil {
		return result, err
	}
	result.FinalMB = size
	return result, nil
}

// awaitLogReuse waits, bounded by LogReuseWaitTimeout, for the log to become
// truncatable (log_reuse_wait_desc → NOTHING), e.g. once a scheduled BACKUP LOG
// runs. It re-reads the reason on the log poll cadence and emits a pause per cycle.
// It never issues BACKUP LOG itself. It returns errLogReuseTimeout on expiry.
func (r *ShrinkRunner) awaitLogReuse(ctx context.Context, sink ReactionSink) error {
	deadline := r.clk.Now().Add(r.tuning.LogReuseWaitTimeout)
	for {
		_, reuse, err := r.reader.LogReuse(ctx)
		if err != nil {
			return err
		}
		if strings.EqualFold(reuse, "NOTHING") {
			sink(ReactionEvent{Kind: "resume", Detail: "log reuse cleared (NOTHING)"})
			return nil
		}
		if !r.clk.Now().Before(deadline) {
			sink(ReactionEvent{Kind: "abort", Detail: fmt.Sprintf("log reuse wait timed out (reuse_wait=%s); no BACKUP LOG issued", reuse)})
			return errLogReuseTimeout
		}
		sink(ReactionEvent{Kind: "pause", Detail: fmt.Sprintf("waiting for the log to free (reuse_wait=%s)", reuse)})
		if err := r.wait(ctx, r.logPoll); err != nil {
			return err
		}
	}
}

// runChunk runs one page-moving shrink statement under monitoring. It returns
// stopped=true when sustained pressure made the driver stop the chunk (context
// cancel, KILL via the pool as a fallback) with its committed work preserved;
// stopped=false when the statement finished on its own (err carries a real DDL
// failure, if any). It mirrors MonitoredRunner.runStatement.
func (r *ShrinkRunner) runChunk(ctx context.Context, file string, targetMB int, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) (stopped bool, err error) {
	stmt := ddl.ShrinkChunkSQL(file, targetMB, res)

	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	done := make(chan error, 1)
	go func() { done <- r.exec.ExecDDL(execCtx, stmt) }()

	sampleCtx, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	samples := make(chan Sample)
	go pumpSamples(sampleCtx, samples, r.sampler, r.pollIntv, r.logPoll, ignore)

	// A shrink chunk is cancel-safe: each internal ~32-page batch commits, so a stop
	// preserves work and is re-entrant. It is never resumable in the ALTER sense.
	caps := Capabilities{CancelSafe: true, MaxBlock: blockCap(res.MaxBlockMinutes)}
	action, pressure, serr := supervise(ctx, r.clk, caps, r.blockTO, samples, done)
	if action == Continue {
		return false, serr
	}

	sink(ReactionEvent{Kind: "pause", Detail: "shrink chunk stopped under pressure; committed work preserved — " + pressure.Detail()})
	cancelExec()
	select {
	case <-done:
		// stopped on its own after the cancel
	case <-time.After(r.killGr):
		sink(ReactionEvent{Kind: "kill", Detail: "abort did not stop the chunk within the grace period"})
		_ = r.exec.Kill(context.Background(), r.exec.SPID())
		<-done
	}
	return true, nil
}

// awaitRelief samples until the pressure that stopped a chunk clears, enforcing the
// log-drain timeout. Identical in shape to MonitoredRunner.awaitRelief.
func (r *ShrinkRunner) awaitRelief(ctx context.Context, ignore IgnoreSource, sink ReactionSink) error {
	sampleCtx, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	samples := make(chan Sample)
	go pumpSamples(sampleCtx, samples, r.sampler, r.pollIntv, r.logPoll, ignore)
	return waitForRelief(ctx, r.clk, r.logDrain, samples, sink)
}

func (r *ShrinkRunner) emitProgress(file string, start, current, final int) {
	if r.progress != nil {
		r.progress(ShrinkProgress{File: file, StartMB: start, CurrentMB: current, FinalMB: final})
	}
}

// waitDeltas extracts the stepsize-gating wait deltas from two session-wait
// snapshots: average WRITELOG and PAGEIOLATCH_EX latency per waiting task. The
// blocking dimension is handled by the pressure-stop path, not the step adjust, so
// BlockingSeconds is left zero here.
func waitDeltas(before, after []mssql.SessionWait) WaitDeltas {
	var d WaitDeltas
	for _, w := range mssql.DiffWaits(before, after) {
		if w.WaitingTasksCount <= 0 {
			continue
		}
		avg := float64(w.WaitTimeMS) / float64(w.WaitingTasksCount)
		switch {
		case w.WaitType == "WRITELOG":
			d.WriteLogAvgMs = avg
		case strings.HasPrefix(w.WaitType, "PAGEIOLATCH_EX"):
			d.PageIOLatchExAvgMs = avg
		}
	}
	return d
}

// nextBackoff doubles d, capped at max.
func nextBackoff(d, max time.Duration) time.Duration {
	d *= 2
	if max > 0 && d > max {
		return max
	}
	return d
}

// sleep is the production wait: a context-cancellable timer. Tests inject a fake
// that advances the manual clock instead.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
