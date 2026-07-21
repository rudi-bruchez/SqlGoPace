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
			// A huge file size with MaxStepPctOfFile=0 (see tuning()) keeps the effective
			// ceiling at MaxStepMB, so the legacy tier value is returned unchanged.
			if got := run.InitialStepMB(tt.reclaimMB, 100*1024*1024, tn); got != tt.want {
				t.Errorf("InitialStepMB(%d) = %d, want %d", tt.reclaimMB, got, tt.want)
			}
		})
	}
}

func TestInitialStepMBTargetChunks(t *testing.T) {
	tn := run.ShrinkTuning{TargetChunks: 1000, MaxStepPctOfFile: 5, MinStepMB: 50, MaxStepMB: 8192}
	tests := []struct {
		name       string
		reclaimMB  int
		fileSizeMB int
		want       int
	}{
		// 7 TB reclaim / 1000 = ceil(7340032/1000) = 7341 MB (~7.2 GB) chunks, under the 8 GB
		// ceiling — so the whole shrink is ~1000 moves instead of tens of thousands.
		{"multi-TB reclaim bounded to ~target chunks", 7 * 1024 * 1024, 16 * 1024 * 1024, 7341},
		// Small reclaim: 2 GB / 1000 = ~3 MB, clamped up to the MinStepMB floor.
		{"tiny reclaim floors at min step", 2 * 1024, 10 * 1024, 50},
		// Mid reclaim on a small file: 5% of a 20 GB file (1024 MB) caps the step.
		{"file-percent ceiling caps the step", 10 * 1024 * 1024, 20 * 1024, 1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run.InitialStepMB(tt.reclaimMB, tt.fileSizeMB, tn); got != tt.want {
				t.Errorf("InitialStepMB(reclaim=%d, file=%d) = %d, want %d", tt.reclaimMB, tt.fileSizeMB, got, tt.want)
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
			// pct=0 in tuning() so the effective ceiling equals MaxStepMB (1024).
			if got := run.AdjustStepMB(tt.step, tt.elapsed, tt.w, tn, tn.MaxStepMB); got != tt.want {
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
