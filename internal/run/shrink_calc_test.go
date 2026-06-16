package run_test

import (
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// tuning returns a ShrinkTuning with the documented defaults, for the calc tests.
func tuning() run.ShrinkTuning {
	return run.ShrinkTuning{
		InitialStepSmallMB:  100,
		InitialStepMediumMB: 250,
		InitialStepLargeMB:  500,
		MinStepMB:           50,
		MaxStepMB:           1024,
		TargetBatch:         5 * time.Second,
		MaxNoProgress:       3,
	}
}

func TestInitialStepMB(t *testing.T) {
	tn := tuning()
	tests := []struct {
		name      string
		reclaimMB int
		want      int
	}{
		{"small under 5GB", 1024, 100},
		{"small just under 5GB", 5*1024 - 1, 100},
		{"medium at 5GB boundary", 5 * 1024, 250},
		{"medium mid range", 20 * 1024, 250},
		{"medium at 50GB boundary", 50 * 1024, 250},
		{"large over 50GB", 50*1024 + 1, 500},
		{"large very big", 500 * 1024, 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run.InitialStepMB(tt.reclaimMB, tn); got != tt.want {
				t.Errorf("InitialStepMB(%d) = %d, want %d", tt.reclaimMB, got, tt.want)
			}
		})
	}
}

func TestAdjustStepMB(t *testing.T) {
	tn := tuning()
	quick := 1 * time.Second // < TargetBatch
	slow := 9 * time.Second  // > TargetBatch
	tests := []struct {
		name    string
		step    int
		elapsed time.Duration
		w       run.WaitDeltas
		want    int
	}{
		{
			name:    "halve on high WRITELOG",
			step:    400,
			elapsed: quick,
			w:       run.WaitDeltas{WriteLogAvgMs: 15},
			want:    200,
		},
		{
			name:    "halve on high PAGEIOLATCH_EX",
			step:    400,
			elapsed: quick,
			w:       run.WaitDeltas{PageIOLatchExAvgMs: 25},
			want:    200,
		},
		{
			name:    "halve on sustained blocking",
			step:    400,
			elapsed: quick,
			w:       run.WaitDeltas{BlockingSeconds: 45},
			want:    200,
		},
		{
			name:    "grow on light io and quick chunk",
			step:    200,
			elapsed: quick,
			w:       run.WaitDeltas{WriteLogAvgMs: 2, PageIOLatchExAvgMs: 3},
			want:    400,
		},
		{
			name:    "no change when chunk slower than target",
			step:    200,
			elapsed: slow,
			w:       run.WaitDeltas{WriteLogAvgMs: 2, PageIOLatchExAvgMs: 3},
			want:    200,
		},
		{
			name:    "halve clamps to MinStepMB",
			step:    50,
			elapsed: quick,
			w:       run.WaitDeltas{WriteLogAvgMs: 15},
			want:    50, // 25 < min 50
		},
		{
			name:    "grow clamps to MaxStepMB",
			step:    1000,
			elapsed: quick,
			w:       run.WaitDeltas{},
			want:    1024, // 2000 > max 1024
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run.AdjustStepMB(tt.step, tt.elapsed, tt.w, tn); got != tt.want {
				t.Errorf("AdjustStepMB(%d, %v, %+v) = %d, want %d", tt.step, tt.elapsed, tt.w, got, tt.want)
			}
		})
	}
}

func TestNextTargetMB(t *testing.T) {
	tests := []struct {
		name                 string
		current, step, final int
		want                 int
	}{
		{"normal step down", 1000, 200, 100, 800},
		{"last chunk clamps to final", 250, 200, 100, 100},
		{"exact landing on final", 300, 200, 100, 100},
		{"already at final", 100, 200, 100, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run.NextTargetMB(tt.current, tt.step, tt.final); got != tt.want {
				t.Errorf("NextTargetMB(%d, %d, %d) = %d, want %d", tt.current, tt.step, tt.final, got, tt.want)
			}
		})
	}
}
