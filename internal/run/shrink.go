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
// in place of the fluctuating percent_complete of dm_exec_requests. StepMB is the
// (adaptively adjusted) chunk size going into the next chunk, so the operator can see
// the increment the shrink is moving by; Type is "data" or "log". Chunks/ChunksRemaining/
// ETASeconds project the work left from what the completed chunks have achieved.
type ShrinkProgress struct {
	File            string
	Type            string
	StartMB         int
	CurrentMB       int
	FinalMB         int
	StepMB          int
	Chunks          int // page-moving chunks completed so far
	ChunksRemaining int // estimated chunks left (from the average chunk so far)
	ETASeconds      int // estimated seconds left (from the achieved reduction rate)
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
	stop     func() bool // graceful stop (cancellable): finish the current chunk, then stop
}

// ShrinkOption customizes a ShrinkRunner.
type ShrinkOption func(*ShrinkRunner)

// WithShrinkProgress sets the progress callback (fed to the TUI by the engine).
func WithShrinkProgress(f func(ShrinkProgress)) ShrinkOption {
	return func(r *ShrinkRunner) { r.progress = f }
}

// WithShrinkStop wires the engine's graceful-stop predicate (the DrainFlag's Draining
// method): once it reports true, the driver finishes the current chunk (already committed)
// and stops, returning ErrStopped so the run is left in processing and the next run resumes
// the shrink from the current size. A Cancel before the next chunk resumes normally.
func WithShrinkStop(stop func() bool) ShrinkOption {
	return func(r *ShrinkRunner) { r.stop = stop }
}

// defaultShrinkPollInterval floors the poll cadence so time.NewTicker (used by
// pumpSamples and runTruncateOnly) never gets a non-positive interval and panics.
// config.Validate rejects a zero blocking_poll in the production path; this defends any
// other caller (tests, future wiring) that constructs a runner directly.
const defaultShrinkPollInterval = time.Second

