package run

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// Preflighter runs the preflight checks for a manifest.
type Preflighter interface {
	Check(ctx context.Context, m *ddl.Manifest) (preflight.Report, error)
}

// OpRunner executes one planned operation (with monitoring and reaction). caps
// reports the reaction capabilities derived from the resolved options and server;
// sink receives reaction events (pause/resume/cancel/kill) as they happen.
type OpRunner interface {
	Run(ctx context.Context, op ddl.Operation, sql string, caps Capabilities, sink ReactionSink) error
}

// ShrinkDriver runs a shrink operation, which does not fit the one-statement
// OpRunner model: it reads DMVs at run time, builds per-chunk SQL, and loops. The
// engine routes ddl.Shrink operations here. *ShrinkRunner satisfies it.
type ShrinkDriver interface {
	Run(ctx context.Context, op ddl.Shrink, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) ([]ShrinkResult, error)
}

var _ ShrinkDriver = (*ShrinkRunner)(nil)

// SessionInfo provides the execution session signature for the crash-recovery
// sidecar, including a CONTEXT_INFO marker so an orphaned session can be correlated
// to its run reliably (a bare SPID is reused). *mssql.Conn satisfies it.
type SessionInfo interface {
	SPID() int
	LoginTime(ctx context.Context) (string, error)
	SetMarker(ctx context.Context, marker [16]byte) error
}

// ResumableProbe reports whether an interrupted operation left a paused resumable
// rebuild behind (so it is recoverable rather than failed). *mssql.Conn satisfies it.
type ResumableProbe interface {
	PausedResumable(ctx context.Context, schema, table, index string) (bool, error)
}

var _ ResumableProbe = (*mssql.Conn)(nil)

// IndexExpander lists a table's concrete indexes so an "ALTER INDEX ALL" rebuild
// can be expanded into one rebuild per index (clustered first), letting each carry
// RESUMABLE. *mssql.Conn satisfies it.
type IndexExpander interface {
	RebuildableIndexes(ctx context.Context, schema, table string) ([]mssql.IndexInfo, error)
}

var _ IndexExpander = (*mssql.Conn)(nil)

// ProgressReader reads the running operation's completion estimate, used to record
// where an operation stood when it was interrupted. *mssql.Conn satisfies it.
type ProgressReader interface {
	Progress(ctx context.Context, spid int) (mssql.Progress, bool, error)
}

var _ ProgressReader = (*mssql.Conn)(nil)

// WaitReader reads a session's cumulative wait statistics, used to report what
// slowed an operation down. *mssql.Conn satisfies it.
type WaitReader interface {
	SessionWaits(ctx context.Context, spid int) ([]mssql.SessionWait, error)
}

var _ WaitReader = (*mssql.Conn)(nil)

// BlockerReader reads the active sessions so the engine can record which ones our
// DDL was blocking when it reacted, for the advisory capture file. *mssql.Conn
// satisfies it.
type BlockerReader interface {
	ActiveSessions(ctx context.Context) ([]mssql.Session, error)
}

var _ BlockerReader = (*mssql.Conn)(nil)

// Summary counts the outcome of a ProcessAll run.
type Summary struct {
	Done        int
	Failed      int
	Interrupted int // paused and left for recovery (session killed / connection lost)
}

// runOutcome is the result of processing one manifest.
type runOutcome int

const (
	outcomeDone runOutcome = iota
	outcomeFailed
	outcomeInterrupted
)

// Engine is the outer orchestration loop: it walks the manifest queue and, for
// each, claims it, preflights, plans, runs the operations, writes a run report,
// and routes it to done or failed.
type Engine struct {
	dirs             Dirs
	queue            *Queue
	target           ddl.Target
	matrix           *ddl.Matrix
	policy           ddl.Policy
	pf               Preflighter
	runner           OpRunner
	shrink           ShrinkDriver
	adr              bool
	clk              Clock
	session          SessionInfo
	notifier         *report.Notifier
	history          *report.History
	expander         IndexExpander
	progress         ProgressReader
	waits            WaitReader
	blockers         BlockerReader
	resumeCheck      ResumableProbe
	reconnectTimeout time.Duration
	database         string            // when set, process only manifests for this database
	liveReload       bool              // re-read ignore_blocked_sessions from the manifest mid-run
	manifestObserver func(path string) // notified of the in-flight manifest path (TUI editing)
	holdPoll         time.Duration     // cadence for narrating held-through ignored sessions
	out              io.Writer
}

