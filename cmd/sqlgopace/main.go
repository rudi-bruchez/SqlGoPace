// Command sqlgopace runs demanding DDL operations against Microsoft SQL Server
// while monitoring their impact on locking and the transaction log.
//
// See specs/SPECS.md for the full behavior and specs/IMPLEMENTATION.md for the
// implementation plan. So far only offline planning (--dry-run / --explain) is
// wired, optionally connecting to detect the real target; execution and
// monitoring are added in later phases.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
	"github.com/rudi-bruchez/SqlGoPace/internal/tui"
	"github.com/rudi-bruchez/SqlGoPace/internal/version"
)

// sqlitePath strips an optional "sqlite://" prefix from a history destination.
func sqlitePath(dest string) string {
	return strings.TrimPrefix(dest, "sqlite://")
}

func main() {
	if err := cli(os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sqlgopace:", err)
		os.Exit(1)
	}
}

// cli is the testable entry point; main only wires it to the process streams.
func cli(stdout, stderr io.Writer, args []string) error {
	// Subcommands are dispatched before flag parsing; everything else (run /
	// dry-run) keeps the existing flag-based interface.
	if len(args) > 0 && args[0] == "abort-resumable" {
		return runAbortResumable(stdout, stderr, args[1:])
	}
	if len(args) > 0 && args[0] == "plan" {
		return runPlan(stdout, stderr, args[1:])
	}

	fs := flag.NewFlagSet("sqlgopace", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dryRun        = fs.Bool("dry-run", false, "render the final T-SQL without executing anything")
		explain       = fs.Bool("explain", false, "with --dry-run, show why each option was injected")
		configPath    = fs.String("config", "", "path to config.yaml (provides option policy, matrix path, and live connection)")
		assumeVersion = fs.Int("assume-version", 0, "target SQL Server major version for offline dry-run (e.g. 16 for 2022)")
		assumeEdition = fs.String("assume-edition", "enterprise", "target edition tier: enterprise, standard, express, azure")
		matrixPath    = fs.String("matrix", "ddl_compatibility.yaml", "path to the DDL compatibility matrix")
		useTUI        = fs.Bool("tui", false, "run with the interactive incident console")
		showVersion   = fs.Bool("version", false, "print version and exit")
		useAuto       = fs.Bool("auto", false, "analyze the database and run generated maintenance, unattended (no review)")
		autoProfile   = fs.String("profile", "maintenance_profile.yaml", "maintenance profile path (with --auto)")
		autoCats      = fs.String("categories", "", "comma-separated category subset (with --auto); default all")
		autoDatabase  = fs.String("database", "", "single-database --auto: the database to maintain (default the connected database)")
		autoAll       = fs.Bool("all-databases", false, "with --auto: maintain every eligible user database (spec §17)")
		autoDatabases = fs.String("databases", "", "with --auto: comma-separated list of databases to maintain")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if *showVersion {
		fmt.Fprintf(stdout, "sqlgopace %s\n", version.Version())
		return nil
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	visited := visitedFlags(fs)
	matrix, err := ddl.LoadFile(matrixFile(cfg, *configPath, *matrixPath, visited))
	if err != nil {
		return err
	}

	ctx := context.Background()
	if *useAuto && *dryRun {
		return errors.New("--auto runs maintenance; to preview it without running, use `sqlgopace plan --dry-run`")
	}
	if *dryRun {
		return dryRunAll(ctx, stdout, fs.Args(), visited, *assumeVersion, *assumeEdition, *explain, cfg, matrix)
	}

	if cfg == nil {
		return errors.New("run mode requires --config (connection, directories, policy); or pass --dry-run")
	}
	auto := autoConfig{
		enabled: *useAuto, profile: *autoProfile, categories: *autoCats,
		database: *autoDatabase, allDatabases: *autoAll, databases: *autoDatabases,
	}
	return runEngine(ctx, stdout, cfg, matrix, *useTUI, auto)
}

// autoConfig carries the --auto analysis settings into the run path.
type autoConfig struct {
	enabled      bool
	profile      string
	categories   string
	database     string // single-database --auto target
	allDatabases bool   // --auto --all-databases
	databases    string // --auto --databases a,b,c
}

// dryRunAll renders every manifest's planned T-SQL without executing it. When
// connected (not offline), it expands "ALTER INDEX ALL" rebuilds against the live
// server so the rendered plan matches what a real run would execute.
func dryRunAll(ctx context.Context, stdout io.Writer, manifests []string, visited map[string]bool, assumeVersion int, assumeEdition string, explain bool, cfg *config.Config, matrix *ddl.Matrix) error {
	if len(manifests) == 0 {
		return errors.New("no manifest files given")
	}
	target, expander, cleanup, err := dryRunSession(ctx, stdout, visited, assumeVersion, assumeEdition, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	policy := policyOf(cfg)
	for _, path := range manifests {
		if err := dryRunManifest(ctx, stdout, path, target, matrix, policy, explain, expander); err != nil {
			return err
		}
	}
	return nil
}

// runEngine connects, detects the target, and runs every queued manifest with
// monitoring and reaction. With useTUI, the interactive incident console runs in
// the foreground while the engine runs in the background. With auto.enabled, it
// first analyses the database and writes the generated maintenance manifests into
// the queue, then processes the queue — one unattended command, no review pause.
func runEngine(ctx context.Context, stdout io.Writer, cfg *config.Config, matrix *ddl.Matrix,
	useTUI bool, auto autoConfig) error {
	fmt.Fprintf(stdout, "-- sqlgopace %s\n", version.Version())
	conn, err := mssql.Open(ctx, cfg.Database.ConnectionString, version.Version())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	info, err := conn.Detect(ctx)
	if err != nil {
		return err
	}
	if !info.Supported() {
		return fmt.Errorf("unsupported engine edition %d", info.EngineEdition)
	}
	fmt.Fprintf(stdout, "-- target: tier=%s major=%d adr=%t recovery=%s rcsi=%t si=%t\n",
		info.Tier(), info.MajorVersion, info.ADREnabled, info.RecoveryModel, info.RCSIEnabled, info.SnapshotIsolation)

	// --auto: analyze and materialize maintenance manifests into the queue before
	// the engine processes it. Materializing (rather than running purely in memory)
	// means a crash mid-run leaves recoverable manifests + sidecars, like any run.
	if auto.enabled {
		if err := runAuto(ctx, stdout, conn, cfg, auto); err != nil {
			return err
		}
	}

	dirs := run.Dirs{
		ToRun:      cfg.Directories.ToRun,
		Processing: cfg.Directories.Processing,
		Done:       cfg.Directories.Done,
		Failed:     cfg.Directories.Failed,
	}

	// Crash recovery: reconcile any manifests left in processing before starting.
	// Each orphan is reconciled against a connection in the database it ran in
	// (recorded in its sidecar); other databases are reached via OpenDatabase.
	recoverer := run.NewRecoverer(dirs, conn, stdout,
		run.WithRecoveryProbes(info.Database, func(rctx context.Context, db string) (run.RecoveryProbe, func(), error) {
			c, oerr := mssql.OpenDatabase(rctx, cfg.Database.ConnectionString, db, version.Version())
			if oerr != nil {
				return nil, nil, oerr
			}
			return c, func() { _ = c.Close() }, nil
		}))
	if rsum, rerr := recoverer.Recover(ctx); rerr != nil {
		return rerr
	} else if rsum.Requeued > 0 || rsum.Adopted > 0 {
		fmt.Fprintf(stdout, "-- recovery: %d requeued, %d adopted\n", rsum.Requeued, rsum.Adopted)
	}

	engineOut := stdout
	if useTUI {
		engineOut = io.Discard // narration would corrupt the console; the TUI shows state
	}

	var history *report.History
	if cfg.History.Enabled {
		history, err = report.OpenHistory(sqlitePath(cfg.History.Destination))
		if err != nil {
			return err
		}
		defer func() { _ = history.Close() }()
	}

	// Run an engine per database the queue targets, each on a connection in that
	// database's context (spec §17.6), sequentially — at most one heavy DDL runs
	// server-wide at a time (§17.7).
	targets, err := queuedDatabases(dirs.ToRun, info.Database)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintln(stdout, "-- nothing to process")
		return nil
	}

	// Re-check eligibility for any non-connected target: a database that became an
	// AG secondary / read-only since `plan` is left in the queue, not failed (§17.6).
	if needsEligibilityRecheck(targets, info.Database) {
		eligible, eerr := conn.UserDatabases(ctx)
		if eerr != nil {
			return eerr
		}
		var skipped []dbSkip
		targets, skipped = runnableTargets(targets, info.Database, eligible)
		for _, s := range skipped {
			fmt.Fprintf(stdout, "-- skip database %s: %s; left in queue\n", s.name, s.reason)
		}
	}

	// Graceful stop: the first Ctrl+C drains — a running resumable operation is paused now
	// (its work preserved, resumed next run), a non-resumable one finishes, then the run
	// stops before the next operation. A second Ctrl+C cancels the run context for an
	// immediate hard stop. In TUI mode the console's "d" key drives the same drain.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	drain := &run.DrainFlag{}
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-runCtx.Done():
			return
		case <-sigCh:
			drain.Request()
			if !useTUI {
				fmt.Fprintln(stdout, "-- interrupt: draining — pausing a running resumable now (resumes next run) or finishing a non-resumable op, then stopping (Ctrl+C again to abort now)")
			}
		}
		select {
		case <-runCtx.Done():
		case <-sigCh:
			if !useTUI {
				fmt.Fprintln(stdout, "-- interrupt: stopping now")
			}
			cancelRun()
			signal.Stop(sigCh) // restore default SIGINT so a further Ctrl+C can force-quit a hung run
		}
	}()

	var total run.Summary
	var runErr error
	for _, db := range targets {
		// A graceful stop applies across the whole run: once drained, do not start the
		// remaining databases (each would otherwise open a connection only to stop at once).
		if drain.Draining() {
			if !useTUI {
				fmt.Fprintln(stdout, "-- drained: not starting the remaining databases")
			}
			break
		}
		dbConn, dbInfo, reused, cerr := connForDatabase(runCtx, conn, info, db, cfg.Database.ConnectionString)
		if cerr != nil {
			fmt.Fprintf(stdout, "-- skip database %s: %v\n", db, cerr)
			runErr = cerr
			continue
		}
		if !dbInfo.Supported() {
			if !reused {
				_ = dbConn.Close()
			}
			fmt.Fprintf(stdout, "-- skip database %s: unsupported engine edition %d\n", db, dbInfo.EngineEdition)
			continue
		}
		if len(targets) > 1 {
			fmt.Fprintf(stdout, "-- database %s\n", db)
		}

		var (
			current *currentManifest
			fwd     *tuiForwarder
			extra   []run.EngineOption
		)
		if useTUI {
			current = &currentManifest{}
			fwd = &tuiForwarder{}
			extra = append(extra, run.WithManifestObserver(current.set), run.WithAlertSink(fwd.alert))
		}
		engine := buildEngine(cfg, matrix, dbConn, dbInfo, dirs, engineOut, history, fwd, drain.Draining, extra...)
		var sum run.Summary
		if useTUI {
			banner := serverBanner(dbInfo, matrix)
			sum, err = runWithTUI(runCtx, dbConn, engine, current, fwd, drain, banner, cfg.Monitoring.ProgressPoll(), cfg.Monitoring.BlockingTimeout())
		} else {
			sum, err = engine.ProcessAll(runCtx)
		}
		if !reused {
			_ = dbConn.Close()
		}
		if err != nil {
			fmt.Fprintf(stdout, "-- database %s: %v\n", db, err)
			runErr = err
			continue
		}
		total.Done += sum.Done
		total.Failed += sum.Failed
		total.Incomplete += sum.Incomplete
		total.Interrupted += sum.Interrupted
		total.Deferred += sum.Deferred
		total.Failures = append(total.Failures, sum.Failures...)
	}

	fmt.Fprintf(stdout, "processed: %d done, %d failed, %d incomplete, %d interrupted, %d deferred\n",
		total.Done, total.Failed, total.Incomplete, total.Interrupted, total.Deferred)
	// The TUI closes when the run ends, so echo each failure's reason here — the console's
	// alert may have flashed by on a fast preflight rejection. This keeps the actionable
	// detail (e.g. missing db_owner for a shrink) on screen without opening the .log.
	for _, f := range total.Failures {
		fmt.Fprintf(stdout, "-- FAILED %s: %s\n", f.Manifest, f.Error)
		for _, d := range f.Details {
			fmt.Fprintf(stdout, "     %s\n", d)
		}
	}
	if total.Incomplete > 0 {
		fmt.Fprintf(stdout, "-- %d shrink manifest(s) incomplete (stopped short of target, work preserved); moved to failed/ for review — re-run to continue\n", total.Incomplete)
	}
	if total.Interrupted > 0 {
		fmt.Fprintf(stdout, "-- %d interrupted manifest(s) left in processing; the next run will resume them\n", total.Interrupted)
	}
	if total.Deferred > 0 {
		fmt.Fprintf(stdout, "-- %d manifest(s) deferred (outside their execution window); left in queue for a later run\n", total.Deferred)
	}
	if runErr != nil {
		return runErr
	}
	if total.Failed > 0 {
		return fmt.Errorf("%d manifest(s) failed", total.Failed)
	}
	return nil
}

