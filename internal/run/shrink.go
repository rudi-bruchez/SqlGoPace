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
	// Progress reads the running statement's percent_complete from dm_exec_requests, which
	// SQL Server populates for DBCC SHRINKFILE — the server's own view of the current chunk.
	Progress(ctx context.Context, spid int) (mssql.Progress, bool, error)
	// FindTailObject names the user object owning the file's last allocated page (the tail the
	// shrink cannot relocate past). SQL 2019+ only — the driver gates on SQLMajorVersion.
	FindTailObject(ctx context.Context, fileID, maxPagesBack int) (mssql.TailObject, bool, error)
	// ActiveSessions is the running-request snapshot used to identify a session blocking the
	// shrink (via mssql.FindSelfBlock) and classify it as transient maintenance.
	ActiveSessions(ctx context.Context) ([]mssql.Session, error)
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
	File              string
	Type              string
	StartMB           int
	CurrentMB         int
	FinalMB           int
	StepMB            int
	Chunks            int // page-moving chunks completed so far
	ChunksRemaining   int // estimated chunks left (from the average chunk so far)
	ETASeconds        int // estimated seconds left over the full elapsed time (with blocking)
	ETASecondsNoBlock int // estimated seconds left over productive time only (without blocking)
	AvgChunkSeconds   int // observed wall-clock cadence: elapsed per completed chunk (waits included)
	BlockedSeconds    int // cumulative seconds spent blocked/stalled (unproductive)

	// PercentComplete is SQL Server's own percent_complete for the running chunk statement
	// (dm_exec_requests), sampled live while a chunk executes; 0 when unavailable or between
	// chunks. It can be nonlinear (flat for long stretches) — a cross-check, not the ETA basis.
	PercentComplete float64

	// ChunkTargetMB and Statement expose the exact work in flight so the console can show it
	// verbatim: ChunkTargetMB is the target size the next chunk shrinks the file to (0 during
	// the TRUNCATEONLY phase), and Statement is the literal T-SQL about to run — the
	// TRUNCATEONLY pass first, then each DBCC SHRINKFILE (file, target) chunk.
	ChunkTargetMB int
	Statement     string
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

// TempdbProfile carries the tempdb-shrink-specific knobs into the shared chunk loop.
// Nil for a normal (non-tempdb) shrink. When FlushCaches is set, stall escalates a
// persistent no-progress run (any shrink chunk error, or a real no-gain) into one targeted
// temp-object cache flush, guarded by flushed so it never runs more than once per run.
type TempdbProfile struct {
	FlushCaches bool
	flushed     *bool // once-per-run guard, shared across a RunTempdb's files
}

// tailProbe carries the per-operation tail-object walk state into the shared chunk loop.
// Nil on the tempdb path (tempdb shrinks never walk). proactive runs a walk at loop entry;
// warned is the once-per-operation <2019 warning guard; finding holds the proactive walk's
// result, recorded into the sidecar only if the shrink then misses target (see shrinkData).
type tailProbe struct {
	proactive   bool
	warned      *bool
	maintWarned *bool // separate once-per-op guard from warned, so the maintenance-block and 2019+ tail warnings never suppress each other
	finding     *TailFinding
	maintBlock  *MaintBlock // set when a concurrent maintenance op is found blocking the shrink
}

