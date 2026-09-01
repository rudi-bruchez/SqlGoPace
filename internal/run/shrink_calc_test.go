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
		MaxChunkDuration:    5 * time.Second,
		MaxNoProgress:       3,
	}
}

// The three wait profiles the stepsize tests are built from. deadBandWaits is the interesting
// one: it sits above the 5 ms ceiling the old law required to grow and below the 10/20 ms floors
// it required to reduce, so it is what used to freeze the step in place.
var (
	cleanWaits    = run.WaitDeltas{WriteLogAvgMs: 2, PageIOLatchExAvgMs: 3}
	deadBandWaits = run.WaitDeltas{WriteLogAvgMs: 7, PageIOLatchExAvgMs: 8}
	pressureWaits = run.WaitDeltas{WriteLogAvgMs: 15}
)

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
	quick := 1 * time.Second // < MaxChunkDuration
	slow := 9 * time.Second  // > MaxChunkDuration
	tests := []struct {
		name    string
		step    int
		elapsed time.Duration
		stopped bool
		w       run.WaitDeltas
		want    int
	}{
		{
			name:    "halve on high WRITELOG",
			step:    400,
			elapsed: quick,
			w:       pressureWaits,
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
			// The supervisor cut the chunk short, so we were hurting the server: back off
			// even though the latency we saw ourselves was fine.
			name:    "halve when the chunk was stopped under pressure",
			step:    400,
			elapsed: quick,
			stopped: true,
			w:       cleanWaits,
			want:    200,
		},
		{
			// Both values sit in the former dead band: above the old 5 ms grow ceiling,
			// below the 10/20 ms reduce floors. This is what used to freeze the step.
			name:    "grow in the former dead band",
			step:    400,
			elapsed: quick,
			w:       run.WaitDeltas{WriteLogAvgMs: 7, PageIOLatchExAvgMs: 15},
			want:    500,
		},
		{
			name:    "grow on light io and quick chunk",
			step:    200,
			elapsed: quick,
			w:       cleanWaits,
			want:    250,
		},
		{
			name:    "hold at the duration ceiling",
			step:    400,
			elapsed: tn.MaxChunkDuration,
			w:       cleanWaits,
			want:    400,
		},
		{
			name:    "hold above the duration ceiling",
			step:    400,
			elapsed: slow,
			w:       cleanWaits,
			want:    400,
		},
		{
			name:    "halve clamps to MinStepMB",
			step:    50,
			elapsed: quick,
			w:       pressureWaits,
			want:    50, // 25 < min 50
		},
		{
			name:    "grow clamps to MaxStepMB",
			step:    1000,
			elapsed: quick,
			w:       cleanWaits,
			want:    1024, // 1250 > max 1024
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// pct=0 in tuning() so the effective ceiling equals MaxStepMB (1024).
			if got := chunk(tt.step, tt.elapsed, tt.w, tt.stopped, tn); got != tt.want {
				t.Errorf("AdjustStepMB(%d, %v, %+v, stopped=%v) = %d, want %d",
					tt.step, tt.elapsed, tt.w, tt.stopped, got, tt.want)
			}
		})
	}
}

// longBatch is the regime the controller is designed for: a duration ceiling well above the
// time a multi-GB chunk takes, so the hold rule does not mask the growth path.
func longBatch() run.ShrinkTuning {
	tn := tuning()
	tn.MaxChunkDuration = 300 * time.Second
	return tn
}

// chunk applies one chunk's worth of adjustment at the tuning's own ceiling.
func chunk(step int, elapsed time.Duration, w run.WaitDeltas, stopped bool, tn run.ShrinkTuning) int {
	return run.AdjustStepMB(step, elapsed, w, stopped, tn, tn.MaxStepMB)
}

// TestAdjustStepMBRecoversAfterPressure is the regression for the production report. A
// single pressure event used to pin the step forever, because growth also demanded that the
// chunk finish inside MaxChunkDuration — unsatisfiable for a multi-GB shrink chunk. The recovery
// chunks here use deadBandWaits, the latency profile that froze the old law in both directions.
func TestAdjustStepMBRecoversAfterPressure(t *testing.T) {
	tn := longBatch()
	elapsed := 60 * time.Second // comfortably inside the 300 s ceiling

	step := chunk(400, elapsed, pressureWaits, false, tn)
	if step != 200 {
		t.Fatalf("pressure chunk: step = %d, want 200", step)
	}
	for range 10 {
		step = chunk(step, elapsed, deadBandWaits, false, tn)
	}
	if step != tn.MaxStepMB {
		t.Errorf("after 10 healthy chunks step = %d, want the ceiling %d: the step must recover",
			step, tn.MaxStepMB)
	}
}

// TestAdjustStepMBEquilibriumUnderPeriodicPressure pins the stability ratio: recovering one
// halving costs log2/log1.25 ~= 3.1 clean chunks, so a run meeting pressure less often than
// one chunk in three must trend upward instead of walking down to the floor. Lowering the
// growth factor without re-deriving that ratio breaks this test, which is the point.
func TestAdjustStepMBEquilibriumUnderPeriodicPressure(t *testing.T) {
	tn := longBatch()
	elapsed := 60 * time.Second

	step := 400
	for range 6 {
		step = chunk(step, elapsed, pressureWaits, false, tn)
		for range 4 {
			step = chunk(step, elapsed, deadBandWaits, false, tn)
		}
	}
	if step < 400 {
		t.Errorf("step = %d after 6 pressure cycles, want >= 400: one halving per four clean "+
			"chunks has to be a net gain", step)
	}
}

// TestAdjustStepMBReductionAboveCeilingIsPermanent documents the accepted limit of the design:
// the ratchet is bounded by MaxChunkDuration, not removed. A step whose chunk still runs past the
// ceiling after a halving is held there deliberately — the ceiling means "no chunk longer than
// this". Making this case recover would reintroduce unbounded growth.
func TestAdjustStepMBReductionAboveCeilingIsPermanent(t *testing.T) {
	tn := longBatch()
	tn.MaxStepMB = 8192
	overCeiling := 600 * time.Second

	if got := chunk(8192, overCeiling, cleanWaits, false, tn); got != 8192 {
		t.Errorf("clean chunk over the ceiling: step = %d, want 8192 held", got)
	}
	step := chunk(8192, overCeiling, pressureWaits, false, tn)
	if step != 4096 {
		t.Fatalf("pressure chunk: step = %d, want 4096", step)
	}
	if got := chunk(step, tn.MaxChunkDuration, cleanWaits, false, tn); got != 4096 {
		t.Errorf("clean chunk still at the ceiling: step = %d, want 4096 (not recovered)", got)
	}
}

// TestAdjustStepMBGrowthAlwaysAdvances guards the integer truncation in the growth factor: the
// growth path must never hand back the step it was given, or the controller stalls. The sweep
// starts at a MinStepMB of 1 on purpose — step*5/4 truncates back to step only below 4, so a
// sweep from the default floor of 50 would exercise everything except the case being guarded.
func TestAdjustStepMBGrowthAlwaysAdvances(t *testing.T) {
	tn := longBatch()
	tn.MinStepMB = 1
	for step := tn.MinStepMB; step < tn.MaxStepMB; step++ {
		if got := chunk(step, time.Second, cleanWaits, false, tn); got <= step {
			t.Fatalf("growth from %d returned %d, want > %d", step, got, step)
		}
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