// buildEngine wires a run.Engine for one database's connection, sharing the given
// history (may be nil) and reading policy, monitoring, and notification settings
// from cfg. It is called once per database in a multi-database run.
func buildEngine(cfg *config.Config, matrix *ddl.Matrix, conn *mssql.Conn, info mssql.ServerInfo, dirs run.Dirs, engineOut io.Writer, history *report.History, fwd *tuiForwarder, drain func() bool, extra ...run.EngineOption) *run.Engine {
	thresholds := preflight.Thresholds{
		LogMaxBytes:   cfg.Monitoring.LogMaxSizeBytes,
		LogMaxPercent: cfg.Monitoring.LogMaxPercent,
	}
	killArmed := cfg.KillBlockers.Enabled || cfg.OptionsOverride.AllowAbortBlockers
	checker := run.NewPreflightChecker(conn, info, thresholds, killArmed)
	sampler := run.NewServerSampler(conn, conn.SPID(), cfg.Monitoring.LogMaxSizeBytes, cfg.Monitoring.LogMaxPercent)
	// Selective blocker-kill policy (off unless armed in config). The killer reuses the
	// sampler's per-poll session snapshot; the engine feeds it each manifest's kill rules.
	var killOpt run.EngineOption
	if cfg.KillBlockers.Enabled {
		killed := func(ev run.KillEvent) {
			fmt.Fprintf(engineOut, "-- killed blocker SPID %d (login=%s) after %s blocking the DDL\n",
				ev.SPID, ev.Login, ev.Waited.Round(time.Second))
			if fwd != nil {
				fwd.send(tui.LogMsg{Line: fmt.Sprintf("killed blocker SPID %d (%s) after %s", ev.SPID, ev.Login, ev.Waited.Round(time.Second))})
			}
		}
		killer := run.NewBlockerKiller(conn.Kill, killed, nil)
		sampler.SetKiller(killer)
		killOpt = run.WithBlockerKiller(killer, cfg.KillBlockers.DefaultAfter())
	}
	runner := run.NewMonitoredRunner(conn, sampler, run.System, run.RunnerConfig{
		PollInterval:    cfg.Monitoring.BlockingPoll(),
		LogPollInterval: cfg.Monitoring.LogPoll(),
		BlockingTimeout: cfg.Monitoring.BlockingTimeout(),
		LogDrainTimeout: cfg.Monitoring.LogDrainTimeout(),
		KillGrace:       cfg.Monitoring.KillGrace(),
		MaxRetries:      cfg.Monitoring.MaxRetryAttempts,
	})
	// Progress goes to the TUI when the console is running, else to stdout/log. The
	// step sink follows the same rule (in TUI mode engineOut is io.Discard anyway).
	shrinkProgress := func(p run.ShrinkProgress) {
		// Deterministic per-chunk progress (design §9), unlike the fluctuating
		// dm_exec_requests percent used for other operations.
		server := ""
		if p.PercentComplete > 0 {
			server = fmt.Sprintf(", server %.0f%%", p.PercentComplete)
		}
		fmt.Fprintf(engineOut, "-- shrink %s (%s): %s (%.0f%%), step %s, chunk %d (~%d left, ETA %ds, %ds unblocked, blocked %ds%s)\n",
			p.File, p.Type, tui.HumanizeMB(p.CurrentMB), p.Percent()*100, tui.HumanizeMB(p.StepMB),
			p.Chunks, p.ChunksRemaining, p.ETASeconds, p.ETASecondsNoBlock, p.BlockedSeconds, server)
	}
	batchProgress := func(p run.BatchDMLProgress) {
		fmt.Fprintf(engineOut, "-- batch %s %s.%s: %d rows (%.0f%%)\n", p.Verb, p.Schema, p.Table, p.RowsDone, p.Percent()*100)
	}
	stepSink := stepSinkTo(engineOut)
	if fwd != nil {
		shrinkProgress = fwd.shrink
		batchProgress = fwd.batch
		stepSink = fwd.step
	}
	shrinkRunner := run.NewShrinkRunner(conn, conn, sampler, run.System, run.ShrinkRunnerConfig{
		Tuning:          shrinkTuning(cfg.Shrink),
		PollInterval:    cfg.Monitoring.BlockingPoll(),
		LogPollInterval: cfg.Monitoring.LogPoll(),
		BlockingTimeout: cfg.Monitoring.BlockingTimeout(),
		LogDrainTimeout: cfg.Monitoring.LogDrainTimeout(),
		KillGrace:       cfg.Monitoring.KillGrace(),
	}, run.WithShrinkProgress(shrinkProgress), run.WithShrinkStop(drain))
	batchRunner := run.NewBatchDMLRunner(conn, conn, sampler, run.System, run.BatchDMLRunnerConfig{
		Tuning:          batchTuning(cfg.BatchDML),
		RCSI:            info.RCSIEnabled,
		PollInterval:    cfg.Monitoring.BlockingPoll(),
		LogPollInterval: cfg.Monitoring.LogPoll(),
		BlockingTimeout: cfg.Monitoring.BlockingTimeout(),
		LogDrainTimeout: cfg.Monitoring.LogDrainTimeout(),
		KillGrace:       cfg.Monitoring.KillGrace(),
	}, run.WithBatchDMLProgress(batchProgress), run.WithBatchDMLStop(drain))
	opts := []run.EngineOption{
		run.WithADR(info.ADREnabled),
		run.WithSession(conn),
		run.WithExpander(conn),
		run.WithProgress(conn),
		run.WithWaits(conn),
		run.WithBlockerReader(conn),
		run.WithLiveReload(),
		run.WithResumeCheck(conn),
		run.WithResumableAborter(conn),
		run.WithReconnectTimeout(cfg.Monitoring.ReconnectTimeout()),
		run.WithDatabase(info.Database),
		run.WithShrinkRunner(shrinkRunner),
		run.WithBatchDMLRunner(batchRunner),
		run.WithCompressionReader(conn),
		run.WithDrainSignal(drain),
		run.WithOutput(engineOut),
		run.WithStepSink(stepSink),
	}
	if killOpt != nil {
		opts = append(opts, killOpt)
	}
	if fwd != nil {
		opts = append(opts, run.WithOpListSink(fwd.ops)) // operations panel (TUI only)
	}
	if cfg.Notifications.WebhookURL != "" {
		opts = append(opts, run.WithNotifier(report.NewNotifier(cfg.Notifications.WebhookURL, cfg.Notifications.OnEvents)))
	}
	if history != nil {
		opts = append(opts, run.WithHistory(history))
	}
	if conn != nil {
		opts = append(opts, run.WithServerClock(conn))
	}
	opts = append(opts, extra...)
	return run.NewEngine(dirs, info.Target(), matrix, cfg.Policy(), checker, runner, opts...)
}

