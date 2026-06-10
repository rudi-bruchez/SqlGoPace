package run

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// Preflighter runs the preflight checks for a manifest.
type Preflighter interface {
	Check(ctx context.Context, m *ddl.Manifest) (preflight.Report, error)
}

// OpRunner executes one planned operation (with monitoring and reaction). caps
// reports the reaction capabilities derived from the resolved options and server.
type OpRunner interface {
	Run(ctx context.Context, op ddl.Operation, sql string, caps Capabilities) error
}

// SessionInfo provides the execution session signature for the crash-recovery
// sidecar. *mssql.Conn satisfies it.
type SessionInfo interface {
	SPID() int
	LoginTime(ctx context.Context) (string, error)
}

// Summary counts the outcome of a ProcessAll run.
type Summary struct {
	Done   int
	Failed int
}

// Engine is the outer orchestration loop: it walks the manifest queue and, for
// each, claims it, preflights, plans, runs the operations, writes a run report,
// and routes it to done or failed.
type Engine struct {
	dirs     Dirs
	queue    *Queue
	target   ddl.Target
	matrix   *ddl.Matrix
	policy   ddl.Policy
	pf       Preflighter
	runner   OpRunner
	adr      bool
	clk      Clock
	session  SessionInfo
	notifier *report.Notifier
	history  *report.History
	out      io.Writer
}

// EngineOption configures optional Engine behaviour.
type EngineOption func(*Engine)

// WithADR sets the target's Accelerated Database Recovery state (biases reactions).
func WithADR(adr bool) EngineOption { return func(e *Engine) { e.adr = adr } }

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

// NewEngine wires an Engine over the lifecycle directories and required
// dependencies; optional behaviour is supplied via options.
func NewEngine(dirs Dirs, target ddl.Target, matrix *ddl.Matrix, policy ddl.Policy, pf Preflighter, runner OpRunner, opts ...EngineOption) *Engine {
	e := &Engine{
		dirs:   dirs,
		queue:  NewQueue(dirs),
		target: target,
		matrix: matrix,
		policy: policy,
		pf:     pf,
		runner: runner,
		clk:    System,
		out:    io.Discard,
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
		if e.processOne(ctx, name) {
			sum.Done++
		} else {
			sum.Failed++
		}
	}
	return sum, nil
}

func (e *Engine) processOne(ctx context.Context, name string) bool {
	start := e.clk.Now()
	rep := &report.RunReport{Manifest: name, StartedAt: e.now()}

	procPath, err := e.queue.Claim(name)
	if err != nil {
		fmt.Fprintf(e.out, "skip %s: %v\n", name, err)
		return false
	}
	e.writeSidecar(ctx, name)

	manifest, err := ddl.LoadManifestFile(procPath)
	if err != nil {
		rep.Error = "load manifest: " + err.Error()
		return e.finalize(ctx, name, rep, start, false)
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

	for i, step := range planned {
		opStart := e.clk.Now()
		caps := Capabilities{Resumable: step.Options.Resumable, ADR: e.adr}
		runErr := e.runner.Run(ctx, step.Operation, step.SQL, caps)

		opRep := report.OperationReport{
			Index:       i + 1,
			CommandType: step.Operation.CommandType(),
			Target:      opTarget(step.Operation),
			SQL:         step.SQL,
			Options:     optionDecisions(step.Decisions),
			DurationMS:  e.msSince(opStart),
		}
		if runErr != nil {
			opRep.Outcome = "failed"
			opRep.Error = runErr.Error()
			rep.Operations = append(rep.Operations, opRep)
			rep.Error = fmt.Sprintf("operation %d (%s): %v", i, step.Operation.CommandType(), runErr)
			return e.finalize(ctx, name, rep, start, false)
		}
		opRep.Outcome = "success"
		rep.Operations = append(rep.Operations, opRep)
	}
	return e.finalize(ctx, name, rep, start, true)
}

// finalize records the outcome: moves the manifest, writes the report, notifies,
// and persists history.
func (e *Engine) finalize(ctx context.Context, name string, rep *report.RunReport, start time.Time, success bool) bool {
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

	if err := report.WriteFile(filepath.Join(dir, name+".log"), *rep); err != nil {
		fmt.Fprintf(e.out, "write log %s: %v\n", name, err)
	}
	if !success {
		e.notify(ctx, "fail", name, rep.Error)
	}
	e.record(ctx, *rep)

	fmt.Fprintf(e.out, "%s: %s\n", map[bool]string{true: "done", false: "failed"}[success], name)
	return success
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
// recovered. It is a best-effort no-op when no session is configured.
func (e *Engine) writeSidecar(ctx context.Context, name string) {
	if e.session == nil {
		return
	}
	login, err := e.session.LoginTime(ctx)
	if err != nil {
		fmt.Fprintf(e.out, "sidecar %s: login time: %v\n", name, err)
	}
	state := RunState{
		Manifest:  name,
		SPID:      e.session.SPID(),
		LoginTime: login,
		StartedAt: e.now(),
	}
	if err := WriteState(e.sidecarPath(name), state); err != nil {
		fmt.Fprintf(e.out, "sidecar %s: %v\n", name, err)
	}
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
	ref := op.Target()
	return fmt.Sprintf("%s.%s.%s", ref.Schema, ref.Table, ref.Name)
}
