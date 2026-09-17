package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// failingSampler fails its blocking poll for the first failures calls, then succeeds. Its
// log poll always succeeds, so a test can tell the two channels apart.
type failingSampler struct {
	mu       sync.Mutex
	failures int
	calls    int
}

func (s *failingSampler) Blocking(context.Context, IgnoredSessions) (BlockState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.failures {
		return BlockState{}, errors.New("read active sessions: connection reset by peer")
	}
	return BlockState{}, nil
}

func (s *failingSampler) Log(context.Context) (LogSample, error) { return LogSample{}, nil }

func collect(mu *sync.Mutex, out *[]ReactionEvent) ReactionSink {
	return func(e ReactionEvent) {
		mu.Lock()
		defer mu.Unlock()
		*out = append(*out, e)
	}
}

// A monitoring read that fails is the reaction hierarchy going blind: pumpSamples keeps the
// last known state, so a poll that stops answering while nothing was blocking means nothing
// ever reacts again. It used to be dropped by an `if err == nil` with no else — no log line,
// no counter, nothing in the run report. It must say so, once per outage rather than once
// per poll.
func TestPumpSamplesReportsAFailedPollOncePerOutage(t *testing.T) {
	sampler := &failingSampler{failures: 5}
	var mu sync.Mutex
	var events []ReactionEvent

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	samples := make(chan Sample)
	go func() {
		for range samples { // drain, so the pump is never blocked on its send
		}
	}()
	go pumpSamples(ctx, samples, pumpSpec{sampler: sampler, blockEvery: time.Millisecond, logEvery: time.Hour, sink: collect(&mu, &events)})

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		warns, infos := count(events, "warn"), count(events, "info")
		mu.Unlock()
		if warns >= 1 && infos >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no warn+recovery pair after 2s: %+v", events)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if got := count(events, "warn"); got != 1 {
		t.Errorf("warn events = %d, want exactly 1 for one outage of five failed polls", got)
	}
	if got := count(events, "info"); got != 1 {
		t.Errorf("recovery events = %d, want exactly 1", got)
	}
	for _, e := range events {
		if !strings.Contains(e.Detail, "blocking poll") {
			t.Errorf("event %+v does not name the channel that failed", e)
		}
	}
	if w := first(events, "warn"); !strings.Contains(w.Detail, "connection reset by peer") {
		t.Errorf("warn does not carry the server's own error: %q", w.Detail)
	}
}

func count(events []ReactionEvent, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func first(events []ReactionEvent, kind string) ReactionEvent {
	for _, e := range events {
		if e.Kind == kind {
			return e
		}
	}
	return ReactionEvent{}
}

// A pump whose polls all succeed must stay silent: the warning is for an outage, and a
// channel that reports every healthy poll is one an operator learns to ignore.
func TestPumpSamplesIsSilentWhilePollsSucceed(t *testing.T) {
	sampler := &failingSampler{failures: 0}
	var mu sync.Mutex
	var events []ReactionEvent

	ctx, cancel := context.WithCancel(context.Background())
	samples := make(chan Sample)
	go func() {
		for range samples {
		}
	}()
	go pumpSamples(ctx, samples, pumpSpec{sampler: sampler, blockEvery: time.Millisecond, logEvery: time.Hour, sink: collect(&mu, &events)})
	time.Sleep(50 * time.Millisecond)
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 0 {
		t.Errorf("healthy polls produced %d event(s): %+v", len(events), events)
	}
}
