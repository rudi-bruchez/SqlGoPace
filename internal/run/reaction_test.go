package run_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

func TestDecideReaction(t *testing.T) {
	tests := []struct {
		name string
		p    run.Pressure
		c    run.Capabilities
		want run.Action
	}{
		{"no pressure", run.Pressure{}, run.Capabilities{Resumable: true}, run.Continue},
		{"blocking, resumable -> pause", run.Pressure{BlockingOthers: true}, run.Capabilities{Resumable: true}, run.Pause},
		{"blocking, not resumable -> cancel", run.Pressure{BlockingOthers: true}, run.Capabilities{}, run.Cancel},
		{"log over cap, resumable -> pause", run.Pressure{LogOverCap: true}, run.Capabilities{Resumable: true}, run.Pause},
		{"log over cap, not resumable -> cancel", run.Pressure{LogOverCap: true}, run.Capabilities{ADR: true}, run.Cancel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run.DecideReaction(tt.p, tt.c); got != tt.want {
				t.Errorf("DecideReaction(%+v, %+v) = %v, want %v", tt.p, tt.c, got, tt.want)
			}
		})
	}
}

func TestPressureDetailNamesTheLogBreach(t *testing.T) {
	p := run.Pressure{LogOverCap: true, LogReuseWait: "LOG_BACKUP",
		LogBreach: "used 52.0 GB, 20% of a 260.0 GB file, over the 50.0 GB cap (monitoring.log_max_size_bytes)"}
	want := "transaction log over cap: used 52.0 GB, 20% of a 260.0 GB file, over the 50.0 GB cap (monitoring.log_max_size_bytes) (reuse_wait=LOG_BACKUP)"
	if got := p.Detail(); got != want {
		t.Errorf("Detail() = %q\nwant       %q", got, want)
	}
}
