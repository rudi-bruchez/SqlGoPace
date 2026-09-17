package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// deafSampler answers the log poll and never answers the blocking poll, which is what a
// DMV read waiting on THREADPOOL looks like from here: no value, no error, no return.
type deafSampler struct{}

func (deafSampler) Blocking(ctx context.Context, _ IgnoredSessions) (BlockState, error) {
	<-ctx.Done()
	return BlockState{}, ctx.Err()
}

func (deafSampler) Log(context.Context) (LogSample, error) { return LogSample{}, nil }

// The log shrink is the statement with the least to fall back on: no WAIT_AT_LOW_PRIORITY,
// no chunk boundary, nothing but the monitoring loop between it and an unbounded lock on a
// production database. When that loop stops answering, continuing is running blind on the
// one statement that can least afford it — so it is canceled, and the freed space is kept.
func TestShrinkLogStopsWhenMonitoringGoesBlind(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0))
	s := &fakeServer{
		fileType: mssql.FileTypeLog, name: "Log", recovery: "SIMPLE",
		sizeMB: 4000, usedMB: 400, floorMB: 400, blockShrink: true,
	}
	r := newTestRunner(s, clk)
	r.pollIntv = 2 * time.Millisecond
	r.blindAfter = 40 * time.Millisecond
	r.sampler = deafSampler{}

	var mu sync.Mutex
	var reactions []ReactionEvent
	sink := func(e ReactionEvent) { mu.Lock(); reactions = append(reactions, e); mu.Unlock() }

	// Bounded so a blindness check that never fires fails the test instead of hanging it:
	// the fake holds the statement open until something cancels it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	op := ddl.Shrink{Type: "log", Files: "Log", TargetFreeSpace: "10%"}
	got, err := r.Run(ctx, op, ddl.ResolvedOptions{MaxBlockMinutes: 1}, nil, sink)

	if !errors.Is(err, ErrMonitorBlind) {
		t.Fatalf("Run() error = %v, want ErrMonitorBlind", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if !strings.Contains(got[0].Reason, "monitoring") {
		t.Errorf("Reason = %q, want it to say monitoring stopped answering", got[0].Reason)
	}
	// Freed space is preserved, so the reason must not read as a failed shrink.
	if !strings.Contains(got[0].Reason, "freed space preserved") {
		t.Errorf("Reason = %q, want it to say the freed space is kept", got[0].Reason)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range reactions {
		if e.Kind == "cancel" && strings.Contains(e.Detail, "stopped answering") {
			return
		}
	}
	t.Errorf("no cancel event naming the blind channel; got %+v", reactions)
}