// defaultHoldPoll is how often the engine narrates the ignored sessions it is holding
// the lock through, when a blocker reader is wired.
const defaultHoldPoll = 30 * time.Second

// EngineOption configures optional Engine behavior.
type EngineOption func(*Engine)

// WithADR sets the target's Accelerated Database Recovery state (biases reactions).
func WithADR(adr bool) EngineOption { return func(e *Engine) { e.adr = adr } }

// WithShrinkRunner routes ddl.Shrink operations to the dedicated shrink driver
// instead of the OpRunner. Without it, a shrink operation falls back to the
// OpRunner, whose indicative PlannedOperation.SQL is not the real per-chunk SQL —
// so a shrink driver should always be wired when shrink manifests are expected.
func WithShrinkRunner(d ShrinkDriver) EngineOption { return func(e *Engine) { e.shrink = d } }

// WithClock sets the clock (defaults to System).
func WithClock(c Clock) EngineOption { return func(e *Engine) { e.clk = c } }

// WithSession enables crash-recovery sidecars from the execution session.
func WithSession(s SessionInfo) EngineOption { return func(e *Engine) { e.session = s } }

// WithNotifier enables webhook notifications.
func WithNotifier(n *report.Notifier) EngineOption { return func(e *Engine) { e.notifier = n } }

// WithHistory enables run-history persistence.
func WithHistory(h *report.History) EngineOption { return func(e *Engine) { e.history = h } }

// WithOutput sets the progress narration writer (defaults to io.Discard).
func WithOutput(w io.Writer) EngineOption { return func(e *Engine) { e.out = w } }

// WithExpander enables expanding "ALTER INDEX ALL" rebuilds into one rebuild per
// concrete index. Without it, an ALL rebuild is run as a single statement.
func WithExpander(x IndexExpander) EngineOption { return func(e *Engine) { e.expander = x } }

// WithProgress lets the engine record the operation's completion percentage when
// it is interrupted (pause/cancel/abort/kill). Requires a session for the SPID.
func WithProgress(p ProgressReader) EngineOption { return func(e *Engine) { e.progress = p } }

// WithWaits lets the engine record, per operation, which waits slowed it down
// (from sys.dm_exec_session_wait_stats). Requires a session for the SPID.
func WithWaits(w WaitReader) EngineOption { return func(e *Engine) { e.waits = w } }

// WithBlockerReader lets the engine capture the sessions it was blocking when it
// reacts, to an advisory <manifest>.blocked.yaml next to the run report. Requires a
// session for the DDL's SPID.
func WithBlockerReader(b BlockerReader) EngineOption { return func(e *Engine) { e.blockers = b } }

// WithLiveReload re-reads the running manifest's ignore_blocked_sessions on each
// blocking poll, so an exclusion added during the run (by hand or by the TUI) takes
// effect before the operation would abort — without restarting it.
func WithLiveReload() EngineOption { return func(e *Engine) { e.liveReload = true } }

// WithManifestObserver registers a callback notified of the path of the manifest the
// engine is currently processing ("" between manifests), so a host (the TUI) can write
// an ignore rule into the running manifest. Pairs with WithLiveReload.
func WithManifestObserver(f func(path string)) EngineOption {
	return func(e *Engine) { e.manifestObserver = f }
}

// WithHoldPoll sets how often the engine narrates the ignored sessions it is holding
// the lock through (default 30s). Zero disables held-through narration. Requires a
// blocker reader and a session.
func WithHoldPoll(d time.Duration) EngineOption { return func(e *Engine) { e.holdPoll = d } }

// WithResumeCheck lets the engine recognize an interrupted-but-paused resumable
// operation (session killed / connection lost) as recoverable rather than failed.
func WithResumeCheck(p ResumableProbe) EngineOption { return func(e *Engine) { e.resumeCheck = p } }

// WithReconnectTimeout sets how long the resumable check retries while the server
// is unreachable (e.g. restarting), before deciding from the available evidence.
func WithReconnectTimeout(d time.Duration) EngineOption {
	return func(e *Engine) { e.reconnectTimeout = d }
}