// stepSinkTo formats manifest-level step events to w: one line when an operation
// starts and one when it finishes (with outcome and duration). This is the main
// progress signal for a non-TUI (background) run; in TUI mode w is io.Discard and
// the console shows the same "op i/N" counter instead.
func stepSinkTo(w io.Writer) func(run.StepEvent) {
	return func(ev run.StepEvent) {
		switch ev.Phase {
		case run.StepStarted:
			fmt.Fprintf(w, "-- [%d/%d] %s %s — started\n", ev.Index, ev.Total, ev.Command, ev.Target)
		case run.StepFinished:
			detail := ""
			if ev.Detail != "" {
				detail = " (" + ev.Detail + ")"
			}
			fmt.Fprintf(w, "-- [%d/%d] %s %s — %s in %s%s\n",
				ev.Index, ev.Total, ev.Command, ev.Target, ev.Outcome, ev.Duration.Round(time.Second), detail)
		}
	}
}

// batchTuning maps the batch-DML config block to the run-side tuning carrier
// (defaults already applied), mirroring shrinkTuning.
func batchTuning(b config.BatchDMLConfig) run.BatchTuning {
	return run.BatchTuning{
		InitialSmallRows:  b.InitialSmallRows,
		InitialMediumRows: b.InitialMediumRows,
		InitialLargeRows:  b.InitialLargeRows,
		MinRows:           b.MinRows,
		MaxRows:           b.MaxRows,
		EscalationCapRows: b.EscalationCapRows,
		TargetBatch:       b.TargetBatch(),
		SelfWaitTimeout:   b.SelfWaitTimeout(),
	}
}

