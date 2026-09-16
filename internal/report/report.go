// Package report renders per-manifest run logs (a human summary plus a
// machine-readable JSON block), persists run history to SQLite, and sends
// webhook notifications. It owns its own data types and depends on no other
// internal package, so it stays a leaf.
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// JSONDelimiter separates the human summary from the JSON block in a run log.
const JSONDelimiter = "\n===== machine-readable JSON =====\n"

// OptionDecision is an injected option and why it was set that way.
type OptionDecision struct {
	Option string `json:"option"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// ReactionLine records one reaction taken while an operation ran (pause, resume,
// cancel, fallback kill, abort, or an advisory warn/info), so the log shows how
// pressure was handled.
type ReactionLine struct {
	Kind   string `json:"kind"`
	At     string `json:"at"`
	Detail string `json:"detail"`
}

// WaitLine is one category of waits that slowed the operation, with its summed
// time (and the signal/CPU portion).
type WaitLine struct {
	Category    string `json:"category"`
	Description string `json:"description"`
	WaitMS      int64  `json:"wait_ms"`
	SignalMS    int64  `json:"signal_ms,omitempty"`
	Tasks       int64  `json:"tasks"`
}

// ShrinkFileReport is the per-file outcome of a shrink operation: the page-moving
// shrink driver works file by file, so a single shrink operation can produce
// several of these (notably files:all).
type ShrinkFileReport struct {
	File      string `json:"file"`
	Type      string `json:"type"` // "data" | "log"
	InitialMB int    `json:"initial_mb"`
	FinalMB   int    `json:"final_mb"`
	GainedMB  int    `json:"gained_mb"`
	Chunks    int    `json:"chunks,omitempty"`
	NoOp      bool   `json:"no_op,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// BatchDMLReport is the outcome of one batched UPDATE/DELETE operation.
type BatchDMLReport struct {
	Verb      string `json:"verb"` // "update" | "delete"
	Rows      int64  `json:"rows"`
	Batches   int    `json:"batches"`
	FinalRows int    `json:"final_rows,omitempty"` // the last adaptive batch size
	Reason    string `json:"reason,omitempty"`     // why it stopped early; empty on completion
}

// OperationReport is the outcome of one executed operation.
type OperationReport struct {
	Index          int                `json:"index"`
	CommandType    string             `json:"command_type"`
	Target         string             `json:"target"`
	SQL            string             `json:"sql"`
	Options        []OptionDecision   `json:"options,omitempty"`
	Reactions      []ReactionLine     `json:"reactions,omitempty"`
	PeakBlocked    int                `json:"peak_blocked,omitempty"`
	ContendedCount int                `json:"contended_count,omitempty"`
	ContendedFile  string             `json:"contended_file,omitempty"`
	Waits          []WaitLine         `json:"waits,omitempty"`
	WaitTotalMS    int64              `json:"wait_total_ms,omitempty"`
	Shrink         []ShrinkFileReport `json:"shrink,omitempty"`
	BatchDML       *BatchDMLReport    `json:"batch_dml,omitempty"`
	Sizes          []SizeLine         `json:"sizes,omitempty"`
	// SizesPartial marks a reorganize that did not succeed but kept its committed work:
	// the "after" size is real, not the size of a completed operation.
	SizesPartial bool   `json:"sizes_partial,omitempty"`
	Outcome      string `json:"outcome"`
	Detail       string `json:"detail,omitempty"` // context for the outcome, e.g. a skip reason
	Error        string `json:"error,omitempty"`
	DurationMS   int64  `json:"duration_ms"`
}

// CheckLine is one preflight check result.
type CheckLine struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
}

