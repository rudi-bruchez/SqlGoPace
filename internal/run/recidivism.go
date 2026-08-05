package run

import "time"

// recidivismWindow is how long an offender identity's accumulated blocking debt survives
// without a new block before it is forgotten. Not configurable, for the same reason
// killGraceWindow is not: it separates "one episode of trouble seen through a succession of
// sessions" from "this came back an hour later", and that boundary is a property of the
// mechanism rather than something an operator tunes per run.
const recidivismWindow = 5 * time.Minute

// maxRepeatKills bounds how many times one identity may be killed inside the window before
// the run stops killing it and escalates to the operator. Every KILL buys a rollback, and a
// rollback of a large maintenance statement can cost more than the block it ends; a killer
// that never gives up would trade a blocked run for an unbounded rollback storm. Because the
// counter lives in the bucket, it is forgotten with the debt — this is a rate limit (3 kills
// per identity per window), not a per-manifest quota that would permanently disarm a long run.
const maxRepeatKills = 3

// bucket is one offender identity's accumulated blocking debt inside the window.
type bucket struct {
	accrued    time.Duration // blocking time already observed under this identity
	kills      int
	escalated  bool // the cap warning has been emitted for this bucket
	lastActive time.Time
}

// recidivism accumulates blocking debt per offender identity so a killed offender that
// returns under a new SPID does not buy a fresh dwell. It carries no lock of its own: both
// callers (BlockerKiller, VictimKiller) already hold theirs across every call.
type recidivism struct {
	now     func() time.Time
	buckets map[string]*bucket
}

// newRecidivism returns an empty accumulator. A nil clock defaults to time.Now.
func newRecidivism(now func() time.Time) *recidivism {
	if now == nil {
		now = time.Now
	}
	return &recidivism{now: now, buckets: make(map[string]*bucket)}
}

// touch returns key's bucket, creating it, and marks it active now. Only the mutating
// methods call it: a plain read must not keep a bucket alive past the window.
func (r *recidivism) touch(key string) *bucket {
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{}
		r.buckets[key] = b
	}
	b.lastActive = r.now()
	return b
}

// accrue banks d against key. A non-positive d still marks the identity active, which is
// what an episode's first poll (contributing zero) needs.
func (r *recidivism) accrue(key string, d time.Duration) {
	b := r.touch(key)
	if d > 0 {
		b.accrued += d
	}
}

// debt reports the blocking time banked under key.
func (r *recidivism) debt(key string) time.Duration {
	if b, ok := r.buckets[key]; ok {
		return b.accrued
	}
	return 0
}

// kills reports how many kills key has spent inside the window.
func (r *recidivism) kills(key string) int {
	if b, ok := r.buckets[key]; ok {
		return b.kills
	}
	return 0
}

// recordKill spends one kill from key's budget and returns the new count. VictimKiller calls
// it as a reservation while selecting a target (its KILLs are issued after the lock is
// released, and a scan can select several victims of one identity at once); BlockerKiller
// calls it after a successful KILL.
func (r *recidivism) recordKill(key string) int {
	b := r.touch(key)
	b.kills++
	return b.kills
}

// undoKill returns a reservation whose KILL never landed. It never goes negative.
func (r *recidivism) undoKill(key string) {
	if b, ok := r.buckets[key]; ok && b.kills > 0 {
		b.kills--
	}
}

// escalate reports whether the caller should emit the cap warning: true the first time for
// this bucket, false afterwards, so a capped offender does not warn on every poll.
func (r *recidivism) escalate(key string) bool {
	b := r.touch(key)
	if b.escalated {
		return false
	}
	b.escalated = true
	return true
}

// prune forgets every bucket idle for longer than recidivismWindow. Callers run it at the
// top of a consideration, before any debt is read, so an offender returning after a quiet
// window is judged against a pruned map and serves the full dwell again.
func (r *recidivism) prune() {
	now := r.now()
	for key, b := range r.buckets {
		if now.Sub(b.lastActive) > recidivismWindow {
			delete(r.buckets, key)
		}
	}
}

// reset drops every bucket. Called when a killer is re-armed or disarmed: debt is
// per-manifest state and is never carried across manifests.
func (r *recidivism) reset() {
	r.buckets = make(map[string]*bucket)
}
