package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
)

// Preflighter runs the preflight checks for a manifest.
type Preflighter interface {
	Check(ctx context.Context, m *ddl.Manifest) (preflight.Report, error)
}

// SessionInfo provides the execution session signature for the crash-recovery
// sidecar. *mssql.Conn satisfies it.
type SessionInfo interface {
	SPID() int
	LoginTime(ctx context.Context) (string, error)
}

// OpRunner executes one planned operation (with monitoring and reaction). caps
// reports the reaction capabilities derived from the resolved options and server.
type OpRunner interface {
	Run(ctx context.Context, op ddl.Operation, sql string, caps Capabilities) error
}

// Summary counts the outcome of a ProcessAll run.
type Summary struct {
	Done   int
	Failed int
}

// Engine is the outer orchestration loop: it walks the manifest queue and, for
// each, claims it, preflights, plans, runs the operations, and routes it to done
// or failed.
type Engine struct {
	dirs    Dirs
	queue   *Queue
	target  ddl.Target
	matrix  *ddl.Matrix
	policy  ddl.Policy
	adr     bool
	clk     Clock
	session SessionInfo // optional; when set, a recovery sidecar is written
	pf      Preflighter
	runner  OpRunner
	out     io.Writer
}

// NewEngine wires an Engine over the lifecycle directories and dependencies.
// adr is the target's Accelerated Database Recovery state, which biases
// reactions. session may be nil (no recovery sidecar is written).
func NewEngine(dirs Dirs, target ddl.Target, matrix *ddl.Matrix, policy ddl.Policy, adr bool, clk Clock, session SessionInfo, pf Preflighter, runner OpRunner, out io.Writer) *Engine {
	return &Engine{
		dirs:    dirs,
		queue:   NewQueue(dirs),
		target:  target,
		matrix:  matrix,
		policy:  policy,
		adr:     adr,
		clk:     clk,
		session: session,
		pf:      pf,
		runner:  runner,
		out:     out,
	}
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

// processOne handles a single manifest and reports whether it succeeded.
func (e *Engine) processOne(ctx context.Context, name string) bool {
	procPath, err := e.queue.Claim(name)
	if err != nil {
		fmt.Fprintf(e.out, "skip %s: %v\n", name, err)
		return false
	}
	e.writeSidecar(ctx, name)

	manifest, err := ddl.LoadManifestFile(procPath)
	if err != nil {
		return e.failed(name, "load manifest: "+err.Error())
	}

	report, err := e.pf.Check(ctx, manifest)
	if err != nil {
		return e.failed(name, "preflight: "+err.Error())
	}
	if report.HasFailure() {
		return e.failed(name, reportDetail(report))
	}

	planned, err := ddl.Plan(manifest, e.target, e.matrix, e.policy)
	if err != nil {
		return e.failed(name, "plan: "+err.Error())
	}

	for i, step := range planned {
		caps := Capabilities{Resumable: step.Options.Resumable, ADR: e.adr}
		if err := e.runner.Run(ctx, step.Operation, step.SQL, caps); err != nil {
			return e.failed(name, fmt.Sprintf("operation %d (%s): %v", i, step.Operation.CommandType(), err))
		}
	}
	return e.succeeded(name)
}

func (e *Engine) succeeded(name string) bool {
	e.removeSidecar(name)
	if err := e.queue.Complete(name); err != nil {
		fmt.Fprintf(e.out, "complete %s: %v\n", name, err)
	}
	e.writeLog(e.dirs.Done, name, "SUCCESS", "")
	fmt.Fprintf(e.out, "done: %s\n", name)
	return true
}

func (e *Engine) failed(name, detail string) bool {
	e.removeSidecar(name)
	if err := e.queue.Fail(name); err != nil {
		fmt.Fprintf(e.out, "fail %s: %v\n", name, err)
	}
	e.writeLog(e.dirs.Failed, name, "FAILED", detail)
	fmt.Fprintf(e.out, "failed: %s\n", name)
	return false
}

// writeSidecar records the run state next to the manifest so a crash can be
// recovered. It is a best-effort, no-op when no session is configured.
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
		StartedAt: e.clk.Now().UTC().Format(time.RFC3339),
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

func (e *Engine) writeLog(dir, name, outcome, detail string) {
	var b strings.Builder
	fmt.Fprintf(&b, "manifest: %s\noutcome: %s\n", name, outcome)
	if detail != "" {
		fmt.Fprintf(&b, "%s\n", detail)
	}
	path := filepath.Join(dir, name+".log")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintf(e.out, "write log %s: %v\n", path, err)
	}
}

func reportDetail(r preflight.Report) string {
	var b strings.Builder
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "[%s] %s: %s\n", c.Severity, c.Name, c.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}