// RunReport is the full record of processing one manifest.
type RunReport struct {
	Manifest   string            `json:"manifest"`
	Outcome    string            `json:"outcome"`
	StartedAt  string            `json:"started_at"`
	FinishedAt string            `json:"finished_at"`
	DurationMS int64             `json:"duration_ms"`
	Preflight  []CheckLine       `json:"preflight,omitempty"`
	Operations []OperationReport `json:"operations,omitempty"`
	Error      string            `json:"error,omitempty"`

	// CancelOnlyNotice is the manifest-start line naming how many planned operations
	// can only be canceled under pressure (a cancel rolls back all their work), empty
	// when none can. See docs/specs/CANCEL-ONLY.md §2.
	CancelOnlyNotice string `json:"cancel_only_notice,omitempty"`
	// CancelOnlySummary names how many of those were actually canceled, split by
	// whether a retry saved them, empty when none were. See CANCEL-ONLY.md §3.
	CancelOnlySummary string `json:"cancel_only_summary,omitempty"`

	// HeapScopeNotices records, per rebuild_heap operation, that the rebuild also
	// rewrites the table's nonclustered indexes — visible in the .log even though the
	// console-facing warning (preflight's "heap rebuild scope" check) only ever shows
	// once. See OBJECT-SIZES.md §5.
	HeapScopeNotices []string `json:"heap_scope_notices,omitempty"`
	// SizeBeforeKB, SizeAfterKB and SizeStructures are the manifest total from §5.5: one
	// "before" and one "after" per distinct structure (first measured, last measured),
	// so a structure touched twice in one manifest is not double-counted.
	SizeBeforeKB   int64 `json:"size_before_kb,omitempty"`
	SizeAfterKB    int64 `json:"size_after_kb,omitempty"`
	SizeStructures int   `json:"size_structures,omitempty"`
	// SizesUnread names the first size-read failure of the manifest, printed once
	// instead of "unknown -> unknown" under every operation.
	SizesUnread string `json:"sizes_unread,omitempty"`
}

// SizeUnknown marks a side of a size line that was not measured: the read failed, or the
// operation did not reach the point where that side is meaningful.
const SizeUnknown int64 = -1

// SizeLine is one structure's used size before and after an operation. Name is "heap" for
// the heap itself. WasDisabled marks an index the operation re-enabled: it had no pages
// before, so its growth is real and its "before" is not a shrinkable figure.
type SizeLine struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	WasDisabled bool   `json:"was_disabled,omitempty"`
	BeforeKB    int64  `json:"before_kb"`
	AfterKB     int64  `json:"after_kb"`
}

// HumanizeKB renders a size in kilobytes, escalating the unit so large values stay
// readable. It mirrors tui.HumanizeMB's steps (and its two decimals at TB) without
// importing it: internal/report has no internal dependency, and one formatter is not
// worth making the report package depend on the console's.
func HumanizeKB(kb int64) string {
	switch {
	case kb < 1024:
		return fmt.Sprintf("%d KB", kb)
	case kb < 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(kb)/1024)
	case kb < 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(kb)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f TB", float64(kb)/(1024*1024*1024))
	}
}

// sizeChange renders "before -> after (-p%)", dropping the percentage when either side is
// unknown or the before size is zero (a re-enabled index grew from nothing; there is no
// percentage to state).
func sizeChange(beforeKB, afterKB int64) string {
	before, after := "unknown", "unknown"
	if beforeKB != SizeUnknown {
		before = HumanizeKB(beforeKB)
	}
	if afterKB != SizeUnknown {
		after = HumanizeKB(afterKB)
	}
	out := before + " -> " + after
	if beforeKB > 0 && afterKB != SizeUnknown {
		out += fmt.Sprintf(" (%+.1f%%)", (float64(afterKB)-float64(beforeKB))/float64(beforeKB)*100)
	}
	return out
}

// renderSizes prints an operation's size lines: one line for a single structure, a block
// with a total when the operation rewrote several (a heap rebuild).
func renderSizes(w io.Writer, op OperationReport) {
	partial := ""
	if op.SizesPartial {
		partial = ", partial"
	}
	switch len(op.Sizes) {
	case 0:
		return
	case 1:
		s := op.Sizes[0]
		fmt.Fprintf(w, "      size: %s %s%s\n", sizeName(s), sizeChange(s.BeforeKB, s.AfterKB), partial)
		return
	}
	fmt.Fprintf(w, "      size (heap and %d nonclustered index(es))%s:\n", len(op.Sizes)-1, partial)
	var totalBefore, totalAfter int64
	var measured int
	for _, s := range op.Sizes {
		fmt.Fprintf(w, "        %-34s %s\n", sizeName(s), sizeChange(s.BeforeKB, s.AfterKB))
		// Skip a structure unless both sides are known — the same rule sizeTotals.add
		// (internal/run/sizes.go) uses. Adding a known side on its own lets the total show
		// growth that is an artifact of a half-measured set rather than a real change (H7,
		// first bullet, the 2026-09-16 harm review).
		if s.BeforeKB == SizeUnknown || s.AfterKB == SizeUnknown {
			continue
		}
		totalBefore += s.BeforeKB
		totalAfter += s.AfterKB
		measured++
	}
	label := "total"
	if measured < len(op.Sizes) {
		label = fmt.Sprintf("total (%d of %d measured)", measured, len(op.Sizes))
	}
	fmt.Fprintf(w, "        %-34s %s\n", label, sizeChange(totalBefore, totalAfter))
}

