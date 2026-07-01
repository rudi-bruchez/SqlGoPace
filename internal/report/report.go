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

// ReactionLine records one reaction taken while an operation ran (a pause,
// resume, cancel, or fallback kill), so the log shows how pressure was handled.
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
	Index       int                `json:"index"`
	CommandType string             `json:"command_type"`
	Target      string             `json:"target"`
	SQL         string             `json:"sql"`
	Options     []OptionDecision   `json:"options,omitempty"`
	Reactions   []ReactionLine     `json:"reactions,omitempty"`
	PeakBlocked int                `json:"peak_blocked,omitempty"`
	Waits       []WaitLine         `json:"waits,omitempty"`
	WaitTotalMS int64              `json:"wait_total_ms,omitempty"`
	Shrink      []ShrinkFileReport `json:"shrink,omitempty"`
	BatchDML    *BatchDMLReport    `json:"batch_dml,omitempty"`
	Outcome     string             `json:"outcome"`
	Error       string             `json:"error,omitempty"`
	DurationMS  int64              `json:"duration_ms"`
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
}

// Write renders the report as a human summary followed by a JSON block.
func Write(w io.Writer, r RunReport) error {
	fmt.Fprintln(w, "SqlGoPace run report")
	fmt.Fprintf(w, "manifest: %s\n", r.Manifest)
	fmt.Fprintf(w, "outcome: %s\n", r.Outcome)
	fmt.Fprintf(w, "started: %s  finished: %s  duration: %dms\n", r.StartedAt, r.FinishedAt, r.DurationMS)

	if len(r.Preflight) > 0 {
		fmt.Fprintln(w, "\npreflight:")
		for _, c := range r.Preflight {
			fmt.Fprintf(w, "  [%s] %s: %s\n", c.Severity, c.Name, c.Detail)
		}
	}
	if len(r.Operations) > 0 {
		fmt.Fprintln(w, "\noperations:")
		for _, op := range r.Operations {
			fmt.Fprintf(w, "  [%d] %s %s — %s (%dms)\n", op.Index, op.CommandType, op.Target, op.Outcome, op.DurationMS)
			for _, d := range op.Options {
				fmt.Fprintf(w, "      %s = %s (%s)\n", d.Option, d.Value, d.Reason)
			}
			for _, rx := range op.Reactions {
				fmt.Fprintf(w, "      reaction: %s at %s (%s)\n", rx.Kind, rx.At, rx.Detail)
			}
			if op.PeakBlocked > 0 {
				fmt.Fprintf(w, "      peak blocked: %d session(s)\n", op.PeakBlocked)
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
			if op.Error != "" {
				fmt.Fprintf(w, "      error: %s\n", op.Error)
			}
			fmt.Fprintf(w, "      %s\n", op.SQL)
		}
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
