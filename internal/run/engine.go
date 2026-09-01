package run

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
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

// TempdbShrinkDriver runs a shrink_tempdb operation. *ShrinkRunner satisfies it;
// it is wired to a tempdb-scoped connection so its DBCC/reads run in tempdb context.
type TempdbShrinkDriver interface {
	RunTempdb(ctx context.Context, op ddl.ShrinkTempdb, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) ([]ShrinkResult, error)
}

var _ TempdbShrinkDriver = (*ShrinkRunner)(nil)

// BatchDMLDriver runs a batched UPDATE/DELETE, which like a shrink does not fit the
// one-statement OpRunner model: it loops a per-batch statement at run time. The
// engine routes ddl.BatchDML operations here, supplying a watermark store so a
// key_range walk resumes mid-table after a crash. *BatchDMLRunner satisfies it.
type BatchDMLDriver interface {
	Run(ctx context.Context, op ddl.BatchDML, res ddl.ResolvedOptions, ignore IgnoreSource, wm WatermarkStore, sink ReactionSink) (BatchDMLResult, error)
}

var _ BatchDMLDriver = (*BatchDMLRunner)(nil)

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

// ResumableAborter runs an ALTER INDEX … ABORT to clear a paused resumable operation,
// discarding its server-side progress. The engine uses it, when a manifest opts in with
// abort_blocking_resumable, to remove a stale/foreign paused resumable that would block a
// fresh REBUILD (SQL Server Msg 10637). *mssql.Conn satisfies it via ExecDDL.
type ResumableAborter interface {
	ExecDDL(ctx context.Context, sql string) error
}

var _ ResumableAborter = (*mssql.Conn)(nil)

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
// DDL was blocking when it reacted, for the advisory capture file, and the objects
// our own session currently holds a Sch-M lock on, for the shrink contended-object
// capture. *mssql.Conn satisfies it.
type BlockerReader interface {
	ActiveSessions(ctx context.Context) ([]mssql.Session, error)
	HeldObjectLocks(ctx context.Context, spid int) ([]mssql.LockedObject, error)
}

var _ BlockerReader = (*mssql.Conn)(nil)

// Summary counts the outcome of a ProcessAll run.
type Summary struct {
	Done        int
	Failed      int
	Incomplete  int // a shrink finished but stopped short of target (work preserved, re-runnable)
	Interrupted int // paused and left for recovery (session killed / connection lost)
	Deferred    int // manifests skipped this run because they were outside their window
	Failures    []ManifestFailure
}

// ManifestFailure records why a manifest failed, for the post-run summary and the TUI
// alert. Details carries the human-readable preflight FAIL line(s) when the failure is a
// preflight rejection (e.g. a shrink refused because the login lacks db_owner), so the
// operator sees the actionable reason without opening the .log; it is empty for other
// failures, whose reason is in Error.
type ManifestFailure struct {
	Manifest string
	Error    string
	Details  []string
}

// runOutcome is the result of processing one manifest.
type runOutcome int

const (
	outcomeDone runOutcome = iota
	outcomeFailed
	outcomeIncomplete
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
	tempdbShrink     TempdbShrinkDriver
	batchDML         BatchDMLDriver
	adr              bool
	rcsi             bool
	clk              Clock
	session          SessionInfo
	notifiers        []Notifier
	history          *report.History
	expander         IndexExpander
	progress         ProgressReader
	waits            WaitReader
	blockers         BlockerReader
	resumeCheck      ResumableProbe
	aborter          ResumableAborter // clears a blocking paused resumable (opt-in)
	reconnectTimeout time.Duration
	database         string                      // when set, process only manifests for this database
	liveReload       bool                        // re-read ignore_blocked_sessions from the manifest mid-run
	killer           *BlockerKiller              // when set, kills matching blockers per kill_blocking_sessions
	killDefaultAfter time.Duration               // default delay for a kill rule that sets none
	victims          *VictimKiller               // when set, kills amplifying maintenance victims we block
	victimPolicy     AmplifierPolicy             // the armed policy for that killer
	asyncStats       AsyncStatsSetting           // ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY on the target
	amplifierSink    func([]string)              // notified with the distinct conflicting jobs (TUI)
	manifestObserver func(path string)           // notified of the in-flight manifest path (TUI editing)
	holdPoll         time.Duration               // cadence for narrating held-through ignored sessions
	stepSink         func(StepEvent)             // manifest-level per-operation progress (stdout + TUI)
	opListSink       func([]OpInfo)              // full operation list, once per manifest (TUI operations panel)
	alertSink        func(ManifestFailure)       // notified when a manifest fails, so the TUI can show why
	compression      CompressionReader           // reads current index compression for the intent: compression skip
	drain            func() bool                 // reports a requested graceful stop (cancellable DrainFlag)
	serverClock      ServerClock                 // reads SQL Server local time for manifest windows
	checkpoint       func(context.Context) error // issues a CHECKPOINT between operations; nil = disabled
	out              io.Writer
	failures         []ManifestFailure // accumulated across the run, surfaced in Summary
}

// defaultHoldPoll is how often the engine narrates the ignored sessions it is holding
// the lock through, when a blocker reader is wired.
const defaultHoldPoll = 30 * time.Second

// EngineOption configures optional Engine behavior.
type EngineOption func(*Engine)

// Notifier delivers a run event to an external channel. Both *report.Notifier
// (webhook) and *report.EmailNotifier (email) satisfy it; the engine fans out to
// every wired notifier.
type Notifier interface {
	Notify(ctx context.Context, event string, payload map[string]any) error
}

// WithADR sets the target's Accelerated Database Recovery state (biases reactions).
func WithADR(adr bool) EngineOption { return func(e *Engine) { e.adr = adr } }

// WithRCSI sets the connected database's READ_COMMITTED_SNAPSHOT state. Used to warn
// before a reorganize_index whose page locks would block readers when RCSI is off.
func WithRCSI(rcsi bool) EngineOption { return func(e *Engine) { e.rcsi = rcsi } }

// WithShrinkRunner routes ddl.Shrink operations to the dedicated shrink driver
// instead of the OpRunner. Without it, a shrink operation falls back to the
// OpRunner, whose indicative PlannedOperation.SQL is not the real per-chunk SQL —
// so a shrink driver should always be wired when shrink manifests are expected.
func WithShrinkRunner(d ShrinkDriver) EngineOption { return func(e *Engine) { e.shrink = d } }

// WithTempdbShrinkRunner routes ddl.ShrinkTempdb operations to the dedicated tempdb
// shrink driver. Without it, a shrink_tempdb operation fails: it has no meaningful
// OpRunner fallback (there is no single indicative statement to run).
func WithTempdbShrinkRunner(d TempdbShrinkDriver) EngineOption {
	return func(e *Engine) { e.tempdbShrink = d }
}

// WithBlockerKiller arms the selective blocker-kill policy: the killer terminates sessions
// blocking this run's DDL that match the manifest's kill_blocking_sessions (after each rule's
// delay). defaultAfter seeds a rule that sets no delay. The same killer must be attached to
// the sampler (ServerSampler.SetKiller) so it is consulted on each blocking poll.
func WithBlockerKiller(k *BlockerKiller, defaultAfter time.Duration) EngineOption {
	return func(e *Engine) { e.killer = k; e.killDefaultAfter = defaultAfter }
}

// WithVictimKiller arms the amplifying-maintenance-victim kill: the killer terminates
// maintenance statements this run's operation blocks once other sessions have queued
// behind them for the policy's dwell. The same killer must be attached to the sampler
// (ServerSampler.SetVictimKiller) so it is consulted on each blocking poll.
func WithVictimKiller(k *VictimKiller, p AmplifierPolicy) EngineOption {
	return func(e *Engine) { e.victims = k; e.victimPolicy = p }
}

