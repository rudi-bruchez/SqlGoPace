package run

import (
	"encoding/json"
	"fmt"
	"os"
)

// RunState is the sidecar written next to a manifest while it executes. After a
// crash it lets recovery correlate an orphaned session to its run (SPID +
// login_time + CONTEXT_INFO marker) and decide whether to resume or abandon.
type RunState struct {
	Manifest  string `json:"manifest"`
	SPID      int    `json:"spid"`
	LoginTime string `json:"login_time,omitempty"`
	Marker    string `json:"marker"`
	Command   string `json:"command"`
	StartedAt string `json:"started_at"`
}

// WriteState writes the sidecar state as indented JSON.
func WriteState(path string, s RunState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run state: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write run state: %w", err)
	}
	return nil
}

// ReadState reads a sidecar state file.
func ReadState(path string) (RunState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RunState{}, fmt.Errorf("read run state: %w", err)
	}
	var s RunState
	if err := json.Unmarshal(data, &s); err != nil {
		return RunState{}, fmt.Errorf("unmarshal run state: %w", err)
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
