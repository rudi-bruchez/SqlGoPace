package main

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
)

func TestCheckpointBetweenOperations(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		recovery string
		want     bool
	}{
		{"off under simple", false, "SIMPLE", false},
		{"on under simple", true, "SIMPLE", true},
		{"on under full does nothing", true, "FULL", false},
		{"on under bulk_logged does nothing", true, "BULK_LOGGED", false},
		{"case is the server's, not ours", true, "simple", true},
		{"unknown model is not assumed simple", true, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Monitoring.CheckpointBetweenOperations = tt.enabled
			if got := checkpointBetweenOperations(cfg, tt.recovery); got != tt.want {
				t.Errorf("checkpointBetweenOperations(%t, %q) = %t, want %t", tt.enabled, tt.recovery, got, tt.want)
			}
		})
	}
}

// A key that is set and silently does nothing is what made this defect: say so at startup
// rather than letting the operator believe the log is being released.
func TestCheckpointIneffectiveWarning(t *testing.T) {
	cfg := &config.Config{}
	cfg.Monitoring.CheckpointBetweenOperations = true

	if w := checkpointIneffectiveWarning(cfg, "FULL"); !strings.Contains(w, "FULL") || !strings.Contains(w, "checkpoint_between_operations") {
		t.Errorf("warning should name the key and the model, got %q", w)
	}
	if w := checkpointIneffectiveWarning(cfg, "SIMPLE"); w != "" {
		t.Errorf("no warning is due under SIMPLE, got %q", w)
	}
	cfg.Monitoring.CheckpointBetweenOperations = false
	if w := checkpointIneffectiveWarning(cfg, "FULL"); w != "" {
		t.Errorf("no warning is due when the key is off, got %q", w)
	}
}