// shrinkTuning maps the config block to the run-side tuning carrier (config.ShrinkConfig
// has already applied its defaults), mirroring how cfg.Policy() maps to ddl.Policy.
func shrinkTuning(s config.ShrinkConfig) run.ShrinkTuning {
	return run.ShrinkTuning{
		InitialStepSmallMB:   s.InitialStepSmallMB,
		InitialStepMediumMB:  s.InitialStepMediumMB,
		InitialStepLargeMB:   s.InitialStepLargeMB,
		TargetChunks:         s.TargetChunks,
		MaxStepPctOfFile:     s.MaxStepPctOfFile,
		MinStepMB:            s.MinStepMB,
		MaxStepMB:            s.MaxStepMB,
		TargetBatch:          s.TargetBatch(),
		MaxNoProgress:        s.MaxNoProgress,
		NoProgressBackoff:    s.NoProgressBackoff(),
		NoProgressBackoffMax: s.NoProgressBackoffMax(),
		SelfWaitTimeout:      s.SelfWaitTimeout(),
		LogReuseWaitTimeout:  s.LogReuseWaitTimeout(),
	}
}

// connForDatabase returns a connection in the target database's context. It reuses
// the already-open base connection when the target is the connected database;
// otherwise it opens a fresh connection (and detects its server facts). reused is
// true when the base connection was returned (the caller must not close it).
func connForDatabase(ctx context.Context, base *mssql.Conn, baseInfo mssql.ServerInfo, db, dsn string) (conn *mssql.Conn, info mssql.ServerInfo, reused bool, err error) {
	if strings.EqualFold(db, baseInfo.Database) {
		return base, baseInfo, true, nil
	}
	conn, err = mssql.OpenDatabase(ctx, dsn, db, version.Version())
	if err != nil {
		return nil, mssql.ServerInfo{}, false, err
	}
	info, err = conn.Detect(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, mssql.ServerInfo{}, false, err
	}
	return conn, info, false, nil
}

// queuedDatabases returns the distinct databases the to_run manifests target, in
// name order. A manifest with no `database:` field (or one that fails to load)
// belongs to the connected database, so its work still runs and load errors still
// surface during processing.
func queuedDatabases(toRunDir, connected string) ([]string, error) {
	entries, err := os.ReadDir(toRunDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan queue: %w", err)
	}
	seen := make(map[string]bool)
	var dbs []string
	for _, e := range entries {
		if e.IsDir() || !isYAMLManifest(e.Name()) {
			continue
		}
		db := connected
		if m, lerr := ddl.LoadManifestFile(filepath.Join(toRunDir, e.Name())); lerr == nil && m.Database != "" {
			db = m.Database
		}
		if key := strings.ToLower(db); !seen[key] {
			seen[key] = true
			dbs = append(dbs, db)
		}
	}
	sort.Strings(dbs)
	return dbs, nil
}

