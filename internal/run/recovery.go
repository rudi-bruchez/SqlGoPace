package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

const stateSuffix = ".state.json"

// RecoveryAction is what to do with a manifest left in processing after a crash.
type RecoveryAction int

const (
	// Restart re-runs the manifest from the beginning (idempotent guards protect).
	Restart RecoveryAction = iota
	// Resume continues a paused resumable operation: the manifest is re-enqueued with
	// its sidecar kept, so the re-run recognizes it as resumed and issues ALTER INDEX …
	// RESUME on the boundary operation (reusing the server-side progress) instead of a
	// fresh REBUILD.
	Resume
	// Adopt leaves a still-running orphan session alone to avoid double execution.
	Adopt
)

// String returns the action name.
func (a RecoveryAction) String() string {
	switch a {
	case Restart:
		return "restart"
	case Resume:
		return "resume"
	case Adopt:
		return "adopt"
	default:
		return "unknown"
	}
}

// RecoveryFacts are the correlated facts for one orphaned manifest.
type RecoveryFacts struct {
	OrphanAlive     bool // a session matching the run's signature is executing a request
	ResumableExists bool // a resumable operation is known to the engine
}

// DecideRecovery chooses the action for an orphaned manifest. An alive orphan is
// adopted (never relaunched); otherwise a resumable operation is resumed, else
// the manifest is restarted.
func DecideRecovery(f RecoveryFacts) RecoveryAction {
	switch {
	case f.OrphanAlive:
		return Adopt
	case f.ResumableExists:
		return Resume
	default:
		return Restart
	}
}

// matchesOrphan reports whether the run recorded in state is still executing. The
// SPID alone is unreliable (reused), so login_time must match and, when present,
// the CONTEXT_INFO marker must prefix the session's context_info. Identity is not
// enough: a crashed process leaves a dangling idle connection whose session still
// exists with the recorded signature but runs nothing, so the session must also
// have a request in flight (id.Active). This assumes recovery runs against a
// crashed prior run, never concurrently with a live SqlGoPace process — the only
// case where our session is legitimately idle between requests (e.g. a shrink
// between chunks); the tool does not support concurrent instances on one queue.
func matchesOrphan(id mssql.SessionIdentity, st State) bool {
	if !id.Exists || !id.Active || id.LoginTime != st.LoginTime {
		return false
	}
	if st.Marker != "" && !strings.HasPrefix(strings.ToLower(id.ContextInfo), strings.ToLower(st.Marker)) {
		return false
	}
	return true
}

// RecoveryProbe is the narrow set of server reads recovery needs.
type RecoveryProbe interface {
	SessionIdentity(ctx context.Context, spid int) (mssql.SessionIdentity, error)
	ResumableOps(ctx context.Context) ([]mssql.ResumableOp, error)
}

var (
	_ RecoveryProbe = (*mssql.Conn)(nil)
	_ SessionInfo   = (*mssql.Conn)(nil)
)

// RecoverySummary counts the recovery outcomes.
type RecoverySummary struct {
	Requeued int
	Adopted  int
}

// ProbeResolver opens a recovery probe for one database, returning the probe and a
// cleanup to close it. It lets recovery reconcile each orphan against a connection
// in the database the orphan ran in (spec §17.8).
type ProbeResolver func(ctx context.Context, database string) (RecoveryProbe, func(), error)

// Recoverer scans the processing directory for orphaned manifests after a crash
// and reconciles them with the live server state.
type Recoverer struct {
	dirs      Dirs
	queue     *Queue
	probe     RecoveryProbe // the connected database's probe (and the default)
	connected string        // the connected database's name
	resolver  ProbeResolver // opens a probe for another database; nil = single-database
	out       io.Writer
}

// RecovererOption configures optional Recoverer behavior.
type RecovererOption func(*Recoverer)

// WithRecoveryProbes enables per-database recovery: orphans whose sidecar names a
// database other than connected are reconciled against a probe from resolver
// (spec §17.8). Without it, every orphan uses the single connected probe.
func WithRecoveryProbes(connected string, resolver ProbeResolver) RecovererOption {
	return func(r *Recoverer) { r.connected, r.resolver = connected, resolver }
}