// WithDatabase declares the database this engine's connection targets, so it
// processes only manifests for that database — those with no `database:` field, or
// one matching (case-insensitive). Manifests targeting another database are left in
// the queue for the engine that owns it (spec §17.6). Empty (the default) processes
// every manifest, regardless of its `database:` field.
func WithDatabase(name string) EngineOption { return func(e *Engine) { e.database = name } }

// NewEngine wires an Engine over the lifecycle directories and required
// dependencies; optional behavior is supplied via options.
func NewEngine(dirs Dirs, target ddl.Target, matrix *ddl.Matrix, policy ddl.Policy, pf Preflighter, runner OpRunner, opts ...EngineOption) *Engine {
	e := &Engine{
		dirs:     dirs,
		queue:    NewQueue(dirs),
		target:   target,
		matrix:   matrix,
		policy:   policy,
		pf:       pf,
		runner:   runner,
		clk:      System,
		holdPoll: defaultHoldPoll,
		out:      io.Discard,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// ProcessAll runs every manifest currently in the to_run directory, sequentially.
func (e *Engine) ProcessAll(ctx context.Context) (Summary, error) {
	if err := e.queue.EnsureDirs(); err != nil {
		return Summary{}, err
	}
	names, err := e.queue.Discover()
	if err != nil {
		return Summary{}, err
	}

	var sum Summary
	for _, name := range names {
		if !e.ownsManifest(name) {
			fmt.Fprintf(e.out, "skip %s: targets another database; left in queue (run is on %q)\n", name, e.database)
			continue
		}
		switch e.processOne(ctx, name) {
		case outcomeDone:
			sum.Done++
		case outcomeInterrupted:
			sum.Interrupted++
		default:
			sum.Failed++
		}
	}
	return sum, nil
}

// ownsManifest reports whether this engine should process the named manifest: it
// does when no database filter is set, or the manifest's database is empty or
// matches the filter (case-insensitive). A manifest that cannot be read is claimed
// anyway so processOne surfaces the load error properly.
func (e *Engine) ownsManifest(name string) bool {
	if e.database == "" {
		return true
	}
	m, err := ddl.LoadManifestFile(filepath.Join(e.dirs.ToRun, name))
	if err != nil {
		return true
	}
	return m.Database == "" || strings.EqualFold(m.Database, e.database)
}

func (e *Engine) processOne(ctx context.Context, name string) runOutcome {
	start := e.clk.Now()
	rep := &report.RunReport{Manifest: name, StartedAt: e.now()}

	procPath, err := e.queue.Claim(name)
	if err != nil {
		fmt.Fprintf(e.out, "skip %s: %v\n", name, err)
		return outcomeFailed
	}
	e.setCurrentManifest(procPath)
	defer e.setCurrentManifest("")
	e.writeSidecar(ctx, name)

	manifest, err := ddl.LoadManifestFile(procPath)
	if err != nil {
		rep.Error = "load manifest: " + err.Error()
		return e.finalize(ctx, name, rep, start, false)
	}

	// Expand "ALTER INDEX ALL" into per-index rebuilds before preflight, so each
	// real index is verified and can carry RESUMABLE.
	if e.expander != nil {
		manifest, err = ExpandAll(ctx, e.expander, manifest)
		if err != nil {
			rep.Error = "expand ALL rebuild: " + err.Error()
			return e.finalize(ctx, name, rep, start, false)
		}
	}

	pfReport, err := e.pf.Check(ctx, manifest)
	rep.Preflight = checkLines(pfReport)
	if err != nil {
		rep.Error = "preflight: " + err.Error()
		return e.finalize(ctx, name, rep, start, false)
	}
	if pfReport.HasFailure() {
		rep.Error = "preflight failed"
		return e.finalize(ctx, name, rep, start, false)
	}

	planned, err := ddl.Plan(manifest, e.target, e.matrix, e.policy)
	if err != nil {
		rep.Error = "plan: " + err.Error()
		return e.finalize(ctx, name, rep, start, false)
	}

	// Sessions the operator allows to stay blocked, applied to every operation in the
	// manifest. The regexps were validated at load, so an error here is defensive.
	ignore, err := e.ignoreSource(name, manifest.IgnoreBlockedSessions)
	if err != nil {
		rep.Error = "compile ignore_blocked_sessions: " + err.Error()
		return e.finalize(ctx, name, rep, start, false)
	}

	var failedOps []ddl.Operation
	captured := &blockerCapture{}
	for i, step := range planned {
		opStart := e.clk.Now()
		caps := Capabilities{Resumable: step.Options.Resumable, ADR: e.adr, CancelSafe: cancelSafe(step.Operation), IgnoreBlocking: step.Options.IgnoreBlocking, Ignore: ignore, MaxBlock: blockCap(step.Options.MaxBlockMinutes)}

		// The sink is called from the runner (this goroutine) and from the held-through
		// narrator (a sibling goroutine), so the shared report state is mutex-guarded.
		var (
			reactions  []report.ReactionLine
			reactionMu sync.Mutex
		)
		sink := func(ev ReactionEvent) {
			detail := ev.Detail
			if isInterruption(ev.Kind) {
				if pct, ok := e.operationPercent(ctx); ok {
					detail = fmt.Sprintf("%s (at %.0f%%)", detail, pct)
				}
			}
			reactionMu.Lock()
			reactions = append(reactions, report.ReactionLine{Kind: ev.Kind, At: e.now(), Detail: detail})
			fmt.Fprintf(e.out, "-- %s %s: %s\n", ev.Kind, opTarget(step.Operation), detail)
			reactionMu.Unlock()
			if ev.Kind == "pause" || ev.Kind == "cancel" || ev.Kind == "abort" {
				e.notify(ctx, ev.Kind, name, fmt.Sprintf("%s on %s (%s)", ev.Kind, opTarget(step.Operation), detail))
				e.captureBlockers(ctx, ignore, captured, name)
			}
		}
		waitsBefore := e.snapshotWaits(ctx)

		// Narrate, once each, the ignored sessions we hold the lock through, so the run
		// log shows we are deliberately blocking them (otherwise it is a silent non-event).
		holdCtx, stopHold := context.WithCancel(ctx)
		go e.narrateHeld(holdCtx, ignore, sink)

		var (
			runErr        error
			shrinkResults []ShrinkResult
		)
		if sh, ok := step.Operation.(ddl.Shrink); ok && e.shrink != nil {
			// Shrink is multi-statement and built at run time; route to the driver,
			// passing the resolved options (only WALP is meaningful for a shrink).
			shrinkResults, runErr = e.shrink.Run(ctx, sh, step.Options, ignore, sink)
		} else {
			runErr = e.runner.Run(ctx, step.Operation, step.SQL, caps, sink)
		}
		stopHold()
		waitLines, waitTotal := e.operationWaits(ctx, waitsBefore)

		opRep := report.OperationReport{
			Index:       i + 1,
			CommandType: step.Operation.CommandType(),
			Target:      opTarget(step.Operation),
			SQL:         step.SQL,
			Options:     optionDecisions(step.Decisions),
			Reactions:   reactions,
			Waits:       waitLines,
			WaitTotalMS: waitTotal,
			Shrink:      shrinkReport(shrinkResults),
			DurationMS:  e.msSince(opStart),
		}
		if runErr != nil {
			opRep.Error = runErr.Error()
			// An unexpected error on a resumable operation that left a PAUSED
			// resumable rebuild behind (session killed / connection lost / server
			// restart) is recoverable, not a failure: keep the manifest and sidecar
			// in processing so the next run resumes it.
			if caps.Resumable && e.resumableInterruption(ctx, step.Operation) {
				opRep.Outcome = "interrupted"
				rep.Operations = append(rep.Operations, opRep)
				rep.Error = fmt.Sprintf("operation %d (%s) interrupted; paused and recoverable: %v", i, step.Operation.CommandType(), runErr)
				return e.finalizeInterrupted(ctx, name, rep, start)
			}
			opRep.Outcome = "failed"
			rep.Operations = append(rep.Operations, opRep)
			if !manifest.Continue() {
				rep.Error = fmt.Sprintf("operation %d (%s): %v", i, step.Operation.CommandType(), runErr)
				return e.finalize(ctx, name, rep, start, false)
			}
			// continue-on-failure: quarantine the failed op and keep going.
			failedOps = append(failedOps, step.Operation)
			fmt.Fprintf(e.out, "-- continue-on-failure: operation %d (%s) failed, quarantined: %v\n", i, step.Operation.CommandType(), runErr)
			continue
		}
		opRep.Outcome = "success"
		rep.Operations = append(rep.Operations, opRep)
	}
	return e.finalizeAll(ctx, name, manifest, rep, start, failedOps)
}

// ignoreSource builds the run's ignore matcher from the manifest rules. With live
// reload enabled it returns a source that re-reads the in-processing manifest each
// blocking poll (so a rule added mid-run is honored before the next abort); otherwise
// it is a fixed snapshot. The rules are validated at load, so a compile error here is
// defensive.
func (e *Engine) ignoreSource(name string, rules []ddl.IgnoredSession) (IgnoreSource, error) {
	compiled, err := CompileIgnoredSessions(rules)
	if err != nil {
		return nil, err
	}
	if e.liveReload {
		return newManifestSource(filepath.Join(e.dirs.Processing, name), compiled), nil
	}
	return staticIgnore{rules: compiled}, nil
}

// finalize records a terminal outcome: moves the manifest, writes the report,
// notifies, and persists history.
func (e *Engine) finalize(ctx context.Context, name string, rep *report.RunReport, start time.Time, success bool) runOutcome {
	e.removeSidecar(name)
	rep.FinishedAt = e.now()
	rep.DurationMS = e.msSince(start)

	dir := e.dirs.Failed
	rep.Outcome = "FAILED"
	if success {
		dir = e.dirs.Done
		rep.Outcome = "SUCCESS"
		if err := e.queue.Complete(name); err != nil {
			fmt.Fprintf(e.out, "complete %s: %v\n", name, err)
		}
	} else if err := e.queue.Fail(name); err != nil {
		fmt.Fprintf(e.out, "fail %s: %v\n", name, err)
	}
	e.relocateCapture(name, dir)

	if err := report.WriteFile(filepath.Join(dir, name+".log"), *rep); err != nil {
		fmt.Fprintf(e.out, "write log %s: %v\n", name, err)
	}
	if !success {
		e.notify(ctx, "fail", name, rep.Error)
	}
	e.record(ctx, *rep)

	fmt.Fprintf(e.out, "%s: %s\n", map[bool]string{true: "done", false: "failed"}[success], name)
	if success {
		return outcomeDone
	}
	return outcomeFailed
}

// finalizeAll routes a manifest whose operation loop completed. With no failed
// operations it is a plain success. With some failed operations (only reachable in
// continue-on-failure mode) it is a PARTIAL run: a recovery manifest is written and
// the original manifest is routed to failed for the operator to follow up.
func (e *Engine) finalizeAll(ctx context.Context, name string, m *ddl.Manifest, rep *report.RunReport, start time.Time, failed []ddl.Operation) runOutcome {
	if len(failed) == 0 {
		return e.finalize(ctx, name, rep, start, true)
	}
	return e.finalizePartial(ctx, name, m, rep, start, failed)
}

// finalizePartial records a PARTIAL outcome: some operations succeeded and some
// were quarantined. It moves the original manifest to failed, writes a re-runnable
// recovery manifest (the failed operations, carrying on_failure: continue) next to
// it, and reports/records the run like a failure so the exit code reflects it.
func (e *Engine) finalizePartial(ctx context.Context, name string, m *ddl.Manifest, rep *report.RunReport, start time.Time, failed []ddl.Operation) runOutcome {
	e.removeSidecar(name)
	rep.FinishedAt = e.now()
	rep.DurationMS = e.msSince(start)
	rep.Outcome = "PARTIAL"

	// Carry the ignore_blocked_sessions into the recovery manifest so a resumed run
	// remembers them — including any rule added mid-run, read from the in-processing
	// manifest before the queue move takes it away.
	ignore := e.latestIgnoreRules(name, m)

	if err := e.queue.Fail(name); err != nil {
		fmt.Fprintf(e.out, "fail %s: %v\n", name, err)
	}
	e.relocateCapture(name, e.dirs.Failed)

	recovery := &ddl.Manifest{
		Description:           recoveryDescription(m, name),
		Database:              m.Database,
		OnFailure:             ddl.OnFailureContinue,
		IgnoreBlockedSessions: ignore,
		Operations:            failed,
	}
	recName := name + ".recovery.yaml"
	rep.Error = fmt.Sprintf("%d of %d operation(s) failed; recovery manifest: %s", len(failed), len(rep.Operations), recName)
	if err := e.writeRecovery(filepath.Join(e.dirs.Failed, recName), recovery); err != nil {
		fmt.Fprintf(e.out, "write recovery manifest %s: %v\n", recName, err)
	}

	if err := report.WriteFile(filepath.Join(e.dirs.Failed, name+".log"), *rep); err != nil {
		fmt.Fprintf(e.out, "write log %s: %v\n", name, err)
	}
	e.notify(ctx, "fail", name, rep.Error)
	e.record(ctx, *rep)

	fmt.Fprintf(e.out, "partial: %s — %d failed op(s) quarantined to %s\n", name, len(failed), recName)
	return outcomeFailed
}

// latestIgnoreRules returns the manifest's ignore_blocked_sessions, preferring the
// current on-disk copy in processing (which reflects any mid-run edit) and falling
// back to the in-memory manifest when it cannot be re-read.
func (e *Engine) latestIgnoreRules(name string, m *ddl.Manifest) []ddl.IgnoredSession {
	if latest, err := ddl.LoadManifestFile(filepath.Join(e.dirs.Processing, name)); err == nil {
		return latest.IgnoreBlockedSessions
	}
	return m.IgnoreBlockedSessions
}

// writeRecovery renders a recovery manifest to YAML and writes it.
func (e *Engine) writeRecovery(path string, m *ddl.Manifest) error {
	data, err := ddl.MarshalManifest(m)
	if err != nil {
		return fmt.Errorf("marshal recovery manifest: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write recovery manifest: %w", err)
	}
	return nil
}

// recoveryDescription builds the recovery manifest's description from the original.
func recoveryDescription(m *ddl.Manifest, name string) string {
	if m.Description != "" {
		return "Recovery for: " + m.Description
	}
	return "Recovery for " + name
}

// finalizeInterrupted records a recoverable interruption: the manifest and its
// sidecar are LEFT in processing so the next run's crash recovery resumes the
// paused operation. No move and no sidecar removal happen here.
func (e *Engine) finalizeInterrupted(ctx context.Context, name string, rep *report.RunReport, start time.Time) runOutcome {
	rep.FinishedAt = e.now()
	rep.DurationMS = e.msSince(start)
	rep.Outcome = "INTERRUPTED"

	e.notify(ctx, "interrupted", name, rep.Error)
	e.record(ctx, *rep)

	fmt.Fprintf(e.out, "interrupted: %s — paused, left in processing for recovery\n", name)
	return outcomeInterrupted
}

// reconnectProbeInterval is how often the resumable check is retried while the
// server is unreachable.
const reconnectProbeInterval = 3 * time.Second

// resumableProbeResult is the conclusion of probing for a paused resumable op.
type resumableProbeResult struct {
	paused     bool
	conclusive bool // false when the server stayed unreachable across all attempts
}

// probeResumable calls check until it returns a non-error answer (conclusive) or
// the timeout elapses (inconclusive — the server stayed unreachable, e.g. a
// restart in progress), sleeping between tries. It is pure so the retry logic is
// tested deterministically.
func probeResumable(check func() (bool, error), clk Clock, timeout time.Duration, sleep func()) resumableProbeResult {
	deadline := clk.Now().Add(timeout)
	for {
		if paused, err := check(); err == nil {
			return resumableProbeResult{paused: paused, conclusive: true}
		}
		if !clk.Now().Before(deadline) {
			return resumableProbeResult{conclusive: false}
		}
		sleep()
	}
}

// resumableInterruption reports whether an errored operation should be treated as
// a recoverable interruption rather than a failure. It is true when the target
// index has a paused resumable rebuild, or when the server stayed unreachable so
// the state cannot be read — in which case a resumable operation is kept for the
// next run's recovery to classify (Resume if paused, Restart otherwise), so work
// is never lost to a transient outage or a server restart.
func (e *Engine) resumableInterruption(ctx context.Context, op ddl.Operation) bool {
	if e.resumeCheck == nil {
		return false
	}
	ref := op.Target()
	res := probeResumable(
		func() (bool, error) { return e.resumeCheck.PausedResumable(ctx, ref.Schema, ref.Table, ref.Name) },
		e.clk,
		e.reconnectTimeout,
		func() { time.Sleep(reconnectProbeInterval) },
	)
	return res.paused || !res.conclusive
}

// setCurrentManifest notifies the manifest observer (if any) of the in-flight
// manifest path, so a host can edit it while it runs.
func (e *Engine) setCurrentManifest(path string) {
	if e.manifestObserver != nil {
		e.manifestObserver(path)
	}
}

func (e *Engine) now() string               { return e.clk.Now().UTC().Format(time.RFC3339) }
func (e *Engine) msSince(t time.Time) int64 { return e.clk.Since(t).Milliseconds() }

func (e *Engine) notify(ctx context.Context, event, name, detail string) {
	if e.notifier == nil {
		return
	}
	if err := e.notifier.Notify(ctx, event, map[string]any{"manifest": name, "detail": detail}); err != nil {
		fmt.Fprintf(e.out, "notify %s: %v\n", name, err)
	}
}

func (e *Engine) record(ctx context.Context, rep report.RunReport) {
	if e.history == nil {
		return
	}
	rec := report.RunRecord{
		Manifest:   rep.Manifest,
		Outcome:    rep.Outcome,
		StartedAt:  rep.StartedAt,
		FinishedAt: rep.FinishedAt,
		Operations: len(rep.Operations),
		DurationMS: rep.DurationMS,
		Error:      rep.Error,
	}
	if err := e.history.Record(ctx, rec); err != nil {
		fmt.Fprintf(e.out, "history %s: %v\n", rep.Manifest, err)
	}
}

// writeSidecar records the run state next to the manifest so a crash can be
// recovered. It also stamps a random CONTEXT_INFO marker on the execution session
// so an orphaned session can be correlated to its run beyond the reusable SPID. It
// is a best-effort no-op when no session is configured.
func (e *Engine) writeSidecar(ctx context.Context, name string) {
	if e.session == nil {
		return
	}
	login, err := e.session.LoginTime(ctx)
	if err != nil {
		fmt.Fprintf(e.out, "sidecar %s: login time: %v\n", name, err)
	}

	marker := e.stampMarker(ctx, name)

	state := State{
		Manifest:  name,
		Database:  e.database,
		SPID:      e.session.SPID(),
		LoginTime: login,
		Marker:    marker,
		StartedAt: e.now(),
	}
	if err := WriteState(e.sidecarPath(name), state); err != nil {
		fmt.Fprintf(e.out, "sidecar %s: %v\n", name, err)
	}
}

// stampMarker generates a random 16-byte marker, writes it to CONTEXT_INFO on the
// execution session, and returns its "0x…" literal for the sidecar. Best-effort:
// returns "" when the marker cannot be generated or set.
func (e *Engine) stampMarker(ctx context.Context, name string) string {
	var marker [16]byte
	if _, err := rand.Read(marker[:]); err != nil {
		fmt.Fprintf(e.out, "sidecar %s: marker: %v\n", name, err)
		return ""
	}
	if err := e.session.SetMarker(ctx, marker); err != nil {
		fmt.Fprintf(e.out, "sidecar %s: set marker: %v\n", name, err)
		return ""
	}
	return mssql.ContextInfoLiteral(marker)
}

func (e *Engine) removeSidecar(name string) {
	if e.session == nil {
		return
	}
	if err := RemoveState(e.sidecarPath(name)); err != nil {
		fmt.Fprintf(e.out, "sidecar %s: %v\n", name, err)
	}
}

func (e *Engine) sidecarPath(name string) string {
	return filepath.Join(e.dirs.Processing, name+stateSuffix)
}

func checkLines(r preflight.Report) []report.CheckLine {
	if len(r.Checks) == 0 {
		return nil
	}
	lines := make([]report.CheckLine, len(r.Checks))
	for i, c := range r.Checks {
		lines[i] = report.CheckLine{Name: c.Name, Severity: c.Severity.String(), Detail: c.Detail}
	}
	return lines
}

func optionDecisions(ds []ddl.Decision) []report.OptionDecision {
	if len(ds) == 0 {
		return nil
	}
	out := make([]report.OptionDecision, len(ds))
	for i, d := range ds {
		out[i] = report.OptionDecision{Option: d.Option, Value: d.Value, Reason: d.Reason}
	}
	return out
}

func opTarget(op ddl.Operation) string {
	return op.Target().String()
}

// shrinkReport maps the driver's per-file results into the run report.
func shrinkReport(results []ShrinkResult) []report.ShrinkFileReport {
	if len(results) == 0 {
		return nil
	}
	out := make([]report.ShrinkFileReport, len(results))
	for i, r := range results {
		out[i] = report.ShrinkFileReport{
			File:      r.File,
			Type:      r.Type,
			InitialMB: r.InitialMB,
			FinalMB:   r.FinalMB,
			GainedMB:  r.InitialMB - r.FinalMB,
			Chunks:    r.Chunks,
			NoOp:      r.NoOp,
			Reason:    r.Reason,
		}
	}
	return out
}

// cancelSafe reports whether canceling op under pressure is a clean stop with no
// expensive rollback: a REORGANIZE commits incrementally, DBCC CHECKDB is a
// read-only snapshot, and UPDATE STATISTICS rolls back cheaply. Heavy builders
// (REBUILD index/heap, CREATE INDEX, ALTER COLUMN, ADD CONSTRAINT) are not.
func cancelSafe(op ddl.Operation) bool {
	switch op.(type) {
	case ddl.ReorganizeIndex, ddl.CheckDB, ddl.UpdateStatistics:
		return true
	default:
		return false
	}
}

// isInterruption reports whether a reaction kind stops the running statement, so
// the operation's completion percentage is worth recording.
func isInterruption(kind string) bool {
	switch kind {
	case "pause", "cancel", "abort", "kill":
		return true
	default:
		return false
	}
}

// operationPercent reads the running DDL's completion percentage, when a progress
// reader and session are configured and the request is still active.
func (e *Engine) operationPercent(ctx context.Context) (float64, bool) {
	if e.progress == nil || e.session == nil {
		return 0, false
	}
	p, found, err := e.progress.Progress(ctx, e.session.SPID())
	if err != nil || !found {
		return 0, false
	}
	return p.PercentComplete, true
}

// snapshotWaits captures the session's cumulative waits before an operation, so
// the operation's own waits can be computed as a delta. Best-effort: returns nil
// when waits are unavailable (no reader, no session, or an unsupported server).
func (e *Engine) snapshotWaits(ctx context.Context) []mssql.SessionWait {
	if e.waits == nil || e.session == nil {
		return nil
	}
	w, err := e.waits.SessionWaits(ctx, e.session.SPID())
	if err != nil {
		return nil
	}
	return w
}

// operationWaits computes the per-operation wait categories as the delta from the
// before snapshot, and the total kept wait time.
func (e *Engine) operationWaits(ctx context.Context, before []mssql.SessionWait) ([]report.WaitLine, int64) {
	if e.waits == nil || e.session == nil {
		return nil, 0
	}
	after, err := e.waits.SessionWaits(ctx, e.session.SPID())
	if err != nil {
		return nil, 0
	}
	cats, total := mssql.CategorizeWaits(mssql.DiffWaits(before, after))
	if len(cats) == 0 {
		return nil, 0
	}
	lines := make([]report.WaitLine, len(cats))
	for i, c := range cats {
		lines[i] = report.WaitLine{
			Category:    c.Name,
			Description: c.Description,
			WaitMS:      c.WaitTimeMS,
			SignalMS:    c.SignalMS,
			Tasks:       c.Tasks,
		}
	}
	return lines, total
}

// ExpandAll replaces ALL-index rebuilds with per-index rebuilds, sourcing the
// concrete index list from the expander and mapping each index's storage type.
// It is exported so the CLI can render the expanded plan in a connected dry-run.
func ExpandAll(ctx context.Context, ex IndexExpander, m *ddl.Manifest) (*ddl.Manifest, error) {
	return ddl.ExpandRebuildAll(m, func(schema, table string) ([]ddl.IndexDescriptor, error) {
		idx, err := ex.RebuildableIndexes(ctx, schema, table)
		if err != nil {
			return nil, err
		}
		descs := make([]ddl.IndexDescriptor, len(idx))
		for i, x := range idx {
			descs[i] = ddl.IndexDescriptor{Name: x.Name, Clustered: x.IsClustered, Kind: indexKind(x.Type)}
		}
		return descs, nil
	})
}

// indexKind maps a sys.indexes.type to the resolution-relevant storage kind.
func indexKind(t int) ddl.IndexKind {
	switch t {
	case 1, 2:
		return ddl.KindRowstore
	case 3:
		return ddl.KindXML
	case 4:
		return ddl.KindSpatial
	case 5, 6:
		return ddl.KindColumnstore
	default:
		return ddl.KindOther
	}
}