// isYAMLManifest reports whether a filename is a manifest the queue picks up: a
// .yaml/.yml file not starting with a dot (matching the runner's own rule).
func isYAMLManifest(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		return true
	default:
		return false
	}
}

// runWithTUI runs the incident console in the foreground while the engine runs
// in the background. The console is fed live from the monitoring connection, and
// operator actions (kill DDL, kill a blocker) are dispatched to the server.
func runWithTUI(ctx context.Context, conn *mssql.Conn, engine *run.Engine, current *currentManifest, fwd *tuiForwarder, drain *run.DrainFlag, banner tui.ServerInfoMsg, pollInterval, blockingTimeout time.Duration) (run.Summary, error) {
	actions := make(chan tui.Action, 8)
	program := tui.NewProgram(tui.New("(running)", actions))
	fwd.attach(program) // engine step/batch/shrink progress now reaches the console
	// The header banner is sent from a goroutine: Program.Send blocks on an unbuffered
	// channel until the event loop (started by program.Run below) drains it, so a
	// synchronous Send here would deadlock before Run is ever reached. The goroutine
	// blocks harmlessly until Run starts, then delivers; if the run finishes first,
	// program.Quit cancels the program context and the Send unblocks and drops the banner.
	go program.Send(banner)

	feedCtx, stopFeed := context.WithCancel(ctx)
	defer stopFeed()
	go feedConsole(feedCtx, program, conn, conn.SPID(), pollInterval, blockingTimeout)
	go dispatchActions(feedCtx, program, conn, conn.SPID(), current, drain, actions)

	// The engine runs under its own cancelable context so that if the console dies first
	// (program.Run returns an error) we can unwind ProcessAll before the caller closes the
	// connection under a still-running statement.
	engineCtx, cancelEngine := context.WithCancel(ctx)
	defer cancelEngine()
	type result struct {
		summary run.Summary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := engine.ProcessAll(engineCtx)
		done <- result{summary, err}
		program.Quit() // close the console when the run finishes
	}()

	runErr := program.Run()
	if runErr != nil {
		cancelEngine() // console died first: stop ProcessAll from touching the connection
	}
	// Always wait for the engine goroutine to return before the caller closes the connection.
	r := <-done
	if runErr != nil {
		return run.Summary{}, runErr
	}
	return r.summary, r.err
}

// blockerGate debounces blocker visibility: a session is surfaced only once it has been
// continuously blocked by our DDL for at least the blocking timeout — the same threshold
// the reaction engine acts on — so a fleeting block (e.g. a page-reclaim latch during a
// shrink) never prompts the operator. firstSeen tracks when each session was first seen
// blocked; entries are pruned once a session is no longer blocked, so its timer resets.
type blockerGate struct {
	firstSeen map[int]time.Time
}

func newBlockerGate() *blockerGate { return &blockerGate{firstSeen: make(map[int]time.Time)} }

// persistent returns the sessions blocked by ddlSPID that have persisted for at least
// timeout, updating the first-seen bookkeeping against now.
func (g *blockerGate) persistent(sessions []mssql.Session, ddlSPID int, now time.Time, timeout time.Duration) []mssql.Session {
	live := make(map[int]bool)
	var out []mssql.Session
	for _, s := range sessions {
		if !s.BlockedBy(ddlSPID) {
			continue
		}
		live[s.SPID] = true
		if _, ok := g.firstSeen[s.SPID]; !ok {
			g.firstSeen[s.SPID] = now
		}
		if now.Sub(g.firstSeen[s.SPID]) >= timeout {
			out = append(out, s)
		}
	}
	for spid := range g.firstSeen {
		if !live[spid] {
			delete(g.firstSeen, spid) // no longer blocked: its debounce timer resets
		}
	}
	return out
}

// blockerAgg accumulates one blocking session's contribution to our suspension.
type blockerAgg struct {
	login string
	count int
	total time.Duration
}

// suspensionTracker accumulates how long and how often the operation has been blocked
// (suspended) and by which sessions, fed one observation per poll. It is the inverse of
// blockerGate (our op as victim, not aggressor). Durations are sampled at the poll
// cadence, so totals are accurate to within one interval.
type suspensionTracker struct {
	episodes  int
	total     time.Duration
	blocked   bool      // were we blocked at the previous observation
	curSPID   int       // the session blocking us in the current episode
	last      time.Time // previous observation time, for interval accrual
	byBlocker map[int]*blockerAgg
	order     []int // SPIDs in first-seen order, for stable rendering
}

func newSuspensionTracker() *suspensionTracker {
	return &suspensionTracker{byBlocker: make(map[int]*blockerAgg)}
}

// observe records one poll. It accrues the interval since the previous observation to the
// session that was blocking us over it, and counts a new episode on each transition into a
// block (or a change of blocker while still blocked).
func (t *suspensionTracker) observe(blocked bool, spid int, login string, now time.Time) {
	if t.blocked && !t.last.IsZero() {
		if d := now.Sub(t.last); d > 0 {
			t.total += d
			if a := t.byBlocker[t.curSPID]; a != nil {
				a.total += d
			}
		}
	}
	t.last = now

	switch {
	case !blocked:
		t.blocked, t.curSPID = false, 0
	case !t.blocked || spid != t.curSPID:
		// A fresh block, or the blocking session changed mid-block: a new episode.
		t.episodes++
		t.blocked, t.curSPID = true, spid
		a := t.byBlocker[spid]
		if a == nil {
			a = &blockerAgg{}
			t.byBlocker[spid] = a
			t.order = append(t.order, spid)
		}
		a.count++
		if login != "" {
			a.login = login
		}
	}
}

