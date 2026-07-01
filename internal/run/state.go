package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State is the sidecar written next to a manifest while it executes. After a
// crash it lets recovery correlate an orphaned session to its run (SPID +
// login_time + CONTEXT_INFO marker) and decide whether to resume or abandon.
type State struct {
	Manifest  string `json:"manifest"`
	Database  string `json:"database,omitempty"` // the database the operation ran in (for per-database recovery)
	SPID      int    `json:"spid"`
	LoginTime string `json:"login_time,omitempty"`
	Marker    string `json:"marker"`
	Command   string `json:"command"`
	StartedAt string `json:"started_at"`
	// ResumeFromOp is the resume cursor: the number of operations already completed
	// (the next op to run), left by a drained or interrupted run so the next run skips
	// what is done. 0 (omitted) means start from the first operation.
	ResumeFromOp int `json:"resume_from_op,omitempty"`
	// PlanFingerprint identifies the plan the resume cursor was recorded against: a hash
	// over the ordered (command, target) of every planned operation. A resumed run whose
	// current plan hashes differently ignores the stale cursor and restarts from the first
	// operation, so a shortened, reordered, or re-expanded manifest is never silently
	// skipped (which would report SUCCESS having executed nothing). Empty on a legacy
	// sidecar written before this field existed.
	PlanFingerprint string `json:"plan_fingerprint,omitempty"`
	// Paused records an operation this manifest left with a paused resumable rebuild on the
	// server, so a resumed run continues exactly that operation via ALTER INDEX … RESUME by
	// recorded identity — not by inferring ownership from the cursor position (which fails
	// when a continue-on-failure gap freezes the cursor before the interrupted op). Nil when
	// nothing was left paused.
	Paused *PausedResumable `json:"paused,omitempty"`
}

// PausedResumable identifies the operation and target index of a paused resumable rebuild the
// engine left on the server, so a resumed run can positively recognize its own paused work
// (RESUME it) versus a foreign paused resumable (reject or ABORT per the opt-in).
type PausedResumable struct {
	Op     int    `json:"op"` // the plan index of the interrupted operation
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Index  string `json:"index"`
}

// WriteState writes the sidecar state as indented JSON. The write is atomic (temp
// file + rename) because the engine rewrites it after every operation to advance the
// resume cursor: a crash mid-write must never leave a torn sidecar that recovery
// cannot parse.
func WriteState(path string, s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run state: %w", err)
	}
	if err := atomicWriteFile(path, data); err != nil {
		return fmt.Errorf("write run state: %w", err)
	}
	return nil
}

// atomicWriteFile writes data to a temp file in path's directory and renames it over
// path, so a reader — or a crash — sees either the old or the new complete file, never
// a partial write. Used for the crash-recovery sidecars.
func atomicWriteFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".sqlgopace-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ReadState reads a sidecar state file.
func ReadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("read run state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("unmarshal run state: %w", err)
	}
	return s, nil
}

// RemoveState deletes a sidecar state file, ignoring a missing file.
func RemoveState(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove run state: %w", err)
	}
	return nil
}