// WithAsyncStatsSetting supplies the target's ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY
// state, used for the pre-reorganize advisory.
func WithAsyncStatsSetting(s AsyncStatsSetting) EngineOption {
	return func(e *Engine) { e.asyncStats = s }
}

// WithAmplifierSink is notified with the distinct SQL Agent jobs whose statements this
// run has killed, whenever that set changes, and with nil at the end of each manifest.
func WithAmplifierSink(f func([]string)) EngineOption {
	return func(e *Engine) { e.amplifierSink = f }
}

// WithBatchDMLRunner routes ddl.BatchDML operations to the batch-DML driver instead
// of the OpRunner. Without it, a batch_update/batch_delete falls back to the
// OpRunner, whose indicative PlannedOperation.SQL is not the real per-batch SQL — so
// a batch-DML driver should always be wired when batch manifests are expected.
func WithBatchDMLRunner(d BatchDMLDriver) EngineOption { return func(e *Engine) { e.batchDML = d } }

// WithClock sets the clock (defaults to System).
func WithClock(c Clock) EngineOption { return func(e *Engine) { e.clk = c } }

// WithSession enables crash-recovery sidecars from the execution session.
func WithSession(s SessionInfo) EngineOption { return func(e *Engine) { e.session = s } }

// WithNotifier adds a notification channel (webhook or email). May be called once
// per channel; all wired notifiers receive every enabled event.
func WithNotifier(n Notifier) EngineOption {
	return func(e *Engine) { e.notifiers = append(e.notifiers, n) }
}

// WithHistory enables run-history persistence.
func WithHistory(h *report.History) EngineOption { return func(e *Engine) { e.history = h } }

// WithOutput sets the progress narration writer (defaults to io.Discard).
func WithOutput(w io.Writer) EngineOption { return func(e *Engine) { e.out = w } }

// WithStepSink registers a callback fed one StepEvent when each operation starts and
// another when it finishes, so stdout and the TUI can show manifest-level progress
// (op i/N, per-op timing, outcome). Independent of the text narration on WithOutput.
func WithStepSink(f func(StepEvent)) EngineOption { return func(e *Engine) { e.stepSink = f } }

// WithOpListSink receives the full operation list of each manifest once, before its
// operations run, so the TUI can show pending operations, not just the current one.
func WithOpListSink(f func([]OpInfo)) EngineOption { return func(e *Engine) { e.opListSink = f } }

// WithAlertSink registers a callback fed one ManifestFailure whenever a manifest fails,
// so the incident console can show the reason (notably a preflight rejection like a
// missing db_owner for a shrink) prominently instead of leaving it only in the .log.
func WithAlertSink(f func(ManifestFailure)) EngineOption { return func(e *Engine) { e.alertSink = f } }

// WithCompressionReader lets the engine honor a rebuild's intent: compression by
// reading an index's current compression and skipping a rebuild already at its target.
func WithCompressionReader(r CompressionReader) EngineOption {
	return func(e *Engine) { e.compression = r }
}

// WithDrainSignal wires a graceful-stop predicate (the DrainFlag's Draining method): once
// it reports true, the engine finishes the operation in flight, then stops before the next
// one — leaving the manifest in processing for the next run to resume — instead of aborting
// mid-operation. Because it is polled (not latched), a Cancel before the next check resumes
// normal processing.
func WithDrainSignal(fn func() bool) EngineOption { return func(e *Engine) { e.drain = fn } }

// WithCheckpointBetweenOperations makes the engine issue a CHECKPOINT after each
// operation that has another one behind it, backing the config key of the same name.
// The caller supplies the statement and decides whether it is worth issuing at all: a
// CHECKPOINT only releases log space under SIMPLE recovery, so the recovery-model gate
// lives at the wiring site, where the server's model is known.
//
// The key shipped in config.yaml and was documented in four places while being read by
// nothing — its struct field was its only appearance in the tree — so an operator who
// set it believed the log was released between the operations of a long manifest.
func WithCheckpointBetweenOperations(fn func(context.Context) error) EngineOption {
	return func(e *Engine) { e.checkpoint = fn }
}

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

// WithResumableAborter lets the engine clear a stale/foreign paused resumable that blocks
// a fresh REBUILD, when the manifest sets abort_blocking_resumable. Without it (or without
// the flag) such a conflict fails the operation with an actionable message instead.
func WithResumableAborter(a ResumableAborter) EngineOption { return func(e *Engine) { e.aborter = a } }

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
	e.failures = nil // fresh accumulation; the Engine may be run more than once
	if err := e.queue.EnsureDirs(); err != nil {
		return Summary{}, err
	}
	names, err := e.queue.Discover()
	if err != nil {
		return Summary{}, err
	}

	var sum Summary
	for _, name := range names {
		// A graceful stop requested between manifests: stop before starting the next
		// one (a manifest drained mid-flight is finalized inside processOne).
		if e.draining() {
			fmt.Fprintf(e.out, "-- drained — stopping before %s; remaining manifests left in queue\n", name)
			break
		}
		if !e.ownsManifest(name) {
			fmt.Fprintf(e.out, "skip %s: targets another database; left in queue (run is on %q)\n", name, e.database)
			continue
		}
		if e.deferredByWindow(ctx, name) {
			sum.Deferred++
			continue
		}
		switch e.processOne(ctx, name) {
		case outcomeDone:
			sum.Done++
		case outcomeIncomplete:
			sum.Incomplete++
		case outcomeInterrupted:
			sum.Interrupted++
		default:
			sum.Failed++
		}
	}
	sum.Failures = e.failures
	return sum, nil
}

