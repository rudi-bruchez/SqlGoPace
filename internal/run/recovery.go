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
	// Resume continues a resumable operation; re-enqueued for an idempotent re-run
	// in this version (true RESUME is a refinement).
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
	OrphanAlive     bool // a live session matches the run's signature
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

// matchesOrphan reports whether a live session is the one recorded in state. The
// SPID alone is unreliable (reused), so login_time must match and, when present,
// the CONTEXT_INFO marker must prefix the session's context_info.
func matchesOrphan(id mssql.SessionIdentity, st RunState) bool {
	if !id.Exists || id.LoginTime != st.LoginTime {
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

// Recoverer scans the processing directory for orphaned manifests after a crash
// and reconciles them with the live server state.
type Recoverer struct {
	dirs  Dirs
	queue *Queue
	probe RecoveryProbe
	out   io.Writer
}

// NewRecoverer builds a Recoverer.
func NewRecoverer(dirs Dirs, probe RecoveryProbe, out io.Writer) *Recoverer {
	return &Recoverer{dirs: dirs, queue: NewQueue(dirs), probe: probe, out: out}
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
		facts, err := r.facts(ctx, st)
		if err != nil {
			return RecoverySummary{}, err
		}

		switch action := DecideRecovery(facts); action {
		case Adopt:
			fmt.Fprintf(r.out, "recovery: %s — orphan SPID %d still running; left in processing\n", manifest, st.SPID)
			sum.Adopted++
		case Resume, Restart:
			fmt.Fprintf(r.out, "recovery: %s — %s (re-enqueued for idempotent re-run)\n", manifest, action)
			if err := r.requeue(manifest, statePath); err != nil {
				return RecoverySummary{}, err
			}
			sum.Requeued++
		}
	}
	return sum, nil
}

func (r *Recoverer) facts(ctx context.Context, st RunState) (RecoveryFacts, error) {
	id, err := r.probe.SessionIdentity(ctx, st.SPID)
	if err != nil {
		return RecoveryFacts{}, fmt.Errorf("session identity for spid %d: %w", st.SPID, err)
	}
	ops, err := r.probe.ResumableOps(ctx)
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

func (r *Recoverer) requeue(manifest, statePath string) error {
	if err := r.queue.Requeue(manifest); err != nil {
		return err
	}
	return RemoveState(statePath)
}