// snapshot renders the cumulative stats into a TUI message.
func (t *suspensionTracker) snapshot() tui.SuspensionMsg {
	msg := tui.SuspensionMsg{Episodes: t.episodes, TotalMS: t.total.Milliseconds()}
	for _, spid := range t.order {
		a := t.byBlocker[spid]
		msg.Blockers = append(msg.Blockers, tui.SuspensionBlocker{
			SPID: spid, Login: a.login, Count: a.count, TotalMS: a.total.Milliseconds(),
		})
	}
	return msg
}

// feedConsole polls the server and sends progress and blocker updates to the TUI. A
// blocker is shown only once it has persisted for blockingTimeout (see blockerGate), so
// transient blocks that clear on their own never reach the console.
func feedConsole(ctx context.Context, program *tui.Program, conn *mssql.Conn, ddlSPID int, interval, blockingTimeout time.Duration) {
	program.Send(tui.SPIDMsg{SPID: ddlSPID}) // show which server session is ours
	gate := newBlockerGate()
	susp := newSuspensionTracker()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if sessions, err := conn.ActiveSessions(ctx); err == nil {
				var blockers []tui.Blocker
				for _, s := range gate.persistent(sessions, ddlSPID, time.Now(), blockingTimeout) {
					blockers = append(blockers, tui.Blocker{
						SPID: s.SPID, Login: s.Login, Host: s.Host, Program: s.Program,
						WaitType: s.WaitType, WaitMS: s.WaitMS, Query: s.ActiveQuery,
					})
				}
				program.Send(tui.BlockersMsg{Blockers: blockers})

				// Mirror: report whether OUR operation is itself blocked (the victim), from
				// the same snapshot. Sent every poll so the indicator clears when unblocked.
				sb := mssql.FindSelfBlock(sessions, ddlSPID)
				program.Send(tui.BlockedByMsg{
					Blocked: sb.Blocked, SPID: sb.SPID, Login: sb.Login, Program: sb.Program,
					WaitType: sb.WaitType, WaitMS: sb.WaitMS, Query: sb.Query,
				})
				// Accumulate the suspension history (how long/often/by whom) and send it.
				susp.observe(sb.Blocked, sb.SPID, sb.Login, time.Now())
				program.Send(susp.snapshot())
			}
			if p, found, err := conn.Progress(ctx, ddlSPID); err == nil && found {
				program.Send(progressMsg(p))
			}
			if waits, err := conn.SessionWaits(ctx, ddlSPID); err == nil {
				program.Send(waitsMsg(waits))
			}
		}
	}
}

// progressMsg maps a server progress reading to a TUI message. During a rollback
// the request's percent_complete is rollback progress, shown separately from
// forward progress.
func progressMsg(p mssql.Progress) tui.ProgressMsg {
	msg := tui.ProgressMsg{ETASeconds: p.EstimatedCompletionMS / 1000}
	if p.IsRollback() {
		msg.RollbackPercent = p.PercentComplete
	} else {
		msg.Percent = p.PercentComplete
	}
	return msg
}

// waitsMsg categorizes a session's cumulative waits into a TUI message showing
// what is slowing the DDL down.
func waitsMsg(waits []mssql.SessionWait) tui.WaitsMsg {
	cats, total := mssql.CategorizeWaits(waits)
	out := make([]tui.WaitCategory, len(cats))
	for i, c := range cats {
		out[i] = tui.WaitCategory{Name: c.Name, WaitMS: c.WaitTimeMS, Tasks: c.Tasks}
	}
	return tui.WaitsMsg{Categories: out, TotalMS: total}
}

// serverBanner builds the console header banner from the detected server info: the app
// version, the SQL Server product year (via the matrix's major→year map, falling back to the
// raw major), the edition tier, and the recovery/ADR/RCSI/snapshot flags.
func serverBanner(info mssql.ServerInfo, matrix *ddl.Matrix) tui.ServerInfoMsg {
	product := fmt.Sprintf("SQL Server (major %d)", info.MajorVersion)
	if year, ok := matrix.Year(info.MajorVersion); ok {
		product = fmt.Sprintf("SQL Server %d", year)
	}
	return tui.ServerInfoMsg{
		App:         version.Version(),
		Name:        info.ServerName,
		Product:     product,
		Database:    info.Database,
		Edition:     info.Tier().String(),
		Recovery:    info.RecoveryModel,
		ADR:         info.ADREnabled,
		RCSI:        info.RCSIEnabled,
		SnapshotIso: info.SnapshotIsolation,
	}
}

// tuiForwarder relays engine-side progress (per-operation step events, batch-DML and
// shrink progress) to the incident console. The console's program is created after the
// engine is built, so the forwarder holds a deferred reference, attached once the
// console starts. attach happens-before the engine goroutine that drives send (the
// step/batch callbacks fire only inside ProcessAll), so no lock is needed.
type tuiForwarder struct {
	program *tui.Program
}

func (f *tuiForwarder) attach(p *tui.Program) { f.program = p }

func (f *tuiForwarder) send(msg any) {
	if f.program != nil {
		f.program.Send(msg)
	}
}

// step forwards an operation's start (as a StatusMsg — label, i/N counter, timer anchor)
// or its terminal outcome (as a StepDoneMsg, so the operations panel can mark it DONE/FAILED).
func (f *tuiForwarder) step(ev run.StepEvent) {
	if ev.Phase == run.StepFinished {
		f.send(tui.StepDoneMsg{Index: ev.Index, Outcome: ev.Outcome})
		return
	}
	if msg, ok := stepStatusMsg(ev); ok {
		f.send(msg)
	}
}

// ops forwards the manifest's full operation list to the console's operations panel.
func (f *tuiForwarder) ops(list []run.OpInfo) {
	rows := make([]tui.OperationRow, len(list))
	for i, o := range list {
		rows[i] = tui.OperationRow{Index: o.Index, Label: opLabel(o.Command, o.Target), Status: "TO RUN"}
	}
	f.send(tui.OperationsMsg{Ops: rows})
}

// opLabel is the "<command> <target>" display label an operation shows in the console, used
// for both the operations-panel rows and the current-op status line so they never diverge.
func opLabel(command, target string) string { return strings.TrimSpace(command + " " + target) }

