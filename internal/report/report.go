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

// OperationReport is the outcome of one executed operation.
type OperationReport struct {
	Index       int              `json:"index"`
	CommandType string           `json:"command_type"`
	Target      string           `json:"target"`
	SQL         string           `json:"sql"`
	Options     []OptionDecision `json:"options,omitempty"`
	Outcome     string           `json:"outcome"`
	Error       string           `json:"error,omitempty"`
	DurationMS  int64            `json:"duration_ms"`
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
