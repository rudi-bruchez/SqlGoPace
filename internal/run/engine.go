package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
)

// Preflighter runs the preflight checks for a manifest.
type Preflighter interface {
	Check(ctx context.Context, m *ddl.Manifest) (preflight.Report, error)
}

// OpRunner executes one planned operation (with monitoring and reaction).
type OpRunner interface {
	Run(ctx context.Context, op ddl.Operation, sql string) error
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
	dirs   Dirs
	queue  *Queue
	target ddl.Target
	matrix *ddl.Matrix
	policy ddl.Policy
	pf     Preflighter
	runner OpRunner
	out    io.Writer
}

// NewEngine wires an Engine over the lifecycle directories and dependencies.
func NewEngine(dirs Dirs, target ddl.Target, matrix *ddl.Matrix, policy ddl.Policy, pf Preflighter, runner OpRunner, out io.Writer) *Engine {
	return &Engine{
		dirs:   dirs,
		queue:  NewQueue(dirs),
		target: target,
		matrix: matrix,
		policy: policy,
		pf:     pf,
		runner: runner,
		out:    out,
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
		if err := e.runner.Run(ctx, step.Operation, step.SQL); err != nil {
			return e.failed(name, fmt.Sprintf("operation %d (%s): %v", i, step.Operation.CommandType(), err))
		}
	}
	return e.succeeded(name)
}

func (e *Engine) succeeded(name string) bool {
	if err := e.queue.Complete(name); err != nil {
		fmt.Fprintf(e.out, "complete %s: %v\n", name, err)
	}
	e.writeLog(e.dirs.Done, name, "SUCCESS", "")
	fmt.Fprintf(e.out, "done: %s\n", name)
	return true
}

func (e *Engine) failed(name, detail string) bool {
	if err := e.queue.Fail(name); err != nil {
		fmt.Fprintf(e.out, "fail %s: %v\n", name, err)
	}
	e.writeLog(e.dirs.Failed, name, "FAILED", detail)
	fmt.Fprintf(e.out, "failed: %s\n", name)
	return false
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