// batch forwards batch-DML progress to the console.
func (f *tuiForwarder) batch(p run.BatchDMLProgress) { f.send(batchMsg(p)) }

// shrink forwards shrink per-chunk progress to the console.
func (f *tuiForwarder) shrink(p run.ShrinkProgress) { f.send(shrinkMsg(p)) }

// alert forwards a manifest failure to the console as a prominent, sticky notice so the
// operator sees the reason (e.g. a shrink refused for lack of db_owner) on screen.
func (f *tuiForwarder) alert(mf run.ManifestFailure) {
	f.send(tui.AlertMsg{Title: "manifest failed: " + mf.Manifest + " — " + mf.Error, Lines: mf.Details})
}

// stepStatusMsg maps a step event to a console status update. Only the started event
// maps (ok=true): it carries the label, the i/N counter and the timer anchor. The
// finished event is dropped — the next operation's started event replaces the
// display, and the run summary/log records outcomes.
func stepStatusMsg(ev run.StepEvent) (tui.StatusMsg, bool) {
	if ev.Phase != run.StepStarted {
		return tui.StatusMsg{}, false
	}
	return tui.StatusMsg{
		Status:    tui.StatusRunning,
		Operation: opLabel(ev.Command, ev.Target),
		StepIndex: ev.Index,
		StepTotal: ev.Total,
		StartedAt: ev.StartedAt,
	}, true
}

// batchMsg maps batch-DML progress to a console BatchMsg.
func batchMsg(p run.BatchDMLProgress) tui.BatchMsg {
	return tui.BatchMsg{
		Verb: p.Verb, Table: p.Schema + "." + p.Table,
		RowsDone: p.RowsDone, EstRows: p.EstRows, Percent: p.Percent(),
		BatchRows: p.BatchRows, RowsPerSec: p.RowsPerSec,
	}
}

// shrinkMsg maps shrink per-chunk progress to a console ShrinkMsg.
func shrinkMsg(p run.ShrinkProgress) tui.ShrinkMsg {
	return tui.ShrinkMsg{
		File: p.File, Type: p.Type, CurrentMB: p.CurrentMB, StartMB: p.StartMB,
		FinalMB: p.FinalMB, StepMB: p.StepMB, Percent: p.Percent(),
		Chunks: p.Chunks, ChunksRemaining: p.ChunksRemaining, ETASeconds: p.ETASeconds,
		ETASecondsNoBlock: p.ETASecondsNoBlock, BlockedSeconds: p.BlockedSeconds,
		ChunkTargetMB: p.ChunkTargetMB, Statement: p.Statement, PercentComplete: p.PercentComplete,
	}
}

// currentManifest holds the path of the manifest the engine is processing, set via
// the engine's manifest observer and read by the action dispatcher so the TUI can
// write an ignore rule into the running manifest.
type currentManifest struct {
	mu   sync.Mutex
	path string
}

func (c *currentManifest) set(path string) {
	c.mu.Lock()
	c.path = path
	c.mu.Unlock()
}

func (c *currentManifest) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path
}

// dispatchActions routes operator intents to the server (kill) or to the running
// manifest (ignore a blocked session).
func dispatchActions(ctx context.Context, program *tui.Program, conn *mssql.Conn, ddlSPID int, current *currentManifest, drain *run.DrainFlag, actions <-chan tui.Action) {
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-actions:
			switch a.Kind {
			case tui.ActionKillDDL:
				_ = conn.Kill(ctx, ddlSPID)
			case tui.ActionKillBlocker:
				_ = conn.Kill(ctx, a.SPID)
			case tui.ActionDrain:
				// The drain key toggles: request a graceful stop, or cancel a pending one.
				if drain.Draining() {
					drain.Cancel()
					program.Send(tui.StatusMsg{Status: tui.StatusRunning})
				} else {
					drain.Request()
					program.Send(tui.StatusMsg{Status: tui.StatusDraining})
				}
			case tui.ActionIgnoreBlocker:
				ignoreBlocker(program, current, a)
			case tui.ActionKillBlockerAuto:
				killBlockerAuto(ctx, program, conn, current, a)
			}
		}
	}
}

// ignoreBlocker writes a one-criterion ignore rule into the running manifest. The
// engine hot-reloads it (WithLiveReload), so the operation stops yielding to that
// session. The outcome is echoed to the console.
func ignoreBlocker(program *tui.Program, current *currentManifest, a tui.Action) {
	rule, ok := ddl.IgnoredSessionFor(a.Criterion, a.Value, a.SPID)
	if !ok {
		program.Send(tui.LogMsg{Line: "ignore: nothing to match on for that session"})
		return
	}
	path := current.get()
	if path == "" {
		program.Send(tui.LogMsg{Line: "ignore: no manifest is running"})
		return
	}
	if err := ddl.AppendIgnoredSession(path, rule); err != nil {
		program.Send(tui.LogMsg{Line: "ignore: " + err.Error()})
		return
	}
	program.Send(tui.LogMsg{Line: fmt.Sprintf("ignoring SPID %d by %s — added to manifest; holding the lock", a.SPID, a.Criterion)})
}

// killBlockerAuto kills the selected blocker now and writes a one-criterion kill rule into
// the running manifest (after_seconds = 0, kill on sight). The engine hot-reloads it
// (WithLiveReload), so a recurrence is auto-killed without a restart. Both effects are the
// inverse of ignoreBlocker; the outcome is echoed to the console. The kill takes effect
// regardless of whether kill_blockers is armed in config — it is an explicit operator act —
// but the auto-kill rule only fires later if the killer is armed.
func killBlockerAuto(ctx context.Context, program *tui.Program, conn *mssql.Conn, current *currentManifest, a tui.Action) {
	rule, ok := ddl.KilledSessionFor(a.Criterion, a.Value, a.SPID)
	if !ok {
		program.Send(tui.LogMsg{Line: "kill: nothing to match on for that session"})
		return
	}
	if err := conn.Kill(ctx, a.SPID); err != nil {
		program.Send(tui.LogMsg{Line: fmt.Sprintf("kill SPID %d: %v", a.SPID, err)})
		// Still record the rule below: a failed kill now should not prevent auto-kill later.
	}
	path := current.get()
	if path == "" {
		program.Send(tui.LogMsg{Line: "kill: no manifest is running (killed once, not remembered)"})
		return
	}
	if err := ddl.AppendKilledSession(path, rule); err != nil {
		program.Send(tui.LogMsg{Line: "kill: " + err.Error()})
		return
	}
	program.Send(tui.LogMsg{Line: fmt.Sprintf("killed SPID %d and auto-killing by %s — added to manifest", a.SPID, a.Criterion)})
}