// NewRecoverer builds a Recoverer.
func NewRecoverer(dirs Dirs, probe RecoveryProbe, out io.Writer, opts ...RecovererOption) *Recoverer {
	r := &Recoverer{dirs: dirs, queue: NewQueue(dirs), probe: probe, out: out}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Recover reconciles every orphaned manifest found in processing.
func (r *Recoverer) Recover(ctx context.Context) (RecoverySummary, error) {
	entries, err := os.ReadDir(r.dirs.Processing)
	if os.IsNotExist(err) {
		return RecoverySummary{}, nil
	}
	if err != nil {
		return RecoverySummary{}, fmt.Errorf("scan processing: %w", err)
	}

	// Sweep stray atomic-write temp files a crash may have left between CreateTemp and Rename
	// (sidecar/watermark writes rewrite these per operation): nothing consumes them and they
	// would only accrue.
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, ".sqlgopace-") && strings.HasSuffix(n, ".tmp") {
			if err := os.Remove(filepath.Join(r.dirs.Processing, n)); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(r.out, "recovery: remove temp %s: %v\n", n, err)
			}
		}
	}

	// Probes opened per database are cached and closed at the end.
	cache := make(map[string]RecoveryProbe)
	var closers []func()
	defer func() {
		for _, c := range closers {
			c()
		}
	}()

	var sum RecoverySummary
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, stateSuffix) {
			continue
		}
		manifest := strings.TrimSuffix(name, stateSuffix)
		statePath := filepath.Join(r.dirs.Processing, name)

		st, err := ReadState(statePath)
		if err != nil {
			fmt.Fprintf(r.out, "recovery: read %s: %v\n", name, err)
			continue
		}

		probe, err := r.probeFor(ctx, st.Database, cache, &closers)
		if err != nil {
			// Can't reach the orphan's database (e.g. an AG secondary now): leave it
			// in processing for a later run that finds the database writable here.
			fmt.Fprintf(r.out, "recovery: %s — cannot reach database %q: %v; left in processing\n", manifest, st.Database, err)
			continue
		}
		facts, err := factsWith(ctx, probe, st)
		if err != nil {
			// Same treatment as an unreachable database above: one orphan we cannot read
			// the server about must not abandon the sweep, which would strand every
			// orphan after it and discard what this pass already reconciled. Leaving it
			// in processing is the conservative outcome — a later run reconsiders it, and
			// nothing is requeued while we cannot tell whether the session is still live.
			fmt.Fprintf(r.out, "recovery: %s — cannot read server state: %v; left in processing\n", manifest, err)
			continue
		}

		switch action := DecideRecovery(facts); action {
		case Adopt:
			fmt.Fprintf(r.out, "recovery: %s — orphan SPID %d still running; left in processing\n", manifest, st.SPID)
			sum.Adopted++
		case Resume, Restart:
			fmt.Fprintf(r.out, "recovery: %s — %s (re-enqueued for the next run)\n", manifest, action)
			// Keep the sidecar when the next run needs it: to skip completed operations
			// (cursor set) or to adopt a paused resumable at the boundary op (Resume).
			keepSidecar := action == Resume || st.ResumeFromOp > 0
			if err := r.requeue(manifest, statePath, keepSidecar); err != nil {
				return RecoverySummary{}, err
			}
			sum.Requeued++
		}
	}
	return sum, nil
}

// probeFor resolves the recovery probe for an orphan's database: the connected
// probe for the connected database (or single-database mode), else a cached probe
// opened via the resolver.
func (r *Recoverer) probeFor(ctx context.Context, database string, cache map[string]RecoveryProbe, closers *[]func()) (RecoveryProbe, error) {
	if r.resolver == nil || database == "" || strings.EqualFold(database, r.connected) {
		return r.probe, nil
	}
	key := strings.ToLower(database)
	if p, ok := cache[key]; ok {
		return p, nil
	}
	p, closer, err := r.resolver(ctx, database)
	if err != nil {
		return nil, err
	}
	cache[key] = p
	*closers = append(*closers, closer)
	return p, nil
}

// factsWith correlates an orphan's recorded session against the live state read
// through probe (the probe for the orphan's database).
func factsWith(ctx context.Context, probe RecoveryProbe, st State) (RecoveryFacts, error) {
	id, err := probe.SessionIdentity(ctx, st.SPID)
	if err != nil {
		return RecoveryFacts{}, fmt.Errorf("session identity for spid %d: %w", st.SPID, err)
	}
	ops, err := probe.ResumableOps(ctx)
	if err != nil {
		return RecoveryFacts{}, fmt.Errorf("resumable operations: %w", err)
	}
	paused := false
	for _, op := range ops {
		if op.StateDesc == "PAUSED" {
			paused = true
			break
		}
	}
	return RecoveryFacts{OrphanAlive: matchesOrphan(id, st), ResumableExists: paused}, nil
}

// requeue moves a manifest back to to_run for a fresh run. keepSidecar preserves the
// State sidecar so the re-run can honor it — skipping completed operations via the resume
// cursor, and adopting a paused resumable (which marks the run as resumed) via ALTER INDEX
// … RESUME; otherwise the sidecar is removed (a clean restart from the first operation).
func (r *Recoverer) requeue(manifest, statePath string, keepSidecar bool) error {
	if err := r.queue.Requeue(manifest); err != nil {
		// A prior requeue that kept the sidecar may already have moved the manifest (a
		// crash between that requeue and the re-run's claim). If it is already in
		// to_run, the engine will pick it up — not an error.
		if !r.queue.InToRun(manifest) {
			return err
		}
	}
	if keepSidecar {
		return nil
	}
	return RemoveState(statePath)
}