// MaintBlock identifies a concurrent maintenance operation blocking the shrink, captured
// from an ActiveSessions self-block snapshot. Its presence downgrades a give-up tail record
// to confirmed_by=transient_maintenance (see shrinkData / captureGiveUpTail).
type MaintBlock struct {
	SPID    int
	Command string
	WaitMS  int64
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
	// SQLMajorVersion gates the tail-object walk: it needs sys.dm_db_page_info, SQL 2019+ only.
	SQLMajorVersion int
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
	major    int // SQL Server major version; gates the tail-object walk (2019+ only)

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
		major:    cfg.SQLMajorVersion,
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

	// One warning guard per operation: below SQL 2019 the driver warns about the skipped
	// tail-object walk exactly once, however many files this operation shrinks.
	warned := new(bool)
	tp := &tailProbe{proactive: op.IdentifyTailObject, warned: warned, maintWarned: new(bool)}

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
			result, ferr = r.shrinkLog(ctx, op, res, ignore, f, sink)
		} else {
			result, ferr = r.shrinkData(ctx, op, res, ignore, f, sink, tp)
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

// RunTempdb shrinks every tempdb data file to a common absolute target, two-phase:
// Phase 0 runs TRUNCATEONLY on all files (free tails returned to the OS first), then
// Phase 1 runs the shared chunkLoop per file. The per-file target is clamped to the
// file's used space. It reuses this runner's exec/reader, which the wiring binds to a
// tempdb-scoped connection. See specs/TEMPDB-SHRINK.md.
func (r *ShrinkRunner) RunTempdb(ctx context.Context, op ddl.ShrinkTempdb, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) ([]ShrinkResult, error) {
	files, err := r.resolveFiles(ctx, ddl.Shrink{Type: "data", Files: "all"})
	if err != nil {
		return nil, err
	}
	flushed := false
	prof := &TempdbProfile{FlushCaches: op.FlushCaches, flushed: &flushed}

	// State the total explicitly (spec §6): targetsizemb is PER FILE, easy to misread.
	sink(ReactionEvent{Kind: "tempdb", Detail: fmt.Sprintf(
		"shrinking %d tempdb data files to %d MB each (total target %d MB)",
		len(files), op.TargetSizeMB, len(files)*op.TargetSizeMB)})

	// Phase 0 — TRUNCATEONLY on all files first.
	for _, f := range files {
		if stopRequested(r.stop) {
			return nil, ErrStopped
		}
		// Same as shrinkData: surface the file and its target before the possibly-long
		// TRUNCATEONLY, so the pass reports the server's percent_complete against it.
		base := ShrinkProgress{
			File: f.Name, Type: "data", StartMB: f.SizeMB, CurrentMB: f.SizeMB,
			FinalMB:   max(op.TargetSizeMB, f.UsedMB), // cannot shrink below used
			Statement: ddl.ShrinkTruncateOnlySQL(f.Name),
		}
		r.emitProgress(base)
		if stopped, terr := r.runTruncateOnly(ctx, f.Name, base, ignore, sink); terr != nil {
			return nil, fmt.Errorf("shrink_tempdb %q: truncateonly: %w", f.Name, terr)
		} else if stopped {
			return nil, ErrStopped
		}
	}

	// Phase 1 — per-file chunk loop.
	results := make([]ShrinkResult, 0, len(files))
	for _, f := range files {
		if stopRequested(r.stop) {
			return results, ErrStopped
		}
		size, ferr := r.reader.FileSizeMB(ctx, f.Name)
		if ferr != nil {
			return results, ferr
		}
		final := op.TargetSizeMB
		if final < f.UsedMB {
			final = f.UsedMB // clamp: cannot shrink below used
		}
		f.SizeMB = size
		if size <= final {
			results = append(results, ShrinkResult{File: f.Name, Type: "data", InitialMB: size, TargetMB: final, FinalMB: size, NoOp: true, Reason: "already at or below target"})
			continue
		}
		res2, cerr := r.chunkLoop(ctx, f, size, final, res, ignore, sink, prof, nil)
		if cerr != nil {
			if errors.Is(cerr, ErrStopped) {
				results = append(results, res2)
			}
			return results, cerr
		}
		results = append(results, res2)
	}
	warnIfUnbalanced(results, sink)
	return results, nil
}

// warnIfUnbalanced emits a warning when the data files did not all end at the same
// whole-MB size: an asymmetric tempdb defeats proportional fill (see spec §6).
func warnIfUnbalanced(results []ShrinkResult, sink ReactionSink) {
	first := -1
	for _, r := range results {
		if first == -1 {
			first = r.FinalMB
		} else if r.FinalMB != first {
			sink(ReactionEvent{Kind: "tempdb", Detail: "Unbalanced tempdb files: data files ended at different sizes; proportional fill will skew — re-run or intervene"})
			return
		}
	}
}

// shrinkData runs the data-file algorithm (design §7.1): no-op gating, a free
// TRUNCATEONLY pass, then the chunk loop.
func (r *ShrinkRunner) shrinkData(ctx context.Context, op ddl.Shrink, res ddl.ResolvedOptions, ignore IgnoreSource, f mssql.FileSpace, sink ReactionSink, tp *tailProbe) (ShrinkResult, error) {
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
	truncProgress := ShrinkProgress{
		File: f.Name, Type: "data", StartMB: f.SizeMB, CurrentMB: f.SizeMB,
		FinalMB: final, StepMB: InitialStepMB(f.SizeMB-final, f.SizeMB, r.tuning),
		Statement: ddl.ShrinkTruncateOnlySQL(f.Name), // the first statement is the free TRUNCATEONLY pass
	}
	r.emitProgress(truncProgress)

	// Phase A — TRUNCATEONLY: releases trailing free space with no page movement, no
	// fragmentation. On a large file this can run for a while, so it is interruptible: a
	// graceful stop cancels it and the space it already released is preserved (a re-run
	// resumes from the smaller size), so it stops cleanly rather than failing.
	stopped, err := r.runTruncateOnly(ctx, f.Name, truncProgress, ignore, sink)
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

	// Phase B — chunked page-moving shrink. The proactive tail-object walk (if requested)
	// runs at chunkLoop's entry, so its look-back cap (tailWalkPages) should reflect the free
	// space actually left after TRUNCATEONLY, not f's pre-truncate snapshot.
	f.FreeMB = size - f.UsedMB
	out, err := r.chunkLoop(ctx, f, size, final, res, ignore, sink, nil, tp)

	// #1: a proactively-identified tail is a *confirmed* blocker only when the shrink missed
	// target — a graceful stop or a log-drain stop (a give-up already recorded it in the loop).
	// On full success (FinalMB <= TargetMB) it was relocated fine, so recording it would leave a
	// false blocker in the sidecar and mislead `plan --confirmed`; drop it.
	if tp != nil && tp.finding != nil && out.FinalMB > out.TargetMB {
		r.markTransient(tp.finding, tp)
		r.emitTail(sink, tp.finding, f.Name, true)
	}
	return out, err
}

// chunkLoop runs the page-moving chunk loop for one already-truncated file, from start
// down to final MB. It is shared by shrinkData (prof == nil, a normal shrink) and
// RunTempdb (prof carries tempdb-specific escalation knobs — see stall).
func (r *ShrinkRunner) chunkLoop(ctx context.Context, f mssql.FileSpace, start, final int, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink, prof *TempdbProfile, tp *tailProbe) (ShrinkResult, error) {
	result := ShrinkResult{File: f.Name, Type: "data", InitialMB: f.SizeMB, TargetMB: final, FinalMB: start}

	// Proactive tail-object walk: run once at loop entry when the operation asked for it
	// (op.IdentifyTailObject), before any chunk executes. It is logged now but only *recorded*
	// as a confirmed blocker if the shrink later misses target (#1, handled in shrinkData) —
	// a tail object a successful shrink relocates was never a blocker. warn=true: the operator
	// opted in, so tell them if the server is too old.
	if tp != nil {
		tp.finding = nil    // per-file reset: tp is shared across a files:all run
		tp.maintBlock = nil // per-file reset: a later file may be blocked by a different op
		if tp.proactive {
			if tf := r.walkTail(ctx, f, sink, tp.warned, true); tf != nil {
				r.emitTail(sink, tf, f.Name, false)
				tp.finding = tf
			}
		}
	}

	maxStep := effectiveMaxStepMB(start, r.tuning) // per-file step ceiling, fixed for this file
	step := InitialStepMB(start-final, start, r.tuning)
	current := start
	noProgress := 0
	backoff := r.tuning.NoProgressBackoff
	var stallWaited time.Duration
	var blocked time.Duration  // cumulative unproductive time (no-gain chunks, stalls, relief waits)
	shrinkStart := r.clk.Now() // anchors the ETA from the reduction rate achieved so far
	var lastMaint *MaintBlock  // most recent in-flight maintenance-block observation from runChunk

	for current > final {
		// Graceful stop: each chunk commits, so stopping between chunks preserves work;
		// the next run resumes the shrink from the current (already reduced) size.
		if stopRequested(r.stop) {
			result.FinalMB = current
			result.Reason = "stopped: graceful stop (work preserved)"
			return result, ErrStopped
		}
		// Emit progress every iteration, not only after a chunk gains space, so a shrink
		// that is blocked or stalling still shows its live current size, chunk count, step,
		// ETA and the exact chunk statement in the console instead of appearing frozen.
		next := NextTargetMB(current, step, final)
		chunksLeft, eta, etaNB, avg := estimateShrink(start, current, final, result.Chunks, r.clk.Since(shrinkStart), blocked)
		prog := ShrinkProgress{
			File: f.Name, Type: "data", StartMB: start, CurrentMB: current, FinalMB: final,
			StepMB: step, Chunks: result.Chunks, ChunksRemaining: chunksLeft, ETASeconds: eta,
			ETASecondsNoBlock: etaNB, AvgChunkSeconds: avg, BlockedSeconds: int(blocked.Seconds()),
			ChunkTargetMB: next, Statement: ddl.ShrinkChunkSQL(f.Name, next, res),
		}
		r.emitProgress(prog)

		before, _ := r.reader.SessionWaits(ctx, r.exec.SPID())
		t0 := r.clk.Now()
		stopped, mb, err := r.runChunk(ctx, f.Name, next, res, ignore, sink, prog, tp != nil)
		if mb != nil {
			lastMaint = mb
		}
		if err != nil {
			// A DBCC SHRINKFILE chunk error almost never means the operation is broken — it
			// means the shrink could not move a page right now (Msg 3140 "could not adjust the
			// space allocation", Msg 845 buffer-latch time-out, a WAIT_AT_LOW_PRIORITY timeout,
			// a transient contention error, …). We decide success by progress, not by matching a
			// specific message number: treat any such error as a no-progress event — a smaller
			// step, then stall's bounded budget (MaxNoProgress / SelfWaitTimeout) stops cleanly
			// with work preserved (or, on the tempdb path, escalates to a cache flush). The last
			// error is carried into the reason so it is never masked. Only our own cancellation
			// is fatal.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return result, err
			}
			step = clampStep(step/2, r.tuning.MinStepMB, maxStep)
			blocked += r.clk.Since(t0) // a failed (no-move) chunk is unproductive time
			stop, werr := r.stall(ctx, f.Name, &noProgress, &backoff, &stallWaited, sink, prof)
			r.applyMaintBlock(sink, f, tp, lastMaint)
			if werr != nil {
				return result, werr
			} else if stop {
				result.FinalMB = current
				result.Reason = giveUpReason(tp, fmt.Sprintf("no further progress: %v (work preserved)", err))
				r.captureGiveUpTail(ctx, f, sink, tp)
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
			r0 := r.clk.Now()
			err := r.awaitRelief(ctx, ignore, sink)
			blocked += r.clk.Since(r0) // waiting for pressure to clear is unproductive time
			if err != nil {
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
			blocked += elapsed // the chunk ran but moved nothing (WALP timeout / blocked)
			s0 := r.clk.Now()
			stop, werr := r.stall(ctx, f.Name, &noProgress, &backoff, &stallWaited, sink, prof)
			blocked += r.clk.Since(s0)
			r.applyMaintBlock(sink, f, tp, lastMaint)
			if werr != nil {
				return result, werr
			} else if stop {
				result.FinalMB = current
				result.Reason = giveUpReason(tp, "no further progress (work preserved)")
				r.captureGiveUpTail(ctx, f, sink, tp)
				return result, nil
			}
			continue
		}

		// Progress: adjust the step from this chunk's cost and advance.
		noProgress = 0
		backoff = r.tuning.NoProgressBackoff
		stallWaited = 0
		step = AdjustStepMB(step, elapsed, waitDeltas(before, after), r.tuning, maxStep)
		current = newSize
		result.Chunks++
		result.FinalMB = current
		nextT := NextTargetMB(current, step, final)
		chunksLeft, eta, etaNB, avg = estimateShrink(start, current, final, result.Chunks, r.clk.Since(shrinkStart), blocked)
		r.emitProgress(ShrinkProgress{
			File: f.Name, Type: "data", StartMB: start, CurrentMB: current, FinalMB: final,
			StepMB: step, Chunks: result.Chunks, ChunksRemaining: chunksLeft, ETASeconds: eta,
			ETASecondsNoBlock: etaNB, AvgChunkSeconds: avg, BlockedSeconds: int(blocked.Seconds()),
			ChunkTargetMB: nextT, Statement: ddl.ShrinkChunkSQL(f.Name, nextT, res),
		})
	}
	return result, nil
}

// shrinkLog runs the log-file algorithm (design §5.2): gate on the recovery model,
// then a single DBCC SHRINKFILE (no chunking — the log is truncated to a VLF
// boundary). SqlGoPace never issues BACKUP LOG; in FULL/BULK_LOGGED it waits, with
// a bound, for a scheduled backup to free the log.
func (r *ShrinkRunner) shrinkLog(ctx context.Context, op ddl.Shrink, res ddl.ResolvedOptions, ignore IgnoreSource, f mssql.FileSpace, sink ReactionSink) (ShrinkResult, error) {
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

	// The log shrink is a single unchunked statement, but it is not instant — it can be
	// blocked (a snapshot transaction, §8.2) and can block others. It runs under the same
	// monitoring as the TRUNCATEONLY pass, so the killers are consulted and its
	// percent_complete reaches the console.
	stmt := ddl.ShrinkChunkSQL(f.Name, final, res)
	base := ShrinkProgress{
		File: f.Name, Type: "log", StartMB: f.SizeMB, CurrentMB: f.SizeMB,
		FinalMB: final, Statement: stmt,
	}
	r.emitProgress(base)
	stopped, err := r.runWatchedStatement(ctx, stmt, "log shrink", base, ignore, sink)
	if err != nil {
		return result, fmt.Errorf("shrink %q: %w", f.Name, err)
	}
	size, err := r.reader.FileSizeMB(ctx, f.Name)
	if err != nil {
		return result, err
	}
	result.FinalMB = size
	if stopped {
		result.Reason = "stopped: graceful stop during the log shrink (freed space preserved)"
		return result, ErrStopped
	}
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
// failure, if any). It mirrors MonitoredRunner.runStatement. When watchSelfBlock is
// true, it also samples our own session in-flight (via pumpSelfBlock) for a concurrent
// maintenance operation blocking the chunk — the only window such a block is
// observable, since between chunks our session has no live request row (see
// pumpSelfBlock) — and returns the first such observation, if any.
func (r *ShrinkRunner) runChunk(ctx context.Context, file string, targetMB int, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink, base ShrinkProgress, watchSelfBlock bool) (stopped bool, maint *MaintBlock, err error) {
	stmt := ddl.ShrinkChunkSQL(file, targetMB, res)

	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	done := make(chan error, 1)
	go func() { done <- r.exec.ExecDDL(execCtx, stmt) }()

	sampleCtx, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	samples := make(chan Sample)
	go pumpSamples(sampleCtx, samples, r.sampler, r.pollIntv, r.logPoll, ignore)
	// Re-emit progress with the chunk's live server-side percent_complete while it runs.
	go r.pumpServerProgress(sampleCtx, base)

	var selfBlockCh chan MaintBlock
	if watchSelfBlock {
		selfBlockCh = make(chan MaintBlock, 1)
		go r.pumpSelfBlock(sampleCtx, selfBlockCh)
	}

	// A shrink chunk is cancel-safe: each internal ~32-page batch commits, so a stop
	// preserves work and is re-entrant. It is never resumable in the ALTER sense.
	caps := Capabilities{CancelSafe: true, MaxBlock: blockCap(res.MaxBlockMinutes)}
	action, pressure, serr := supervise(ctx, r.clk, caps, r.blockTO, samples, done)

	// Collect any in-flight maintenance-block observation (best-effort, non-blocking):
	// pumpSelfBlock sends at most once on a buffered channel and returns.
	if selfBlockCh != nil {
		select {
		case mb := <-selfBlockCh:
			maint = &mb
		default:
		}
	}

	if action == Continue {
		return false, maint, serr
	}

	sink(ReactionEvent{Kind: "pause", Detail: "shrink chunk stopped under pressure; committed work preserved — " + pressure.Detail()})
	r.cancelAndAwait(cancelExec, done, sink, "abort did not stop the chunk within the grace period")
	return true, maint, nil
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

// runTruncateOnly runs the free TRUNCATEONLY pass under monitoring. It is the Phase A
// case of runWatchedStatement.
func (r *ShrinkRunner) runTruncateOnly(ctx context.Context, file string, base ShrinkProgress, ignore IgnoreSource, sink ReactionSink) (stopped bool, err error) {
	return r.runWatchedStatement(ctx, ddl.ShrinkTruncateOnlySQL(file), "TRUNCATEONLY", base, ignore, sink)
}

// runWatchedStatement runs one *unchunked* shrink statement — the TRUNCATEONLY pass and the
// log shrink, the two statements the driver issues outside the chunk loop — while watching
// for a graceful stop. Neither is a chunk loop, so a drain cannot be honored at a chunk
// boundary; instead this polls the stop flag on the monitoring cadence and cancels the
// running statement when a stop is requested. What such a statement has already released is
// preserved on the server (a re-run resumes from the smaller size), so stopping one is a
// clean, re-entrant interruption. It returns stopped=true when a drain canceled it (the
// caller turns that into ErrStopped) and the raw exec error otherwise (nil on completion,
// ctx.Err on a hard cancel). It mirrors runChunk's cancel → KILL-grace fallback, drawing the
// KILL from the pool via Executor. label names the phase in the stop events; base is the
// progress re-emitted with the server's live percent_complete.
func (r *ShrinkRunner) runWatchedStatement(ctx context.Context, stmt, label string, base ShrinkProgress, ignore IgnoreSource, sink ReactionSink) (stopped bool, err error) {
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	done := make(chan error, 1)
	go func() { done <- r.exec.ExecDDL(execCtx, stmt) }()

	// Watch the server for as long as the statement runs — on a large file that is minutes,
	// and an unmonitored phase is one where nothing can act. The progress pump reports the
	// server's percent_complete; the sample pump keeps the blocking poll alive, which is
	// what consults the blocker and victim killers — both hang off Sampler.Blocking, so
	// without this poll no blocker is ever killed while these statements run. The samples
	// themselves are drained below: no pressure reaction applies here, since there is no
	// chunk boundary to pause at and aborting stays the operator's call, via a graceful stop.
	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go r.pumpServerProgress(watchCtx, base)
	samples := make(chan Sample)
	go pumpSamples(watchCtx, samples, r.sampler, r.pollIntv, r.logPoll, ignore)

	ticker := time.NewTicker(r.pollIntv)
	defer ticker.Stop()
	for {
		// Prefer a finished statement over a concurrent stop tick, so a statement that
		// completes on its own is never mis-reported as stopped.
		select {
		case err := <-done:
			return false, err
		default:
		}
		select {
		case err := <-done:
			return false, err
		case <-samples:
			// Drained: the poll runs for its side effects (the killers), not for a reaction.
		case <-ticker.C:
			if !stopRequested(r.stop) {
				continue
			}
			// The statement may have finished between this tick and the stop check; prefer
			// the real result over reporting a completed statement as stopped.
			select {
			case err := <-done:
				return false, err
			default:
			}
			sink(ReactionEvent{Kind: "pause", Detail: fmt.Sprintf("shrink %q %s stopped on graceful stop; freed space preserved", base.File, label)})
			r.cancelAndAwait(cancelExec, done, sink, label+" did not stop within the grace period")
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

// pumpServerProgress polls the running statement's server-side percent_complete
// (dm_exec_requests, which SQL Server populates for DBCC SHRINKFILE — the TRUNCATEONLY pass
// included) on the blocking cadence and re-emits base with it filled in, so the console shows
// the server's own view of the statement while it runs. Reads go through the pooled connection,
// concurrent with the statement on its own pooled connection — the same pattern the sampler
// uses. It stops when the caller's context is canceled. A rollback (a chunk being aborted under
// pressure) reports rollback progress, not forward progress, so it is skipped.
func (r *ShrinkRunner) pumpServerProgress(ctx context.Context, base ShrinkProgress) {
	t := time.NewTicker(r.pollIntv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p, found, err := r.reader.Progress(ctx, r.exec.SPID())
			if err != nil || !found || p.PercentComplete <= 0 || p.IsRollback() {
				continue
			}
			b := base
			b.PercentComplete = p.PercentComplete
			r.emitProgress(b)
		}
	}
}

// pumpSelfBlock samples the running chunk's self-block: while a DBCC SHRINKFILE chunk waits on a
// lock held by a concurrent operation, our session appears in dm_exec_requests with a
// blocking_session_id. It reads immediately, then on the blocking poll cadence, and sends the
// first observation of a maintenance blocker (ALTER INDEX / DBCC) on out (buffered, at most once),
// then returns. Reads go through the pooled connection, concurrent with the chunk — the same
// pattern pumpServerProgress uses. Best-effort: a read error is skipped; a non-maintenance or
// absent blocker just keeps polling until the chunk's sampling context is canceled.
func (r *ShrinkRunner) pumpSelfBlock(ctx context.Context, out chan<- MaintBlock) {
	observe := func() bool {
		sessions, err := r.reader.ActiveSessions(ctx)
		if err != nil {
			return false
		}
		sb := mssql.FindSelfBlock(sessions, r.exec.SPID())
		if !sb.Blocked || !mssql.IsMaintenanceCommand(sb.Command) {
			return false
		}
		select {
		case out <- MaintBlock{SPID: sb.SPID, Command: sb.Command, WaitMS: sb.WaitMS}:
		default:
		}
		return true
	}
	if observe() {
		return
	}
	t := time.NewTicker(r.pollIntv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if observe() {
				return
			}
		}
	}
}

// stall records a no-progress chunk (a real no-gain, or any DBCC error that left the file
// unmoved — "could not adjust the space allocation", a buffer-latch time-out, …) and
// decides whether to give up. On the tempdb
// path (prof != nil && prof.FlushCaches), once the stall persists to NoProgressBeforeFlush
// it flushes the temp-object cache exactly once (the flushed guard) and gives the loop a
// fresh no-progress budget rather than counting straight through to a give-up. Otherwise it
// returns stop=true once the no-progress count or the total self-wait budget trips, so the
// caller ends the shrink cleanly with the reduction so far preserved; otherwise it backs off
// (doubling each time) and returns stop=false so the caller retries. The pointers are the
// loop's running counters.
func (r *ShrinkRunner) stall(ctx context.Context, file string, noProgress *int, backoff, stallWaited *time.Duration, sink ReactionSink, prof *TempdbProfile) (stop bool, err error) {
	*noProgress++
	// Tempdb escalation: once, when the stall is persistent and flushing is enabled.
	if prof != nil && prof.FlushCaches && prof.flushed != nil && !*prof.flushed && *noProgress >= r.tuning.NoProgressBeforeFlush {
		if ferr := r.flushTempdbCaches(ctx, sink); ferr != nil {
			return false, ferr
		}
		*prof.flushed = true
		*noProgress = 0 // give the freed pages a fresh budget
		return false, nil
	}
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

// flushTempdbCaches releases the temp-object cachestore that pins tempdb pages, preceded
// by a CHECKPOINT. Targeted, not instance-wide: it deliberately avoids FREEPROCCACHE /
// FREESYSTEMCACHE('ALL') (whole-plan-cache recompile storm) and DROPCLEANBUFFERS
// (buffer-pool wipe). Runs on the tempdb-scoped exec connection.
func (r *ShrinkRunner) flushTempdbCaches(ctx context.Context, sink ReactionSink) error {
	sink(ReactionEvent{Kind: "pause", Detail: "tempdb stall: flushing temp-object cache (CHECKPOINT + FREESYSTEMCACHE)"})
	return r.exec.ExecDDL(ctx, "CHECKPOINT;\nDBCC FREESYSTEMCACHE ('Temporary Tables & Table Variables') WITH NO_INFOMSGS;")
}

// applyMaintBlock promotes an in-flight maintenance-block observation (from runChunk) to the
// operation's tailProbe once the shrink has stalled, emitting one warning per operation. Called
// only from the stall branches, so being here already means the shrink is not making progress; a
// non-nil mb means pumpSelfBlock observed our session blocked by a concurrent ALTER INDEX / DBCC.
// Pure and main-goroutine-only. No-op on the tempdb path (tp == nil) or once already recorded.
func (r *ShrinkRunner) applyMaintBlock(sink ReactionSink, f mssql.FileSpace, tp *tailProbe, mb *MaintBlock) {
	if tp == nil || tp.maintBlock != nil || mb == nil {
		return
	}
	tp.maintBlock = mb
	if tp.maintWarned != nil && !*tp.maintWarned {
		*tp.maintWarned = true
		sink(ReactionEvent{Kind: "warn", Detail: fmt.Sprintf(
			"shrink of %q blocked by a concurrent maintenance operation — %s on session %d (waiting %s). "+
				"Transient; SqlGoPace is yielding, not forcing. Re-run after maintenance completes.",
			f.Name, mb.Command, mb.SPID, formatWait(mb.WaitMS))})
	}
}

// formatWait renders a millisecond wait as a compact duration (e.g. "12m31s").
func formatWait(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}

// walkTail runs the tail-object walk for one data file, gated on version, and returns the
// finding (nil when unavailable or nothing is found). It emits the "needs 2019+" warning only
// when warn is true — i.e. for the proactive walk the operator explicitly opted into. The
// always-on reactive give-up walk passes warn=false, so a run that never asked for the feature
// is not nagged on old servers. A read error is surfaced as a warning regardless.
func (r *ShrinkRunner) walkTail(ctx context.Context, f mssql.FileSpace, sink ReactionSink, warned *bool, warn bool) *TailFinding {
	if r.major < 15 {
		if warn && warned != nil && !*warned {
			*warned = true
			sink(ReactionEvent{Kind: "warn", Detail: "tail-object identification needs SQL Server 2019+ (sys.dm_db_page_info); skipped"})
		}
		return nil
	}
	o, found, err := r.reader.FindTailObject(ctx, f.FileID, tailWalkPages(f.FreeMB))
	if err != nil {
		sink(ReactionEvent{Kind: "warn", Detail: fmt.Sprintf("tail-object walk failed on %q: %v", f.Name, err)})
		return nil
	}
	if !found {
		return nil
	}
	return &TailFinding{ObjectID: o.ObjectID, Schema: o.Schema, Table: o.Table, IndexID: o.IndexID, PageFromEnd: o.PageFromEnd}
}

// emitTail logs a tail finding to the console/report. When record is true the finding is
// attached to the event so the engine records it into the .contended.yaml sidecar as a
// confirmed blocker; when false it is a diagnostic line only — used for the proactive walk,
// whose finding is confirmed (recorded) only if the shrink later misses target.
func (r *ShrinkRunner) emitTail(sink ReactionSink, f *TailFinding, fileName string, record bool) {
	ev := ReactionEvent{
		Kind:   "info",
		Detail: fmt.Sprintf("tail object %s.%s (index_id=%d, %d pages from end) on %q", f.Schema, f.Table, f.IndexID, f.PageFromEnd, fileName),
	}
	if record {
		ev.Tail = f
	}
	sink(ev)
}

// captureGiveUpTail records the tail object when a data shrink gives up ("no further
// progress") — the always-on reactive walk. It walks fresh and records only when the
// proactive walk did not already stash a finding (which shrinkData records post-loop for a
// missed target); this keeps the give-up path to a single read. warn=false: a run that never
// opted in must not be nagged about a pre-2019 server just for stalling.
func (r *ShrinkRunner) captureGiveUpTail(ctx context.Context, f mssql.FileSpace, sink ReactionSink, tp *tailProbe) {
	if tp == nil || tp.finding != nil {
		return
	}
	tf := r.walkTail(ctx, f, sink, tp.warned, false)
	if tf == nil {
		return
	}
	r.markTransient(tf, tp)
	r.emitTail(sink, tf, f.Name, true)
}

// markTransient tags a give-up tail finding as transient-maintenance when a concurrent
// maintenance op was found blocking the shrink, so it is recorded as informational rather
// than a structural blocker.
//
// Note: this tags the tail object the walk found even if the concurrent maintenance op operates
// on a different object — the maintenance context is treated as the salient explanation, and a
// re-run without concurrent maintenance re-classifies a genuine structural tail as tail_position.
func (r *ShrinkRunner) markTransient(tf *TailFinding, tp *tailProbe) {
	if tp == nil || tp.maintBlock == nil || tf == nil {
		return
	}
	tf.Transient = true
	tf.BlockedByCommand = tp.maintBlock.Command
	tf.BlockedBySPID = tp.maintBlock.SPID
}

// giveUpReason builds the give-up reason, naming a concurrent maintenance op when one was
// found blocking the shrink; base is the default reason for a non-maintenance give-up.
func giveUpReason(tp *tailProbe, base string) string {
	if tp != nil && tp.maintBlock != nil {
		return fmt.Sprintf("stopped: file tail pinned by concurrent maintenance (%s, session %d) — "+
			"transient, not a structural blocker; work preserved, re-run after maintenance",
			tp.maintBlock.Command, tp.maintBlock.SPID)
	}
	return base
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
