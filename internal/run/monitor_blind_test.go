package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// hangingSampler never returns from Blocking until released, while Log keeps answering.
// That is the shape of the failure this file is about: a poll that hangs is not a poll
// that errors, so nothing in the error path ever runs.
type hangingSampler struct {
	release  chan struct{}
	mu       sync.Mutex
	logCalls int
}

func (s *hangingSampler) Blocking(ctx context.Context, _ IgnoredSessions) (BlockState, error) {
	select {
	case <-s.release:
		return BlockState{}, nil
	case <-ctx.Done():
		return BlockState{}, ctx.Err()
	}
}

func (s *hangingSampler) Log(context.Context) (LogSample, error) {
	s.mu.Lock()
	s.logCalls++
	s.mu.Unlock()
	return LogSample{}, nil
}

func (s *hangingSampler) logs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logCalls
}

// A monitoring read that hangs is the dangerous half of going blind, and the half
// nothing saw before 0.41.0: pollHealth only observes an error, and a read waiting on
// THREADPOOL returns neither a value nor an error. There is no query timeout anywhere,
// by design, so nothing bounds it. The pump must notice that a channel has produced no
// successful read for blindAfter and say which one.
func TestPumpSamplesReportsAChannelThatStopsAnswering(t *testing.T) {
	sampler := &hangingSampler{release: make(chan struct{})}
	var mu sync.Mutex
	var events []ReactionEvent

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	samples := make(chan Sample)
	go pumpSamples(ctx, samples, pumpSpec{
		sampler:    sampler,
		blockEvery: time.Millisecond,
		logEvery:   time.Millisecond,
		sink:       collect(&mu, &events),
		blindAfter: 60 * time.Millisecond,
	})

	deadline := time.After(3 * time.Second)
	for {
		select {
		case s := <-samples:
			if s.Blind == "" {
				continue
			}
			if !strings.Contains(s.Blind, "blocking") {
				t.Fatalf("Sample.Blind = %q, want it to name the blocking poll", s.Blind)
			}
			// The log channel must have kept answering throughout: one poller hanging
			// may not take the other down with it.
			if n := sampler.logs(); n < 2 {
				t.Errorf("log poll ran %d time(s) while the blocking poll hung, want it to keep running", n)
			}
			mu.Lock()
			warns := count(events, "warn")
			mu.Unlock()
			if warns != 1 {
				t.Errorf("warn events = %d, want exactly 1 naming the blind channel", warns)
			}
			return
		case <-deadline:
			t.Fatal("no blind sample after 3s")
		}
	}
}

// A channel that answers again clears the blindness, so a transient stall does not
// condemn an operation that is being watched perfectly well by the time it matters.
func TestPumpSamplesClearsBlindnessWhenTheChannelAnswersAgain(t *testing.T) {
	sampler := &hangingSampler{release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	samples := make(chan Sample)
	go pumpSamples(ctx, samples, pumpSpec{
		sampler:    sampler,
		blockEvery: time.Millisecond,
		logEvery:   time.Millisecond,
		blindAfter: 60 * time.Millisecond,
	})

	deadline := time.After(3 * time.Second)
	blindSeen := false
	for {
		select {
		case s := <-samples:
			if !blindSeen && s.Blind != "" {
				blindSeen = true
				close(sampler.release) // the server starts answering again
				continue
			}
			if blindSeen && s.Blind == "" {
				return
			}
		case <-deadline:
			t.Fatalf("blindness never cleared (blindSeen=%t)", blindSeen)
		}
	}
}

// Going blind is not pressure to wait out — it is the loss of the ability to see
// pressure at all. A resumable operation is normally paused and resumed once things
// clear, but "clear" is exactly what cannot be observed here, so the decision is a
// cancel whatever the operation supports.
func TestSuperviseCancelsWhenAChannelGoesBlind(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	out := runSupervise(NewManualClock(testStart), Capabilities{Resumable: true}, samples, done)

	sendSample(t, samples, Sample{Blind: "blocking poll"})

	got := awaitSupervise(t, out)
	if got.action != Cancel {
		t.Errorf("supervise() = %v, want Cancel even for a resumable operation", got.action)
	}
	if got.pressure.Blind != "blocking poll" {
		t.Errorf("pressure.Blind = %q, want the channel name carried through for the report", got.pressure.Blind)
	}
	if d := got.pressure.Detail(); !strings.Contains(d, "blocking poll") {
		t.Errorf("pressure detail = %q, want it to name the channel that went blind", d)
	}
}

// ignore_blocking says "hold the lock through blocking". It cannot also mean "run
// unwatched": the operator suppressed a reaction to something seen, not the ability
// to see.
func TestSuperviseCancelsOnBlindnessEvenWithIgnoreBlocking(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	out := runSupervise(NewManualClock(testStart), Capabilities{IgnoreBlocking: true}, samples, done)

	sendSample(t, samples, Sample{Blind: "log poll"})

	if got := awaitSupervise(t, out); got.action != Cancel {
		t.Errorf("supervise() = %v, want Cancel — ignore_blocking does not waive monitoring", got.action)
	}
}

// waitForRelief is where a paused operation waits for pressure to clear. Relief is
// read from the same pump, so a blind pump means waiting forever on a report that
// cannot arrive. It must give up instead, with the reason.
func TestWaitForReliefGivesUpWhenTheMonitorIsBlind(t *testing.T) {
	samples := make(chan Sample)
	out := make(chan error, 1)
	go func() {
		out <- waitForRelief(context.Background(), NewManualClock(testStart), time.Hour, samples, func(ReactionEvent) {})
	}()

	sendSample(t, samples, Sample{BlockingOthers: true, Blind: "blocking poll"})

	select {
	case err := <-out:
		if !errors.Is(err, ErrMonitorBlind) {
			t.Errorf("waitForRelief() = %v, want ErrMonitorBlind", err)
		}
	case <-time.After(awaitTimeout):
		t.Fatal("waitForRelief waited for relief it could not see")
	}
}

// reorganize_index is paced: a cancel normally waits for relief and re-issues, which
// converges because REORGANIZE resumes from persisted progress. Blind, that loop would
// re-issue into a server nobody is watching. The run stops instead, and Run must not
// retry it either — a retry is another unwatched attempt.
func TestRunLoopDoesNotReissueWhenTheMonitorIsBlind(t *testing.T) {
	reissued := 0
	err := runLoop("REORGANIZE",
		func(string) (Action, error) { return Cancel, ErrMonitorBlind },
		func() error { t.Error("waited for relief while blind"); return nil },
		func() (string, error) { return "", nil },
		func() (string, error) { reissued++; return "REORGANIZE", nil },
	)

	if !errors.Is(err, ErrMonitorBlind) {
		t.Errorf("runLoop() = %v, want ErrMonitorBlind", err)
	}
	if reissued != 0 {
		t.Errorf("re-issued %d time(s) while blind, want 0", reissued)
	}
	if errors.Is(err, ErrCancelled) {
		t.Error("ErrMonitorBlind must not read as ErrCancelled, or Run retries it blind")
	}
}