// failLines returns the human-readable detail of each preflight check that failed,
// prefixed with the check name (e.g. "permissions: shrink/check_db require db_owner …").
// It is what the TUI alert and the post-run summary show so the operator sees the
// actionable reason for a preflight rejection without opening the .log.
func failLines(checks []report.CheckLine) []string {
	var out []string
	for _, c := range checks {
		if c.Severity == "FAIL" {
			out = append(out, c.Name+": "+c.Detail)
		}
	}
	return out
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

// deferredByWindow reports whether a manifest waiting in to_run should be skipped
// this run because its server-time window is closed. It is conservative: a manifest
// with a window whose server-clock read fails is deferred (never run at an unknown
// time). A manifest that cannot be loaded is not deferred here — processOne surfaces
// the load error.
func (e *Engine) deferredByWindow(ctx context.Context, name string) bool {
	m, err := ddl.LoadManifestFile(filepath.Join(e.dirs.ToRun, name))
	if err != nil || m.Window == nil {
		return false
	}
	open, err := e.windowOpen(ctx, m.Window)
	if err != nil {
		fmt.Fprintf(e.out, "-- defer %s: window not evaluated (%v) — left in queue\n", name, err)
		return true
	}
	if !open {
		fmt.Fprintf(e.out, "-- defer %s: outside window %s–%s %s — left in queue\n", name, m.Window.Start, m.Window.End, strings.Join(m.Window.Days, ","))
		return true
	}
	return false
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
	// The cursor is non-zero when a previous drained/interrupted run of this manifest
	// left one: those operations are already done and are skipped below. resumed is true
	// when a sidecar already existed (this run continues an interrupted one), gating the
	// ALTER INDEX … RESUME of a paused resumable at the boundary operation.
	st, resumed := e.writeSidecar(ctx, name)
	resumeFrom := st.ResumeFromOp

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

	// A windowed manifest resumed while already outside its window stops before
	// preflight — nothing to do until the window reopens. A clock error is treated
	// conservatively as closed.
	if open, err := e.windowOpen(ctx, manifest.Window); err != nil || !open {
		return e.finalizeWindowClosed(ctx, name, rep, start, windowStopReason(err, "before this run's operations"), nil)
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

	// A manifest naming its own database is resolved against that one, not against
	// whatever the connection happens to sit in: some options are database-scoped.
	planned, err := ddl.Plan(manifest, e.target.InDatabase(manifest.Database), e.matrix, e.policy)
	if err != nil {
		rep.Error = "plan: " + err.Error()
		return e.finalize(ctx, name, rep, start, false)
	}

	// Surface the whole operation list once, so the console can show pending operations
	// (not just the running one). Per-op status then follows from the step events.
	if e.opListSink != nil {
		ops := make([]OpInfo, len(planned))
		for i, step := range planned {
			ops[i] = OpInfo{Index: i + 1, Command: step.Operation.CommandType(), Target: opTarget(step.Operation)}
		}
		e.emitOpList(ops)
	}

	// Validate the resume cursor against the current plan: a cursor past the plan length, or a
	// plan that no longer matches the fingerprint the cursor was recorded against, means the
	// manifest changed since it was interrupted — restart clean rather than silently skip
	// operations (which would report SUCCESS having executed nothing).
	resumeFrom = e.reconcileResumePlan(name, st, planned, resumeFrom, resumed)

	// Sessions the operator allows to stay blocked, applied to every operation in the
	// manifest. The regexps were validated at load, so an error here is defensive.
	ignore, err := e.ignoreSource(name, manifest.IgnoreBlockedSessions)
	if err != nil {
		rep.Error = "compile ignore_blocked_sessions: " + err.Error()
		return e.finalize(ctx, name, rep, start, false)
	}

	// Arm the blocker-killer for this manifest (kill_blocking_sessions), if wired. The
	// killer is shared with the sampler; disarm it when this manifest is done so a later
	// manifest without kill rules does not act on stale ones.
	if e.killer != nil {
		killSrc, kerr := e.killSource(name, manifest.KillBlockingSessions)
		if kerr != nil {
			rep.Error = "compile kill_blocking_sessions: " + kerr.Error()
			return e.finalize(ctx, name, rep, start, false)
		}
		e.killer.SetSource(killSrc)
		defer e.killer.SetSource(nil)
	}

	// Arm the amplifying-victim killer for this manifest, if wired. Disarm when the
	// manifest is done so a later manifest does not inherit its episode state, and
	// clear the TUI's conflicting-jobs line at the same moment.
	if e.victims != nil {
		e.victims.Arm(e.victimPolicy)
		defer func() {
			e.victims.Disarm()
			if e.amplifierSink != nil {
				e.amplifierSink(nil)
			}
		}()
	}

	r := &manifestRun{
		name:       name,
		manifest:   manifest,
		planned:    planned,
		rep:        rep,
		start:      start,
		st:         st,
		resumeFrom: resumeFrom,
		ignore:     ignore,
		captured:   &blockerCapture{},
		contended:  &contendedCapture{},
		amplifiers: &amplifierCapture{},
		// cursor is the crash-resume watermark: the number of leading operations durably
		// done. It is advanced and persisted after each completed operation, so a crash —
		// not just a drain — resumes at the next operation instead of replaying.
		cursor: resumeFrom,
	}
	for i, step := range planned {
		if out := e.runStep(ctx, r, i, step); out != nil {
			return *out
		}
	}
	return e.finalizeAll(ctx, name, manifest, rep, start, r.failedOps)
}

// checkpointBetween issues the configured CHECKPOINT between two operations. A failure
// is reported and swallowed: a CHECKPOINT releases log space, it is not part of the work
// the manifest was asked to do, so refusing one is no reason to fail a manifest whose
// operations succeeded.
func (e *Engine) checkpointBetween(ctx context.Context, i, total int) {
	// Only after an operation that actually ran, and only with another behind it. A
	// skipped operation — before the resume cursor, or satisfied by intent: compression —
	// wrote no log, so there is nothing to release; checkpointing there would open a
	// resumed 200-operation manifest with 190 round trips before any work.
	if e.checkpoint == nil || i >= total-1 {
		return
	}
	if err := e.checkpoint(ctx); err != nil {
		fmt.Fprintf(e.out, "-- checkpoint between operations failed: %v (continuing)\n", err)
	}
}

// manifestRun is the state one manifest carries across its operations: what the
// report has accumulated, how far the resume cursor has advanced, which operations
// were quarantined, and the captures the reaction sink writes into. It exists so the
// per-operation body can live in runStep without a dozen parameters.
type manifestRun struct {
	name       string
	manifest   *ddl.Manifest
	planned    []ddl.PlannedOperation
	rep        *report.RunReport
	start      time.Time
	st         State
	resumeFrom int
	cursor     int
	ignore     IgnoreSource
	failedOps  []ddl.Operation
	captured   *blockerCapture
	contended  *contendedCapture
	amplifiers *amplifierCapture
}

// endRun marks an outcome as terminal for the manifest, so runStep's nil return can
// mean "carry on" without ever handing back an outcome the caller must know to ignore.
func endRun(o runOutcome) *runOutcome { return &o }

// runStep runs one planned operation of r. It returns nil when the manifest should
// carry on to the next operation, and the manifest's final outcome when this operation
// ended the run: drained, window closed, interrupted, failed without continue-on-failure,
// or a shrink that stopped short of its target.
func (e *Engine) runStep(ctx context.Context, r *manifestRun, i int, step ddl.PlannedOperation) *runOutcome {
	// Graceful stop: the operation in flight has finished (we are at the top of the
	// loop); stop before starting the next one and leave the manifest for recovery.
	if e.draining() {
		return endRun(e.finalizeDrained(ctx, r.name, r.rep, r.start, r.cursor, len(r.planned), r.failedOps))
	}
	opStart := e.clk.Now()
	caps := Capabilities{Resumable: step.Options.Resumable, ADR: e.adr, CancelSafe: cancelSafe(step.Operation), IgnoreBlocking: step.Options.IgnoreBlocking, Ignore: r.ignore, MaxBlock: blockCap(step.Options.MaxBlockMinutes), Stop: e.drain}

	// Manifest-level progress: the started event is emitted now; a finished event
	// derived from it is emitted at each terminal outcome below.
	stepEv := StepEvent{Index: i + 1, Total: len(r.planned), Command: step.Operation.CommandType(), Target: opTarget(step.Operation), StartedAt: opStart}

	// Resume cursor: operations before it were completed in a previous drained or
	// interrupted run of this manifest, so skip them (near-zero cost) — before the
	// window check, so a resumed manifest does not read the server clock once per
	// already-done operation.
	if i < r.resumeFrom {
		r.rep.Operations = append(r.rep.Operations, e.recordSkipped(stepEv, step, opStart, resumeSkipReason))
		return nil // carry on to the next operation
	}
	if open, err := e.windowOpen(ctx, r.manifest.Window); err != nil || !open {
		return endRun(e.finalizeWindowClosed(ctx, r.name, r.rep, r.start, windowStopReason(err, fmt.Sprintf("after operation %d/%d", r.cursor, len(r.planned))), r.failedOps))
	}
	// intent: compression — a rebuild whose target compression already holds is a no-op,
	// unless this operation left its own paused resumable, which must be resumed/finished
	// rather than skipped (skipping would orphan it paused on the server). Emitting only
	// the finished event keeps a re-run's log to one line per skipped operation.
	if reason, skip := e.skipSatisfied(ctx, r.manifest.Intent, step.Operation); skip && !ownsPausedResumable(r.st.Paused, i, step.Operation) {
		r.rep.Operations = append(r.rep.Operations, e.recordSkipped(stepEv, step, opStart, reason))
		e.advanceCursor(r.name, &r.cursor, i)
		return nil // carry on to the next operation
	}
	e.emitStep(stepEv)

	// The sink is called from the runner (this goroutine) and from the held-through
	// narrator (a sibling goroutine), so the shared report state is mutex-guarded.
	var (
		reactions   []report.ReactionLine
		peakBlocked int
		reactionMu  sync.Mutex
	)
	sink := func(ev ReactionEvent) {
		if ev.Tail != nil {
			e.captureTail(r.contended, r.name, r.manifest.Database, *ev.Tail)
		}
		if ev.Amplifier != nil {
			r.amplifiers.add(*ev.Amplifier, e.now())
			e.flushAmplifiers(r.name, r.amplifiers)
			if e.amplifierSink != nil {
				e.amplifierSink(r.amplifiers.jobs())
			}
		}
		detail := ev.Detail
		if isInterruption(ev.Kind) {
			if pct, ok := e.operationPercent(ctx); ok {
				detail = fmt.Sprintf("%s (at %.0f%%)", detail, pct)
			}
		}
		// On a yield/escalation, snapshot the sessions we are blocking: fold the
		// count into the narration and track the operation's peak for the report.
		capture := ev.Kind == "pause" || ev.Kind == "cancel" || ev.Kind == "abort"
		var blocked int
		if capture {
			blocked = e.captureBlockers(ctx, r.ignore, r.captured, r.name)
			if blocked > 0 {
				if _, isShrink := step.Operation.(ddl.Shrink); isShrink && e.session != nil {
					e.captureContended(ctx, e.session.SPID(), r.contended, r.name, r.manifest.Database)
				}
				detail = fmt.Sprintf("%s; blocking %d session(s)", detail, blocked)
			}
		}
		reactionMu.Lock()
		reactions = append(reactions, report.ReactionLine{Kind: ev.Kind, At: e.now(), Detail: detail})
		fmt.Fprintf(e.out, "-- %s %s: %s\n", ev.Kind, opTarget(step.Operation), detail)
		if blocked > peakBlocked {
			peakBlocked = blocked
		}
		reactionMu.Unlock()
		if capture {
			e.notify(ctx, ev.Kind, r.name, fmt.Sprintf("%s on %s (%s)", ev.Kind, opTarget(step.Operation), detail))
		}
	}
	// The victim killer emits kills on this operation's sink, from the pump
	// goroutine. Each iteration overwrites it before any kill can occur, and
	// Disarm clears it at the end of the manifest.
	e.victims.SetSink(sink)
	// The blocker killer emits its cap escalation on the same per-operation sink,
	// from the same pump goroutine, and is cleared by SetSource(nil) at the end of
	// the manifest.
	e.killer.SetSink(sink)
	waitsBefore := e.snapshotWaits(ctx)

	// Narrate, once each, the ignored sessions we hold the lock through, so the run
	// log shows we are deliberately blocking them (otherwise it is a silent non-event).
	holdCtx, stopHold := context.WithCancel(ctx)
	holdDone := make(chan struct{})
	go func() { defer close(holdDone); e.narrateHeld(holdCtx, r.ignore, sink) }()

	// Resumable conflict handling before running the operation:
	//   - an operation this manifest recorded as leaving its own paused resumable
	//     continues with ALTER INDEX … RESUME (reusing the server-side progress) instead
	//     of a fresh REBUILD, which SQL Server rejects while a resumable is paused;
	//   - otherwise a stale/foreign paused resumable on the target index would block a
	//     fresh REBUILD (Msg 10637): clear it with ABORT when the manifest opts in, else
	//     fail the operation early with an actionable message.
	// Ownership is matched by the recorded identity (op index + target), not the cursor
	// position, so a continue-on-failure gap that freezes the cursor before the paused op
	// no longer misclassifies the manifest's own paused resumable as foreign — and a
	// foreign paused resumable is never adopted, whatever the cursor.
	stmt := step.SQL
	var prepErr error
	switch {
	case ownsPausedResumable(r.st.Paused, i, step.Operation):
		// Our own paused resumable from a previous run: continue it whatever the current
		// resolve says about resumability (a fresh REBUILD would be rejected while it is
		// paused). If nothing is actually paused now, resumeStatement declines and the
		// planned REBUILD runs (a clean restart).
		if resume, ok := e.resumeStatement(ctx, step.Operation); ok {
			stmt = resume
			sink(ReactionEvent{Kind: "resume", Detail: "continuing paused resumable rebuild (server-side progress kept)"})
		}
	case e.blockingResumable(ctx, step.Operation):
		prepErr = e.clearOrRejectBlockingResumable(ctx, step.Operation, r.manifest.AbortBlockingResumable)
	}

	var (
		runErr        error
		shrinkResults []ShrinkResult
		batchResult   *BatchDMLResult
	)
	if prepErr != nil {
		runErr = prepErr
	} else {
		// Advisory: a reorganize_index against an RCSI-off database blocks readers on
		// its page locks. Emitted through the sink so it lands in the run's .log and TUI.
		// manifest.Database is empty for a no-database manifest, which runs on the
		// engine's connected database (e.database). The helper self-gates to reorg only.
		db := r.manifest.Database
		if db == "" {
			db = e.database
		}
		if msg, ok := reorgRCSIWarning(step.Operation, db, e.rcsi); ok {
			sink(ReactionEvent{Kind: "warn", Detail: msg})
		}
		if msg, ok := asyncStatsAdvisory(step.Operation, db, e.asyncStats); ok {
			sink(ReactionEvent{Kind: "warn", Detail: msg})
		}
		switch op := step.Operation.(type) {
		case ddl.Shrink:
			if e.shrink != nil {
				// Shrink is multi-statement and built at run time; route to the driver,
				// passing the resolved options (only WALP is meaningful for a shrink).
				shrinkResults, runErr = e.shrink.Run(ctx, op, step.Options, r.ignore, sink)
			} else {
				runErr = e.runner.Run(ctx, step.Operation, step.SQL, caps, sink)
			}
		case ddl.ShrinkTempdb:
			if e.tempdbShrink != nil {
				// shrink_tempdb is likewise a multi-statement, run-time-built loop; route
				// to the tempdb driver rather than the OpRunner.
				shrinkResults, runErr = e.tempdbShrink.RunTempdb(ctx, op, step.Options, r.ignore, sink)
			} else {
				runErr = fmt.Errorf("shrink_tempdb requires a tempdb shrink runner (not configured)")
			}
		case ddl.BatchDML:
			if e.batchDML != nil {
				// Batched DML is a per-batch loop built at run time; route to its driver.
				// The watermark sidecar lets a key_range walk resume after a crash; it is
				// removed once the walk completes (a crash — or a graceful stop, which
				// returns ErrStopped — skips that, preserving resume).
				store := e.watermarkStore(r.name, i)
				var br BatchDMLResult
				br, runErr = e.batchDML.Run(ctx, op, step.Options, r.ignore, store, sink)
				batchResult = &br
				// Clear the watermark only on true completion; on any failure keep it so a
				// manual re-run resumes mid-table (a graceful stop and a crash already
				// preserve it — the walk is idempotent, so a resume never double-applies).
				// Not on a stopped-short run either: it returns a nil error, so clearing on
				// that alone deleted the resume point of a walk that had abandoned most of
				// its rows, making it unresumable as well as misreported.
				if op.Batch.IsKeyRange() && runErr == nil && !batchStoppedShort(batchResult) {
					store.clear()
				}
			} else {
				runErr = e.runner.Run(ctx, step.Operation, step.SQL, caps, sink)
			}
		default:
			runErr = e.runner.Run(ctx, step.Operation, stmt, caps, sink)
		}
	}
	// Stop the narrator; it is joined, so it appends nothing past this point. The
	// pump goroutine is not joined (monitored_runner.go only stops sampling), and
	// the victim killer emits kills on this same sink from it — a kill decided just
	// before the statement finished can still be mid-flight here. So the report
	// state is read under reactionMu, on a copy, exactly as sink() writes it.
	stopHold()
	<-holdDone
	waitLines, waitTotal := e.operationWaits(ctx, waitsBefore)
	reactionMu.Lock()
	opReactions := append([]report.ReactionLine(nil), reactions...)
	opPeakBlocked := peakBlocked
	reactionMu.Unlock()

	opRep := report.OperationReport{
		Index:          i + 1,
		CommandType:    step.Operation.CommandType(),
		Target:         opTarget(step.Operation),
		SQL:            stmt, // the statement actually executed (RESUME when continuing a paused resumable)
		Options:        optionDecisions(step.Decisions),
		Reactions:      opReactions,
		PeakBlocked:    opPeakBlocked,
		ContendedCount: r.contended.len(),
		Waits:          waitLines,
		WaitTotalMS:    waitTotal,
		Shrink:         shrinkReport(shrinkResults),
		BatchDML:       batchDMLReport(batchResult),
		DurationMS:     e.msSince(opStart),
	}
	if opRep.ContendedCount > 0 {
		opRep.ContendedFile = r.name + contendedCaptureSuffix
	}
	if runErr != nil {
		opRep.Error = runErr.Error()
		// A resumable operation left PAUSED is a clean interruption, not a failure:
		// keep the manifest and sidecar in processing so the next run continues it via
		// ALTER INDEX … RESUME. Two ways to get here: an operator graceful stop
		// (ErrStopped), or a session loss / server restart that resumableInterruption
		// confirms left a paused resumable. A pre-run rejection (prepErr, a blocking
		// resumable the manifest did not opt in to clear) is a deterministic failure and
		// is excluded from the resumableInterruption path.
		stopped := errors.Is(runErr, ErrStopped)
		if stopped || (prepErr == nil && caps.Resumable && e.resumableInterruption(ctx, step.Operation)) {
			// Record which operation left its own paused resumable, so the next run resumes
			// it by identity rather than cursor position. Only a resumable index rebuild
			// leaves one; shrink/batch ErrStopped resume differently (free-space/watermark)
			// and have caps.Resumable false.
			if caps.Resumable {
				ref := step.Operation.Target()
				rec := &PausedResumable{Op: i, Schema: ref.Schema, Table: ref.Table, Index: ref.Name}
				e.updateSidecar(r.name, func(s *State) { s.Paused = rec })
			}
			opRep.Outcome = "interrupted"
			e.emitStep(stepEv.finished("interrupted", opDuration(opRep)))
			r.rep.Operations = append(r.rep.Operations, opRep)
			if stopped {
				r.rep.Error = fmt.Sprintf("operation %d (%s) interrupted by a graceful stop — resumes on the next run", i, step.Operation.CommandType())
			} else {
				r.rep.Error = fmt.Sprintf("operation %d (%s) interrupted; paused and recoverable: %v", i, step.Operation.CommandType(), runErr)
			}
			return endRun(e.finalizeInterrupted(ctx, r.name, r.rep, r.start))
		}
		opRep.Outcome = "failed"
		e.emitStep(stepEv.finished("failed", opDuration(opRep)))
		r.rep.Operations = append(r.rep.Operations, opRep)
		if !r.manifest.Continue() {
			r.rep.Error = fmt.Sprintf("operation %d (%s): %v", i, step.Operation.CommandType(), runErr)
			return endRun(e.finalize(ctx, r.name, r.rep, r.start, false))
		}
		// continue-on-failure: quarantine the failed op and keep going.
		r.failedOps = append(r.failedOps, step.Operation)
		fmt.Fprintf(e.out, "-- continue-on-failure: operation %d (%s) failed, quarantined: %v\n", i, step.Operation.CommandType(), runErr)
		e.checkpointBetween(ctx, i, len(r.planned))
		return nil // carry on to the next operation
	}
	// A shrink can finish without error yet stop short of target (it stalled or timed
	// out with work preserved). That is not a clean success: record it as a distinct
	// INCOMPLETE outcome so it is never mistaken for done, and route the manifest to
	// failed for review. Terminal for the manifest (like a non-continue failure).
	if isShrinkOp(step.Operation) && shrinkStoppedShort(shrinkResults) {
		opRep.Outcome = "incomplete"
		e.emitStep(stepEv.finished("incomplete", opDuration(opRep)))
		r.rep.Operations = append(r.rep.Operations, opRep)
		r.rep.Error = fmt.Sprintf("operation %d (%s): stopped short of target, work preserved — %s",
			i, step.Operation.CommandType(), shrinkShortReason(shrinkResults))
		return endRun(e.finalizeIncomplete(ctx, r.name, r.rep, r.start))
	}
	// A batch-DML operation stops the same way: log pressure, blocking, or the self-wait
	// budget end the loop with its committed batches preserved and a Reason, but no error.
	// That reached the engine as a clean success, so a purge that abandoned most of its
	// rows was finalized into done/ — and an operator draining a queue from cron saw a
	// completed operation. Same verdict as the shrink, same path.
	if batchStoppedShort(batchResult) {
		opRep.Outcome = "incomplete"
		e.emitStep(stepEv.finished("incomplete", opDuration(opRep)))
		r.rep.Operations = append(r.rep.Operations, opRep)
		r.rep.Error = fmt.Sprintf("operation %d (%s): stopped before the predicate was exhausted, work preserved — %s",
			i, step.Operation.CommandType(), batchResult.Reason)
		return endRun(e.finalizeIncomplete(ctx, r.name, r.rep, r.start))
	}
	opRep.Outcome = "success"
	e.emitStep(stepEv.finished("success", opDuration(opRep)))
	r.rep.Operations = append(r.rep.Operations, opRep)
	// This operation completed, so clear any paused-resumable record it carried (a RESUME
	// that finished): the server no longer holds it.
	if r.st.Paused != nil && r.st.Paused.Op == i {
		e.updateSidecar(r.name, func(s *State) { s.Paused = nil })
		r.st.Paused = nil
	}
	e.advanceCursor(r.name, &r.cursor, i)
	e.checkpointBetween(ctx, i, len(r.planned))
	return nil
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

// killSource builds the run's kill-rule source from the manifest rules, mirroring
// ignoreSource: live reload re-reads the in-processing manifest each blocking poll (so a
// rule added mid-run — by hand or the TUI — is honored without a restart); otherwise it is
// a fixed snapshot. Rules are validated at load, so a compile error here is defensive.
func (e *Engine) killSource(name string, rules []ddl.KilledSession) (KillSource, error) {
	compiled, err := compileKilledSessions(rules, e.killDefaultAfter)
	if err != nil {
		return nil, err
	}
	if e.liveReload {
		return newManifestKillSource(filepath.Join(e.dirs.Processing, name), e.killDefaultAfter, compiled), nil
	}
	return staticKill{rules: compiled}, nil
}

// stopVictims stops the amplifying-victim killer for the manifest that is finishing.
// Disarm clears the sink as well as the episodes, so no kill decided after this point
// can reach the finished operation's sink — which would re-create the .amplifiers.yaml
// sidecar in processing right after relocateCaptures moved it, leaving a file nothing
// ever cleans up. It must therefore run BEFORE any relocation: processOne's
// `defer e.victims.Disarm()` runs after finalize returns, far too late. Nil-receiver
// safe and idempotent, so every finalize path can call it and the defer still stands as
// a backstop.
func (e *Engine) stopVictims() { e.victims.Disarm() }

// finalize records a terminal outcome: moves the manifest, writes the report,
// notifies, and persists history.
func (e *Engine) finalize(ctx context.Context, name string, rep *report.RunReport, start time.Time, success bool) runOutcome {
	e.stopVictims()
	e.removeSidecar(name)
	e.removeInterimLog(name)
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
	e.relocateCaptures(name, dir)

	if err := report.WriteFile(filepath.Join(dir, name+".log"), *rep); err != nil {
		fmt.Fprintf(e.out, "write log %s: %v\n", name, err)
	}
	if !success {
		f := ManifestFailure{Manifest: name, Error: rep.Error, Details: failLines(rep.Preflight)}
		e.failures = append(e.failures, f)
		if e.alertSink != nil {
			e.alertSink(f)
		}
		e.notify(ctx, "fail", name, rep.Error)
	}
	e.record(ctx, *rep)

	fmt.Fprintf(e.out, "%s: %s\n", map[bool]string{true: "done", false: "failed"}[success], name)
	if success {
		return outcomeDone
	}
	return outcomeFailed
}

// isShrinkOp reports whether op is one of the shrink operation types (ddl.Shrink or
// ddl.ShrinkTempdb), which both drive a chunk loop that can stop short of target with
// work preserved rather than fail outright.
func isShrinkOp(op ddl.Operation) bool {
	switch op.(type) {
	case ddl.Shrink, ddl.ShrinkTempdb:
		return true
	default:
		return false
	}
}

// shrinkStoppedShort reports whether a completed shrink (no error) failed to reach
// target for at least one file — it stalled, hit a no-progress/self-wait bound, or a
// log-drain/reuse timeout, leaving work preserved. A no-op file (nothing to reclaim,
// target already satisfied) is not short; a file that reached target has no reason set.
func shrinkStoppedShort(results []ShrinkResult) bool {
	for _, r := range results {
		if !r.NoOp && r.Reason != "" {
			return true
		}
	}
	return false
}

// batchStoppedShort reports whether a batch-DML operation ended before its predicate
// was exhausted. The driver signals that with a Reason and a nil error — the batches it
// did commit are real and a re-run continues from them — so without this the engine
// could not tell it from a completed purge.
func batchStoppedShort(r *BatchDMLResult) bool { return r != nil && r.Reason != "" }

// shrinkShortReason returns the reason of the first file that stopped short, for the
// run report's error line. Empty only if no file stopped short (never called then).
func shrinkShortReason(results []ShrinkResult) string {
	for _, r := range results {
		if !r.NoOp && r.Reason != "" {
			return fmt.Sprintf("%s: %s", r.File, r.Reason)
		}
	}
	return ""
}

// finalizeIncomplete records a shrink that finished without reaching target: work is
// preserved and re-runnable, but the run is NOT a success. The manifest moves to failed
// (so it is never mistaken for done), the report is labeled INCOMPLETE, and it is counted
// distinctly in the run summary. It does not join e.failures — that list drives the
// "FAILED" echo and the failed exit code; an incomplete run is surfaced separately.
func (e *Engine) finalizeIncomplete(ctx context.Context, name string, rep *report.RunReport, start time.Time) runOutcome {
	e.stopVictims()
	e.removeSidecar(name)
	e.removeInterimLog(name)
	rep.FinishedAt = e.now()
	rep.DurationMS = e.msSince(start)
	rep.Outcome = "INCOMPLETE"

	if err := e.queue.Fail(name); err != nil {
		fmt.Fprintf(e.out, "fail %s: %v\n", name, err)
	}
	e.relocateCaptures(name, e.dirs.Failed)

	if err := report.WriteFile(filepath.Join(e.dirs.Failed, name+".log"), *rep); err != nil {
		fmt.Fprintf(e.out, "write log %s: %v\n", name, err)
	}
	// Surface it in the TUI (if still open) without labeling it a hard failure.
	if e.alertSink != nil {
		e.alertSink(ManifestFailure{Manifest: name, Error: rep.Error, Details: []string{"shrink stopped short of target; work preserved — re-run to continue"}})
	}
	e.notify(ctx, "incomplete", name, rep.Error)
	e.record(ctx, *rep)

	fmt.Fprintf(e.out, "incomplete: %s — shrink stopped short of target (work preserved); moved to failed/ for review\n", name)
	return outcomeIncomplete
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
	e.stopVictims()
	e.removeSidecar(name)
	e.removeInterimLog(name)
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
	e.relocateCaptures(name, e.dirs.Failed)

	// Copy the manifest so recovery-specific overrides are the only differences; this
	// carries forward every other setting (execution window, intent, …) that
	// a resubmitted recovery run must still honor.
	recovery := *m
	recovery.Description = recoveryDescription(m, name)
	recovery.OnFailure = ddl.OnFailureContinue
	recovery.IgnoreBlockedSessions = ignore
	recovery.KillBlockingSessions = e.latestKillRules(name, m)
	recovery.Operations = failed
	recName := name + ".recovery.yaml"
	rep.Error = fmt.Sprintf("%d of %d operation(s) failed; recovery manifest: %s", len(failed), len(rep.Operations), recName)
	if err := e.writeRecovery(filepath.Join(e.dirs.Failed, recName), &recovery); err != nil {
		fmt.Fprintf(e.out, "write recovery manifest %s: %v\n", recName, err)
	}

	if err := report.WriteFile(filepath.Join(e.dirs.Failed, name+".log"), *rep); err != nil {
		fmt.Fprintf(e.out, "write log %s: %v\n", name, err)
	}
	// A PARTIAL is counted as a failed manifest, so surface its reason like finalize does —
	// otherwise the run summary and the TUI alert would omit it (len(Failures) != Failed).
	f := ManifestFailure{Manifest: name, Error: rep.Error, Details: failLines(rep.Preflight)}
	e.failures = append(e.failures, f)
	if e.alertSink != nil {
		e.alertSink(f)
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

// latestKillRules returns the manifest's kill_blocking_sessions, preferring the current
// on-disk copy in processing (reflecting any mid-run TUI append) and falling back to the
// in-memory manifest — the kill-rule twin of latestIgnoreRules.
func (e *Engine) latestKillRules(name string, m *ddl.Manifest) []ddl.KilledSession {
	if latest, err := ddl.LoadManifestFile(filepath.Join(e.dirs.Processing, name)); err == nil {
		return latest.KillBlockingSessions
	}
	return m.KillBlockingSessions
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

// resumeSkipReason is the skip reason for an operation already completed in a previous run
// (resume cursor). It is distinct from a compression-intent skip reason so the history's
// skipped metric can exclude it — a resumed run re-marks its completed prefix skipped every cycle.
const resumeSkipReason = "already done in a previous run"

// recordSkipped builds the report entry for an operation skipped without running
// (resume cursor or a compression-intent skip) and emits its finished step event, so stdout,
// the TUI, and the .log all show it as skipped with the reason. It never emits a
// started event — a skip is instantaneous.
func (e *Engine) recordSkipped(stepEv StepEvent, step ddl.PlannedOperation, opStart time.Time, reason string) report.OperationReport {
	opRep := report.OperationReport{
		Index:       stepEv.Index,
		CommandType: step.Operation.CommandType(),
		Target:      opTarget(step.Operation),
		SQL:         step.SQL,
		Options:     optionDecisions(step.Decisions),
		Outcome:     "skipped",
		Detail:      reason,
		DurationMS:  e.msSince(opStart),
	}
	stepEv.Detail = reason
	e.emitStep(stepEv.finished("skipped", opDuration(opRep)))
	return opRep
}

// draining reports whether a graceful stop is currently requested. The signal is the
// cancellable DrainFlag predicate, polled at each operation boundary — so a Cancel before
// the next check resumes normal processing. No drain wired (nil predicate) is never draining.
func (e *Engine) draining() bool { return stopRequested(e.drain) }

// finalizeDrained records a graceful stop after `done` of `total` operations: the
// manifest stays in processing (not done/failed) so the next run resumes it. The resume
// cursor is already durable — advanceCursor persisted it after each completed operation —
// so this only finalizes the report; it does not re-write the cursor.
func (e *Engine) finalizeDrained(ctx context.Context, name string, rep *report.RunReport, start time.Time, done, total int, quarantined []ddl.Operation) runOutcome {
	return e.finalizeGracefulStop(ctx, name, rep, start, quarantined,
		fmt.Sprintf("drained after operation %d/%d — resumes on the next run", done, total),
		fmt.Sprintf("-- drained after operation %d/%d on %s — left in processing, resumes next run", done, total, name))
}

// finalizeGracefulStop records a graceful stop — a drain or a closed execution window —
// leaving the manifest in processing with its resume cursor so the next run continues it.
// errMsg goes to the report; logMsg is narrated to stdout.
func (e *Engine) finalizeGracefulStop(ctx context.Context, name string, rep *report.RunReport, start time.Time, quarantined []ddl.Operation, errMsg, logMsg string) runOutcome {
	rep.Error = errMsg + quarantinedNote(quarantined)
	e.recordInterrupted(ctx, name, rep, start)
	fmt.Fprintln(e.out, logMsg)
	return outcomeInterrupted
}

// quarantinedNote reports operations continue-on-failure quarantined before a graceful
// stop. They get no recovery manifest, deliberately: the manifest stays in processing and
// its cursor is frozen at the first gap, so the next run retries them where they are — a
// recovery manifest would run them a second time. Naming them in the report is what keeps
// the quarantine visible until then.
func quarantinedNote(quarantined []ddl.Operation) string {
	if len(quarantined) == 0 {
		return ""
	}
	return fmt.Sprintf("; %d operation(s) quarantined by continue-on-failure, retried on the next run", len(quarantined))
}

// finalizeInterrupted records a recoverable interruption: the manifest and its
// sidecar are LEFT in processing so the next run's crash recovery resumes the
// paused operation. No move and no sidecar removal happen here.
func (e *Engine) finalizeInterrupted(ctx context.Context, name string, rep *report.RunReport, start time.Time) runOutcome {
	e.recordInterrupted(ctx, name, rep, start)
	fmt.Fprintf(e.out, "interrupted: %s — paused, left in processing for recovery\n", name)
	return outcomeInterrupted
}

// recordInterrupted writes the shared bookkeeping for an interruption (report timestamps,
// INTERRUPTED outcome, report, notify, and history) without moving the manifest — it is
// left in processing. The caller sets rep.Error and prints its own log line.
func (e *Engine) recordInterrupted(ctx context.Context, name string, rep *report.RunReport, start time.Time) {
	// The manifest stays in processing, so nothing is relocated here — but the report is
	// written now, and a kill landing after it would be in the sidecar and not in the .log.
	e.stopVictims()
	rep.FinishedAt = e.now()
	rep.DurationMS = e.msSince(start)
	rep.Outcome = "INTERRUPTED"
	// The report goes next to the manifest in processing, where the manifest itself stays.
	// A run that only ever drains or runs out of window (the normal shape of a windowed
	// campaign) otherwise leaves no .log at all, and anything continue-on-failure
	// quarantined before the stop would exist only in the optional history. finalize
	// supersedes this with the terminal report once the manifest leaves processing.
	if err := report.WriteFile(filepath.Join(e.dirs.Processing, name+".log"), *rep); err != nil {
		fmt.Fprintf(e.out, "write log %s: %v\n", name, err)
	}
	e.notify(ctx, "interrupted", name, rep.Error)
	e.record(ctx, *rep)
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

// resumeStatement returns an ALTER INDEX … RESUME for op when the server already holds a
// paused resumable operation for its index — left by a crash or kill mid-rebuild — so a
// resumed run continues from the server-side progress instead of issuing a fresh REBUILD,
// which SQL Server rejects while a resumable is paused. ok is false when no probe is
// wired, when nothing is paused (or the probe errors), or when op does not support
// resumable control; the caller then runs the planned REBUILD.
func (e *Engine) resumeStatement(ctx context.Context, op ddl.Operation) (string, bool) {
	if e.resumeCheck == nil {
		return "", false
	}
	ref := op.Target()
	paused, err := e.resumeCheck.PausedResumable(ctx, ref.Schema, ref.Table, ref.Name)
	if err != nil || !paused {
		return "", false
	}
	stmt, err := ddl.ResumableControlSQL(op, "RESUME")
	if err != nil {
		return "", false
	}
	return stmt, true
}

// blockingResumable reports whether op's index currently holds a paused resumable that
// would block a fresh REBUILD (SQL Server Msg 10637). It is false for a non-index
// operation, when no probe is wired, or when nothing is paused.
func (e *Engine) blockingResumable(ctx context.Context, op ddl.Operation) bool {
	if e.resumeCheck == nil {
		return false
	}
	if _, err := ddl.ResumableControlSQL(op, "ABORT"); err != nil {
		return false // not an index operation
	}
	ref := op.Target()
	paused, err := e.resumeCheck.PausedResumable(ctx, ref.Schema, ref.Table, ref.Name)
	return err == nil && paused
}

// clearOrRejectBlockingResumable handles a paused resumable that blocks op's fresh
// REBUILD. With the manifest opt-in it ABORTs the stale operation — discarding its
// server-side progress so the rebuild proceeds with this manifest's own options — and
// returns nil. Without the opt-in (or with no aborter wired) it returns an actionable
// error so the operator resolves the conflict deliberately, since aborting is destructive
// on a shared/production database.
func (e *Engine) clearOrRejectBlockingResumable(ctx context.Context, op ddl.Operation, optIn bool) error {
	if !optIn {
		// Name the target: abort-resumable refuses a bare invocation, and an operator
		// arriving from this message should not have to work out the flags under pressure.
		ref := op.Target()
		return fmt.Errorf(
			"a paused resumable operation blocks this rebuild; resolve it with `sqlgopace abort-resumable --config <config> --table %s.%s --index %s --yes` "+
				"(preview it with --dry-run first; an aborted operation cannot be resumed), or set abort_blocking_resumable: true in the manifest",
			ref.Schema, ref.Table, ref.Name)
	}
	if e.aborter == nil {
		return fmt.Errorf("abort_blocking_resumable is set but no aborter is wired")
	}
	stmt, err := ddl.ResumableControlSQL(op, "ABORT")
	if err != nil {
		return err
	}
	if err := e.aborter.ExecDDL(ctx, stmt); err != nil {
		return fmt.Errorf("abort blocking resumable: %w", err)
	}
	fmt.Fprintf(e.out, "-- aborted a stale paused resumable on %s before rebuild\n", opTarget(op))
	return nil
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
	payload := map[string]any{"manifest": name, "detail": detail}
	for _, n := range e.notifiers {
		if err := n.Notify(ctx, event, payload); err != nil {
			fmt.Fprintf(e.out, "notify %s: %v\n", name, err)
		}
	}
}

func (e *Engine) record(ctx context.Context, rep report.RunReport) {
	if e.history == nil {
		return
	}
	peak, skipped := 0, 0
	for _, op := range rep.Operations {
		if op.PeakBlocked > peak {
			peak = op.PeakBlocked
		}
		// Count only compression-intent skips; resume-cursor skips (already done in a previous
		// run) are not "satisfied" skips and would otherwise inflate the metric each resume.
		if op.Outcome == "skipped" && op.Detail != resumeSkipReason {
			skipped++
		}
	}
	rec := report.RunRecord{
		Manifest:    rep.Manifest,
		Outcome:     rep.Outcome,
		StartedAt:   rep.StartedAt,
		FinishedAt:  rep.FinishedAt,
		Operations:  len(rep.Operations),
		DurationMS:  rep.DurationMS,
		PeakBlocked: peak,
		Skipped:     skipped,
		Error:       rep.Error,
	}
	if err := e.history.Record(ctx, rec); err != nil {
		fmt.Fprintf(e.out, "history %s: %v\n", rep.Manifest, err)
	}
}

// writeSidecar records fresh run state next to the manifest so a crash can be
// recovered. It also stamps a random CONTEXT_INFO marker on the execution session so
// an orphaned session can be correlated to its run beyond the reusable SPID. Any
// resume cursor and plan fingerprint left by a previous drained/interrupted run of this
// manifest are preserved and returned in st (the cursor is the number of operations already
// completed, to skip on this run). resumed reports whether such a sidecar already existed at
// claim — i.e. this run is resuming an interrupted one — which the caller uses when reconciling
// the cursor against the current plan and when handling a paused resumable. With no session it
// does not write, but still reads and returns the prior state and resumed flag.
func (e *Engine) writeSidecar(ctx context.Context, name string) (st State, resumed bool) {
	if prior, err := ReadState(e.sidecarPath(name)); err == nil {
		st, resumed = prior, true // a prior run of this manifest was interrupted
	}
	if e.session == nil {
		return st, resumed
	}
	login, err := e.session.LoginTime(ctx)
	if err != nil {
		fmt.Fprintf(e.out, "sidecar %s: login time: %v\n", name, err)
	}

	marker := e.stampMarker(ctx, name)

	// Re-stamp this run's session identity, but carry over the resume cursor, the plan
	// fingerprint, and any paused-resumable record left by a prior interrupted run, so a
	// resume still knows what is done, which plan the cursor was recorded against, and which
	// operation left its own paused resumable.
	fresh := State{
		Manifest:        name,
		Database:        e.database,
		SPID:            e.session.SPID(),
		LoginTime:       login,
		Marker:          marker,
		StartedAt:       e.now(),
		ResumeFromOp:    st.ResumeFromOp,
		PlanFingerprint: st.PlanFingerprint,
		Paused:          st.Paused,
	}
	if err := WriteState(e.sidecarPath(name), fresh); err != nil {
		fmt.Fprintf(e.out, "sidecar %s: %v\n", name, err)
	}
	return fresh, resumed
}

// advanceCursor records that operation i is durably done by moving the crash-resume
// cursor to i+1 and persisting it, so a crash (not just a drain) resumes at the next
// operation rather than replaying the manifest. It advances only when the cursor sits
// exactly at i: once continue-on-failure quarantines an operation, the cursor freezes
// at that gap, so a resumed run redoes the failed operation — and the idempotent ones
// after it — instead of skipping past a failure that never produced its effect.
func (e *Engine) advanceCursor(name string, cursor *int, i int) {
	if *cursor != i {
		return
	}
	next := i + 1
	*cursor = next
	e.updateSidecar(name, func(s *State) { s.ResumeFromOp = next })
}

// updateSidecar applies mutate to the manifest's sidecar state and rewrites it, in place
// (best-effort: a no-op when there is no sidecar to update). Used to record resume progress —
// the cursor, plan fingerprint, and paused-resumable record — after each operation.
func (e *Engine) updateSidecar(name string, mutate func(*State)) {
	st, err := ReadState(e.sidecarPath(name))
	if err != nil {
		return
	}
	mutate(&st)
	if err := WriteState(e.sidecarPath(name), st); err != nil {
		fmt.Fprintf(e.out, "sidecar %s: %v\n", name, err)
	}
}

// reconcileResumePlan validates the resume cursor against the current plan and (re)binds the
// plan fingerprint to the sidecar. A cursor past the plan length, or a plan whose fingerprint
// differs from the one the cursor was recorded against, means the manifest changed since it was
// interrupted (edited, re-expanded, or a stale same-name sidecar): the cursor is dropped and the
// run restarts from the first operation, sweeping any stale key_range watermarks. It returns the
// (possibly reset) cursor to use for this run.
func (e *Engine) reconcileResumePlan(name string, st State, planned []ddl.PlannedOperation, resumeFrom int, resumed bool) int {
	fp := planFingerprint(planned)
	if resumeFrom > 0 {
		mismatch := resumeFrom > len(planned) || (resumed && st.PlanFingerprint != "" && st.PlanFingerprint != fp)
		if mismatch {
			fmt.Fprintf(e.out, "-- resume cursor no longer matches the plan (%d planned, cursor %d); restarting from the first operation\n", len(planned), resumeFrom)
			resumeFrom = 0
			e.clearWatermarks(name)
		}
	}
	rf := resumeFrom
	e.updateSidecar(name, func(s *State) { s.ResumeFromOp = rf; s.PlanFingerprint = fp })
	return resumeFrom
}

// ownsPausedResumable reports whether the sidecar's paused-resumable record identifies operation
// i's exact target — i.e. this manifest left its own paused resumable here, so the run should
// RESUME it rather than treat it as a foreign blocker.
func ownsPausedResumable(p *PausedResumable, i int, op ddl.Operation) bool {
	if p == nil || p.Op != i {
		return false
	}
	ref := op.Target()
	return strings.EqualFold(p.Schema, ref.Schema) &&
		strings.EqualFold(p.Table, ref.Table) &&
		strings.EqualFold(p.Index, ref.Name)
}

// planFingerprint hashes the ordered identity (command + target) of the planned operations, so
// a resumed run can detect that the plan changed since the cursor was recorded — a manifest
// edited to fewer or reordered operations, an ALTER INDEX ALL that now expands to a different
// set, or a stale same-name sidecar — and restart cleanly instead of skipping operations.
func planFingerprint(planned []ddl.PlannedOperation) string {
	h := sha256.New()
	for _, s := range planned {
		fmt.Fprintf(h, "%s\x00%s\n", s.Operation.CommandType(), opTarget(s.Operation))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// clearWatermarks removes any key_range watermark sidecars for the manifest (name.opN.wm),
// used when a resume is invalidated so a fresh walk does not read a stale position.
func (e *Engine) clearWatermarks(name string) {
	matches, _ := filepath.Glob(filepath.Join(e.dirs.Processing, name+".op*.wm"))
	for _, m := range matches {
		_ = os.Remove(m)
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

// removeInterimLog drops the in-processing report left by an earlier interrupted run of
// this manifest, now that it has reached a terminal outcome and its final report is
// written next to it in done/failed. Absent (never interrupted) is the common case.
func (e *Engine) removeInterimLog(name string) {
	if err := os.Remove(filepath.Join(e.dirs.Processing, name+".log")); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(e.out, "remove interim log %s: %v\n", name, err)
	}
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

// batchDMLReport maps the driver's batch result into the run report.
func batchDMLReport(r *BatchDMLResult) *report.BatchDMLReport {
	if r == nil {
		return nil
	}
	return &report.BatchDMLReport{
		Verb:      r.Verb,
		Rows:      r.Rows,
		Batches:   r.Batches,
		FinalRows: r.FinalRows,
		Reason:    r.Reason,
	}
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