// NewShrinkRunner builds a ShrinkRunner. A zero PollInterval floors to a default; a zero
// LogPollInterval falls back to the (floored) blocking poll interval, mirroring NewMonitoredRunner.
func NewShrinkRunner(exec Executor, reader ShrinkReader, sampler Sampler, clk Clock, cfg ShrinkRunnerConfig, opts ...ShrinkOption) *ShrinkRunner {
	pollIntv := cfg.PollInterval
	if pollIntv <= 0 {
		pollIntv = defaultShrinkPollInterval
	}
	logPoll := cfg.LogPollInterval
	if logPoll <= 0 {
		logPoll = pollIntv
	}
	r := &ShrinkRunner{
		exec:     exec,
		reader:   reader,
		sampler:  sampler,
		clk:      clk,
		tuning:   cfg.Tuning,
		pollIntv: pollIntv,
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
		// Graceful stop: a running file's chunk loop stops itself between chunks (below);
		// here we stop before starting the next file, leaving the rest for the next run.
		if stopRequested(r.stop) {
			return results, ErrStopped
		}
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
			// On a graceful stop the partial file result is still worth recording.
			if errors.Is(ferr, ErrStopped) {
				results = append(results, result)
			}
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

	// Surface the target immediately — before the possibly-long TRUNCATEONLY and the
	// chunk loop — so the console shows current size, target and step from the start,
	// not only after the first chunk that happens to gain space (a shrink blocked or
	// making no progress would otherwise show no shrink line at all).
	r.emitProgress(ShrinkProgress{
		File: f.Name, Type: "data", StartMB: f.SizeMB, CurrentMB: f.SizeMB,
		FinalMB: final, StepMB: InitialStepMB(f.SizeMB-final, r.tuning),
	})

	// Phase A — TRUNCATEONLY: releases trailing free space with no page movement, no
	// fragmentation. On a large file this can run for a while, so it is interruptible: a
	// graceful stop cancels it and the space it already released is preserved (a re-run
	// resumes from the smaller size), so it stops cleanly rather than failing.
	stopped, err := r.runTruncateOnly(ctx, f.Name, sink)
	if err != nil {
		return result, fmt.Errorf("shrink %q: truncateonly: %w", f.Name, err)
	}
	size, err := r.reader.FileSizeMB(ctx, f.Name)
	if err != nil {
		return result, err
	}
	result.FinalMB = size
	if stopped {
		result.Reason = "stopped: graceful stop during TRUNCATEONLY (freed space preserved)"
		return result, ErrStopped
	}
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
	shrinkStart := r.clk.Now() // anchors the ETA from the reduction rate achieved so far

	for current > final {
		// Graceful stop: each chunk commits, so stopping between chunks preserves work;
		// the next run resumes the shrink from the current (already reduced) size.
		if stopRequested(r.stop) {
			result.FinalMB = current
			result.Reason = "stopped: graceful stop (work preserved)"
			return result, ErrStopped
		}
		// Emit progress every iteration, not only after a chunk gains space, so a shrink
		// that is blocked or stalling still shows its live current size, chunk count, step
		// and ETA in the console instead of appearing frozen.
		chunksLeft, eta := estimateShrink(start, current, final, result.Chunks, r.clk.Since(shrinkStart))
		r.emitProgress(ShrinkProgress{
			File: f.Name, Type: "data", StartMB: start, CurrentMB: current, FinalMB: final,
			StepMB: step, Chunks: result.Chunks, ChunksRemaining: chunksLeft, ETASeconds: eta,
		})
		next := NextTargetMB(current, step, final)

		before, _ := r.reader.SessionWaits(ctx, r.exec.SPID())
		t0 := r.clk.Now()
		stopped, err := r.runChunk(ctx, f.Name, next, res, ignore, sink)
		if err != nil {
			// Msg 5240: the file can't be adjusted to this target right now (pages pinned at
			// the file end, or concurrent allocation). Not a failure — try a smaller move and
			// treat it as no-progress, so a persistent stall stops cleanly with work preserved.
			if !mssql.IsFileAllocationError(err) {
				return result, err
			}
			step = clampStep(step/2, r.tuning.MinStepMB, r.tuning.MaxStepMB)
			if stop, werr := r.stall(ctx, f.Name, &noProgress, &backoff, &stallWaited, sink); werr != nil {
				return result, werr
			} else if stop {
				result.FinalMB = current
				result.Reason = "shrink could not adjust the file allocation (work preserved)"
				return result, nil
			}
			continue
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
			if stop, werr := r.stall(ctx, f.Name, &noProgress, &backoff, &stallWaited, sink); werr != nil {
				return result, werr
			} else if stop {
				result.FinalMB = current
				result.Reason = "no further progress (work preserved)"
				return result, nil
			}
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
		chunksLeft, eta = estimateShrink(start, current, final, result.Chunks, r.clk.Since(shrinkStart))
		r.emitProgress(ShrinkProgress{
			File: f.Name, Type: "data", StartMB: start, CurrentMB: current, FinalMB: final,
			StepMB: step, Chunks: result.Chunks, ChunksRemaining: chunksLeft, ETASeconds: eta,
		})
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
	r.cancelAndAwait(cancelExec, done, sink, "abort did not stop the chunk within the grace period")
	return true, nil
}

// cancelAndAwait cancels the running statement and waits for it to stop, falling back to a
// pool KILL (never the execution connection) if it does not stop within the grace period.
// done must be the channel carrying the statement's ExecDDL result. Shared by runChunk and
// runTruncateOnly.
func (r *ShrinkRunner) cancelAndAwait(cancelExec context.CancelFunc, done <-chan error, sink ReactionSink, killReason string) {
	cancelExec()
	select {
	case <-done:
	case <-time.After(r.killGr):
		sink(ReactionEvent{Kind: "kill", Detail: killReason})
		_ = r.exec.Kill(context.Background(), r.exec.SPID())
		<-done
	}
}

// runTruncateOnly runs the free TRUNCATEONLY pass while watching for a graceful stop.
// TRUNCATEONLY is a single statement, not a chunk loop, so a drain cannot be honored at
// a chunk boundary; instead this polls the stop flag on the monitoring cadence and
// cancels the running statement when a stop is requested. The trailing space it has
// already released is preserved on the server (a re-run resumes from the smaller size),
// so a stopped TRUNCATEONLY is a clean, re-entrant interruption. It returns stopped=true
// when a drain canceled it (the caller turns that into ErrStopped) and the raw exec error
// otherwise (nil on completion, ctx.Err on a hard cancel). It mirrors runChunk's cancel →
// KILL-grace fallback, drawing the KILL from the pool via Executor.
func (r *ShrinkRunner) runTruncateOnly(ctx context.Context, file string, sink ReactionSink) (stopped bool, err error) {
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	done := make(chan error, 1)
	go func() { done <- r.exec.ExecDDL(execCtx, ddl.ShrinkTruncateOnlySQL(file)) }()

	ticker := time.NewTicker(r.pollIntv)
	defer ticker.Stop()
	for {
		// Prefer a finished statement over a concurrent stop tick, so a TRUNCATEONLY that
		// completes on its own is never mis-reported as stopped.
		select {
		case err := <-done:
			return false, err
		default:
		}
		select {
		case err := <-done:
			return false, err
		case <-ticker.C:
			if !stopRequested(r.stop) {
				continue
			}
			// The statement may have finished between this tick and the stop check; prefer
			// the real result over reporting a completed TRUNCATEONLY as stopped.
			select {
			case err := <-done:
				return false, err
			default:
			}
			sink(ReactionEvent{Kind: "pause", Detail: fmt.Sprintf("shrink %q TRUNCATEONLY stopped on graceful stop; freed space preserved", file)})
			r.cancelAndAwait(cancelExec, done, sink, "TRUNCATEONLY did not stop within the grace period")
			return true, nil
		}
	}
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

func (r *ShrinkRunner) emitProgress(p ShrinkProgress) {
	if r.progress != nil {
		r.progress(p)
	}
}

// stall records a no-progress chunk (a real no-gain, or a Msg 5240 that could not adjust
// the file) and decides whether to give up. It returns stop=true once the no-progress
// count or the total self-wait budget trips, so the caller ends the shrink cleanly with
// the reduction so far preserved; otherwise it backs off (doubling each time) and returns
// stop=false so the caller retries. The pointers are the loop's running counters.
func (r *ShrinkRunner) stall(ctx context.Context, file string, noProgress *int, backoff, stallWaited *time.Duration, sink ReactionSink) (stop bool, err error) {
	*noProgress++
	if *noProgress >= r.tuning.MaxNoProgress || *stallWaited >= r.tuning.SelfWaitTimeout {
		return true, nil
	}
	sink(ReactionEvent{Kind: "pause", Detail: fmt.Sprintf("shrink %q made no progress; backing off %s", file, *backoff)})
	if werr := r.wait(ctx, *backoff); werr != nil {
		return false, werr
	}
	*stallWaited += *backoff
	*backoff = nextBackoff(*backoff, r.tuning.NoProgressBackoffMax)
	return false, nil
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
