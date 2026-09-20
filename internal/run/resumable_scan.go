package run

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// resumableWarning is one paused resumable index rebuild found on the server before a run
// starts, and whether anything in the queue will continue it.
//
// A paused rebuild is not inert: it keeps both the old and the new structure allocated
// until it finishes or is aborted, it blocks every other rebuild on its table (Msg 10637),
// and DML keeps maintaining both. Left unnoticed it costs disk and blocks work, which is
// why it is reported at startup rather than discovered when an operation trips over it.
type resumableWarning struct {
	Schema, Table, Index string
	PercentComplete      float64
	ExecutionMinutes     int
	// Covered is true when a queued operation targets this exact index, so the run is
	// expected to continue it rather than leave it stranded.
	Covered bool
}

// classifyResumables pairs the server's paused resumable rebuilds with the indexes the
// queue is going to touch. RUNNING operations are excluded: they belong to a live run.
func classifyResumables(ops []mssql.ResumableOp, queued []ddl.ObjectRef) []resumableWarning {
	var out []resumableWarning
	for _, op := range ops {
		if !strings.EqualFold(op.StateDesc, "PAUSED") {
			continue
		}
		w := resumableWarning{
			Schema: op.Schema, Table: op.Table, Index: op.Name,
			PercentComplete: op.PercentComplete, ExecutionMinutes: op.ExecutionMinutes,
		}
		for _, q := range queued {
			if strings.EqualFold(q.Schema, op.Schema) && strings.EqualFold(q.Table, op.Table) &&
				strings.EqualFold(q.Name, op.Name) {
				w.Covered = true
				break
			}
		}
		out = append(out, w)
	}
	return out
}

// warnStrandedResumables reports, before the first manifest runs, every paused resumable
// rebuild the database still holds and whether the queue will continue it. Best-effort
// throughout: no probe, an unreadable DMV or an unreadable manifest means no warning, never
// a failed run — this only tells the operator what is there.
func (e *Engine) warnStrandedResumables(ctx context.Context, names []string) {
	if e.resumeCheck == nil {
		return
	}
	ops, err := e.resumeCheck.ResumableOps(ctx)
	if err != nil || len(ops) == 0 {
		return
	}
	var queued []ddl.ObjectRef
	for _, name := range names {
		m, err := ddl.LoadManifestFile(filepath.Join(e.dirs.ToRun, name))
		if err != nil {
			continue
		}
		for _, op := range m.Operations {
			queued = append(queued, op.Target())
		}
	}
	for _, w := range classifyResumables(ops, queued) {
		line := resumableWarningLine(w)
		fmt.Fprintf(e.out, "-- warning: %s\n", line)
		if e.noticeSink != nil {
			e.noticeSink(line)
		}
	}
}

// resumableWarningLine renders one warning for the run log and the console. A covered one
// says the queue will continue it; an orphan names the statement that would, because the
// only other way out is an ABORT that discards the progress for good.
func resumableWarningLine(w resumableWarning) string {
	var b strings.Builder
	fmt.Fprintf(&b, "paused resumable rebuild %s.%s.%s at %.1f%%", w.Schema, w.Table, w.Index, w.PercentComplete)
	if w.ExecutionMinutes > 0 {
		fmt.Fprintf(&b, " after %dmin", w.ExecutionMinutes)
	}
	if w.Covered {
		b.WriteString(" — named by a queued operation, which is expected to continue it")
		return b.String()
	}
	fmt.Fprintf(&b, " — no queued operation targets it; it holds disk and blocks every rebuild on %s.%s. "+
		"Continue it with `ALTER INDEX [%s] ON [%s].[%s] RESUME;`",
		w.Schema, w.Table, w.Index, w.Schema, w.Table)
	return b.String()
}