// sizeName names a size line for the .log, marking an index the operation re-enabled.
func sizeName(s SizeLine) string {
	if s.WasDisabled {
		return s.Name + " (was disabled)"
	}
	return s.Name
}

// Write renders the report as a human summary followed by a JSON block.
func Write(w io.Writer, r RunReport) error {
	fmt.Fprintln(w, "SqlGoPace run report")
	fmt.Fprintf(w, "manifest: %s\n", r.Manifest)
	fmt.Fprintf(w, "outcome: %s\n", r.Outcome)
	fmt.Fprintf(w, "started: %s  finished: %s  duration: %dms\n", r.StartedAt, r.FinishedAt, r.DurationMS)
	if r.CancelOnlyNotice != "" {
		fmt.Fprintf(w, "%s\n", r.CancelOnlyNotice)
	}
	for _, notice := range r.HeapScopeNotices {
		fmt.Fprintf(w, "%s\n", notice)
	}

	if len(r.Preflight) > 0 {
		fmt.Fprintln(w, "\npreflight:")
		for _, c := range r.Preflight {
			fmt.Fprintf(w, "  [%s] %s: %s\n", c.Severity, c.Name, c.Detail)
		}
	}
	if len(r.Operations) > 0 {
		fmt.Fprintln(w, "\noperations:")
		for _, op := range r.Operations {
			detail := ""
			if op.Detail != "" {
				detail = ": " + op.Detail
			}
			fmt.Fprintf(w, "  [%d] %s %s — %s%s (%dms)\n",
				op.Index, op.CommandType, op.Target, op.Outcome, detail, op.DurationMS)
			for _, d := range op.Options {
				fmt.Fprintf(w, "      %s = %s (%s)\n", d.Option, d.Value, d.Reason)
			}
			for _, rx := range op.Reactions {
				fmt.Fprintf(w, "      reaction: %s at %s (%s)\n", rx.Kind, rx.At, rx.Detail)
			}
			if op.PeakBlocked > 0 {
				fmt.Fprintf(w, "      peak blocked: %d session(s)\n", op.PeakBlocked)
			}
			if op.ContendedCount > 0 {
				fmt.Fprintf(w, "      contended objects: %d — see %s\n", op.ContendedCount, op.ContendedFile)
			}
			if len(op.Waits) > 0 {
				fmt.Fprintf(w, "      waits (total %dms):\n", op.WaitTotalMS)
				for _, wl := range op.Waits {
					fmt.Fprintf(w, "        %-20s %8dms  %6d tasks  — %s\n", wl.Category, wl.WaitMS, wl.Tasks, wl.Description)
				}
			}
			for _, sf := range op.Shrink {
				fmt.Fprintf(w, "      shrink %s (%s): %d MB -> %d MB (gained %d MB)", sf.File, sf.Type, sf.InitialMB, sf.FinalMB, sf.GainedMB)
				if sf.Chunks > 0 {
					fmt.Fprintf(w, ", %d chunks", sf.Chunks)
				}
				if sf.NoOp {
					fmt.Fprint(w, ", no-op")
				}
				if sf.Reason != "" {
					fmt.Fprintf(w, " — %s", sf.Reason)
				}
				fmt.Fprintln(w)
			}
			renderSizes(w, op)
			if op.Error != "" {
				fmt.Fprintf(w, "      error: %s\n", op.Error)
			}
			fmt.Fprintf(w, "      %s\n", op.SQL)
		}
	}
	if r.SizesUnread != "" {
		fmt.Fprintf(w, "\nsizes not measured: %s\n", r.SizesUnread)
	}
	if r.SizeStructures > 0 {
		fmt.Fprintf(w, "\nsize: %s over %d structure(s)\n", sizeChange(r.SizeBeforeKB, r.SizeAfterKB), r.SizeStructures)
	}
	if r.CancelOnlySummary != "" {
		fmt.Fprintf(w, "\n%s\n", r.CancelOnlySummary)
	}
	if r.Error != "" {
		fmt.Fprintf(w, "\nerror: %s\n", r.Error)
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if _, err := io.WriteString(w, JSONDelimiter); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}

// WriteFile writes the report to path.
func WriteFile(path string, r RunReport) error {
	var buf bytes.Buffer
	if err := Write(&buf, r); err != nil {
		return err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
