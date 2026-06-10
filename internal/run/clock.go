package run

import (
	"sync"
	"time"
)

// Clock abstracts time so the engine's timeouts and intervals are deterministic
// in tests. Production code uses System; tests use ManualClock.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
}

type systemClock struct{}

func (systemClock) Now() time.Time                  { return time.Now() }
func (systemClock) Since(t time.Time) time.Duration { return time.Since(t) }

// System is the real wall-clock.
var System Clock = systemClock{}

// ManualClock is a controllable, concurrency-safe Clock for tests: the engine
// reads it from its supervisor goroutine while the test advances it.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock returns a ManualClock set to t.
func NewManualClock(t time.Time) *ManualClock { return &ManualClock{now: t} }

// Now returns the current manual time.
func (m *ManualClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// Advance moves the manual clock forward by d.
func (m *ManualClock) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = m.now.Add(d)
}

// Since returns the duration between t and the manual now.
func (m *ManualClock) Since(t time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now.Sub(t)
}
