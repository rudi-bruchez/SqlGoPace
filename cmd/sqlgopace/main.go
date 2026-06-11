// Command sqlgopace runs demanding DDL operations against Microsoft SQL Server
// while monitoring their impact on locking and the transaction log.
//
// See specs/SPECS.md for the full behaviour and specs/IMPLEMENTATION.md for the
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
	"path/filepath"
	"strings"
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
	if *dryRun {
		return dryRunAll(ctx, stdout, fs.Args(), visited, *assumeVersion, *assumeEdition, *explain, cfg, matrix)
	}

	if cfg == nil {
		return errors.New("run mode requires --config (connection, directories, policy); or pass --dry-run")
	}
	return runEngine(ctx, stdout, cfg, matrix, *useTUI)
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
// the foreground while the engine runs in the background.
func runEngine(ctx context.Context, stdout io.Writer, cfg *config.Config, matrix *ddl.Matrix, useTUI bool) error {
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
	fmt.Fprintf(stdout, "-- target: tier=%s major=%d adr=%t recovery=%s\n",
		info.Tier(), info.MajorVersion, info.ADREnabled, info.RecoveryModel)

	thresholds := preflight.Thresholds{
		LogMaxBytes:   cfg.Monitoring.LogMaxSizeBytes,
		LogMaxPercent: cfg.Monitoring.LogMaxPercent,
	}
	checker := run.NewPreflightChecker(conn, info, thresholds)
	sampler := run.NewServerSampler(conn, conn.SPID(), cfg.Monitoring.LogMaxSizeBytes, cfg.Monitoring.LogMaxPercent)
	runner := run.NewMonitoredRunner(conn, sampler, run.System, run.RunnerConfig{
		PollInterval:    cfg.Monitoring.BlockingPoll(),
		LogPollInterval: cfg.Monitoring.LogPoll(),
		BlockingTimeout: cfg.Monitoring.BlockingTimeout(),
		LogDrainTimeout: cfg.Monitoring.LogDrainTimeout(),
		KillGrace:       cfg.Monitoring.KillGrace(),
		MaxRetries:      cfg.Monitoring.MaxRetryAttempts,
	})
	dirs := run.Dirs{
		ToRun:      cfg.Directories.ToRun,
		Processing: cfg.Directories.Processing,
		Done:       cfg.Directories.Done,
		Failed:     cfg.Directories.Failed,
	}

	// Crash recovery: reconcile any manifests left in processing before starting.
	recoverer := run.NewRecoverer(dirs, conn, stdout)
	if rsum, rerr := recoverer.Recover(ctx); rerr != nil {
		return rerr
	} else if rsum.Requeued > 0 || rsum.Adopted > 0 {
		fmt.Fprintf(stdout, "-- recovery: %d requeued, %d adopted\n", rsum.Requeued, rsum.Adopted)
	}

	engineOut := stdout
	if useTUI {
		engineOut = io.Discard // narration would corrupt the console; the TUI shows state
	}
	opts := []run.EngineOption{
		run.WithADR(info.ADREnabled),
		run.WithSession(conn),
		run.WithExpander(conn),
		run.WithProgress(conn),
		run.WithWaits(conn),
		run.WithResumeCheck(conn),
		run.WithReconnectTimeout(cfg.Monitoring.ReconnectTimeout()),
		run.WithOutput(engineOut),
	}
	if cfg.Notifications.WebhookURL != "" {
		opts = append(opts, run.WithNotifier(report.NewNotifier(cfg.Notifications.WebhookURL, cfg.Notifications.OnEvents)))
	}
	if cfg.History.Enabled {
		history, err := report.OpenHistory(sqlitePath(cfg.History.Destination))
		if err != nil {
			return err
		}
		defer func() { _ = history.Close() }()
		opts = append(opts, run.WithHistory(history))
	}
	engine := run.NewEngine(dirs, info.Target(), matrix, cfg.Policy(), checker, runner, opts...)

	var summary run.Summary
	if useTUI {
		summary, err = runWithTUI(ctx, conn, engine, cfg.Monitoring.ProgressPoll())
	} else {
		summary, err = engine.ProcessAll(ctx)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "processed: %d done, %d failed, %d interrupted\n", summary.Done, summary.Failed, summary.Interrupted)
	if summary.Interrupted > 0 {
		fmt.Fprintf(stdout, "-- %d interrupted manifest(s) left in processing; the next run will resume them\n", summary.Interrupted)
	}
	if summary.Failed > 0 {
		return fmt.Errorf("%d manifest(s) failed", summary.Failed)
	}
	return nil
}

// runWithTUI runs the incident console in the foreground while the engine runs
// in the background. The console is fed live from the monitoring connection, and
// operator actions (kill DDL, kill a blocker) are dispatched to the server.
func runWithTUI(ctx context.Context, conn *mssql.Conn, engine *run.Engine, pollInterval time.Duration) (run.Summary, error) {
	actions := make(chan tui.Action, 8)
	program := tui.NewProgram(tui.New("(running)", false, actions))

	feedCtx, stopFeed := context.WithCancel(ctx)
	defer stopFeed()
	go feedConsole(feedCtx, program, conn, conn.SPID(), pollInterval)
	go dispatchActions(feedCtx, conn, conn.SPID(), actions)

	type result struct {
		summary run.Summary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := engine.ProcessAll(ctx)
		done <- result{summary, err}
		program.Quit() // close the console when the run finishes
	}()

	if err := program.Run(); err != nil {
		return run.Summary{}, err
	}
	r := <-done
	return r.summary, r.err
}

// feedConsole polls the server and sends progress and blocker updates to the TUI.
func feedConsole(ctx context.Context, program *tui.Program, conn *mssql.Conn, ddlSPID int, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if sessions, err := conn.ActiveSessions(ctx); err == nil {
				var blockers []tui.Blocker
				for _, s := range sessions {
					if s.BlockingSPID == ddlSPID {
						blockers = append(blockers, tui.Blocker{
							SPID: s.SPID, Login: s.Login, Host: s.Host, Program: s.Program,
							WaitType: s.WaitType, WaitMS: s.WaitMS, Query: s.ActiveQuery,
						})
					}
				}
				program.Send(tui.BlockersMsg{Blockers: blockers})
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

// dispatchActions routes operator intents to the server.
func dispatchActions(ctx context.Context, conn *mssql.Conn, ddlSPID int, actions <-chan tui.Action) {
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
			}
		}
	}
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
	fmt.Fprintf(log, "-- detected target: tier=%s major=%d adr=%t recovery=%s\n",
		info.Tier(), info.MajorVersion, info.ADREnabled, info.RecoveryModel)
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

	for i, step := range planned {
		ref := step.Operation.Target()
		fmt.Fprintf(w, "-- [%d] %s %s.%s.%s\n",
			i+1, step.Operation.CommandType(), ref.Schema, ref.Table, ref.Name)
		fmt.Fprintln(w, step.SQL)
		if explain {
			for _, d := range step.Decisions {
				fmt.Fprintf(w, "--     %s = %s  (%s)\n", d.Option, d.Value, d.Reason)
			}
		}
		fmt.Fprintln(w)
	}
}