// loadConfig loads the config file when one is given, else returns nil.
func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		return nil, nil
	}
	return config.Load(path)
}

func policyOf(cfg *config.Config) ddl.Policy {
	if cfg == nil {
		return ddl.Policy{}
	}
	return cfg.Policy()
}

// matrixFile picks the matrix path: an explicit --matrix wins, otherwise the
// config's matrix_file (resolved relative to the config file's directory, so it
// is independent of the working directory).
func matrixFile(cfg *config.Config, configPath, flagValue string, visited map[string]bool) string {
	if cfg == nil || visited["matrix"] {
		return flagValue
	}
	if filepath.IsAbs(cfg.MatrixFile) {
		return cfg.MatrixFile
	}
	return filepath.Join(filepath.Dir(configPath), cfg.MatrixFile)
}

// dryRunSession resolves the dry-run target and, when connected, an index
// expander for "ALTER INDEX ALL" rebuilds. It uses the --assume-* flags when
// either is set (offline, no expander), otherwise connects via the config's
// connection string to detect the real server. The returned cleanup closes any
// connection that was opened.
func dryRunSession(ctx context.Context, log io.Writer, visited map[string]bool, assumeVersion int, assumeEdition string, cfg *config.Config) (ddl.Target, run.IndexExpander, func(), error) {
	noop := func() {}
	offline := visited["assume-version"] || visited["assume-edition"]
	if offline || cfg == nil {
		t, err := offlineTarget(assumeVersion, assumeEdition)
		return t, nil, noop, err
	}

	conn, err := mssql.Open(ctx, cfg.Database.ConnectionString, version.Version())
	if err != nil {
		return ddl.Target{}, nil, noop, err
	}
	cleanup := func() { _ = conn.Close() }

	info, err := conn.Detect(ctx)
	if err != nil {
		cleanup()
		return ddl.Target{}, nil, noop, err
	}
	if !info.Supported() {
		cleanup()
		return ddl.Target{}, nil, noop, fmt.Errorf("unsupported engine edition %d", info.EngineEdition)
	}
	fmt.Fprintf(log, "-- detected target: tier=%s major=%d adr=%t recovery=%s rcsi=%t si=%t\n",
		info.Tier(), info.MajorVersion, info.ADREnabled, info.RecoveryModel, info.RCSIEnabled, info.SnapshotIsolation)
	return info.Target(), conn, cleanup, nil
}

// offlineTarget builds a resolution target from the --assume-* flags.
func offlineTarget(major int, edition string) (ddl.Target, error) {
	tier, err := ddl.ParseTier(edition)
	if err != nil {
		return ddl.Target{}, fmt.Errorf("invalid --assume-edition: %w", err)
	}
	if major <= 0 && tier != ddl.TierAzure {
		return ddl.Target{}, errors.New("set --assume-version (e.g. 16 for SQL Server 2022) or --config to connect")
	}
	return ddl.Target{MajorVersion: major, Tier: tier}, nil
}

// visitedFlags records which flags were explicitly set on the command line.
func visitedFlags(fs *flag.FlagSet) map[string]bool {
	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	return visited
}

// dryRunManifest loads, optionally expands ALL rebuilds, plans, and renders one
// manifest to w. expander is nil for an offline dry-run.
func dryRunManifest(ctx context.Context, w io.Writer, path string, target ddl.Target, matrix *ddl.Matrix, policy ddl.Policy, explain bool, expander run.IndexExpander) error {
	manifest, err := ddl.LoadManifestFile(path)
	if err != nil {
		return err
	}
	if expander != nil {
		manifest, err = run.ExpandAll(ctx, expander, manifest)
		if err != nil {
			return err
		}
	}
	planned, err := ddl.Plan(manifest, target, matrix, policy)
	if err != nil {
		return err
	}
	renderPlan(w, path, manifest, planned, explain)
	return nil
}

// renderPlan prints a manifest's planned operations as runnable, commented T-SQL.
func renderPlan(w io.Writer, source string, manifest *ddl.Manifest, planned []ddl.PlannedOperation, explain bool) {
	if manifest.Description != "" {
		fmt.Fprintf(w, "-- manifest: %s — %s\n", source, manifest.Description)
	} else {
		fmt.Fprintf(w, "-- manifest: %s\n", source)
	}

	if manifest.Window != nil {
		win := manifest.Window
		days := ""
		if len(win.Days) > 0 {
			days = " " + strings.Join(win.Days, ",")
		}
		fmt.Fprintf(w, "-- window %s–%s%s — enforced at runtime (server time; not evaluated in offline dry-run)\n", win.Start, win.End, days)
	}

	if explain && len(manifest.IgnoreBlockedSessions) > 0 {
		fmt.Fprintln(w, "-- ignore_blocked_sessions (allowed to stay blocked; a session matches any one entry):")
		for i, s := range manifest.IgnoreBlockedSessions {
			fmt.Fprintf(w, "--     [%d] %s\n", i+1, s)
		}
		fmt.Fprintln(w)
	}

	for i, step := range planned {
		fmt.Fprintf(w, "-- [%d] %s %s\n",
			i+1, step.Operation.CommandType(), step.Operation.Target())
		fmt.Fprintln(w, step.SQL)
		if explain {
			for _, d := range step.Decisions {
				fmt.Fprintf(w, "--     %s = %s  (%s)\n", d.Option, d.Value, d.Reason)
			}
		}
		fmt.Fprintln(w)
	}
}
