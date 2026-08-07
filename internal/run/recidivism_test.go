package run

import (
	"testing"
	"time"
)

// recidivismFor builds a recidivism whose clock is the variable the test advances.
func recidivismFor(now *time.Time) *recidivism {
	return newRecidivism(func() time.Time { return *now })
}

func TestRecidivismAccruesPerKey(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)

	r.accrue("rule-a", 10*time.Second)
	r.accrue("rule-a", 20*time.Second)
	r.accrue("rule-b", 5*time.Second)

	if got := r.debt("rule-a"); got != 30*time.Second {
		t.Errorf("debt(rule-a) = %s, want 30s", got)
	}
	if got := r.debt("rule-b"); got != 5*time.Second {
		t.Errorf("debt(rule-b) = %s, want 5s", got)
	}
	if got := r.debt("never-seen"); got != 0 {
		t.Errorf("debt of an unknown key = %s, want 0", got)
	}
}

// TestRecidivismAccrueSinceBanksTheIdentitysGap pins the mark on the bucket: the gap the
// poll banks is the identity's own, so it cannot depend on which of an offender's sessions
// the caller happens to charge first, and a second session of the same identity in the same
// poll banks zero by construction rather than by a caller-side "already banked" map.
func TestRecidivismAccrueSinceBanksTheIdentitysGap(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)

	r.accrueSince("k", now) // the identity's first observation: nothing to bank
	if got := r.debt("k"); got != 0 {
		t.Errorf("debt after a first observation = %s, want 0", got)
	}

	now = now.Add(10 * time.Second)
	r.accrueSince("k", now)
	r.accrueSince("k", now)
	if got := r.debt("k"); got != 10*time.Second {
		t.Errorf("debt = %s, want 10s — one poll banks the identity's gap once", got)
	}
}

// TestRecidivismPrunedBucketIsAFirstSightAgain confirms the mark dies with the bucket it
// lives in: a quiet window forgets the identity entirely, so its next observation banks
// nothing rather than the whole silence.
func TestRecidivismPrunedBucketIsAFirstSightAgain(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)
	r.accrueSince("k", now)
	now = now.Add(10 * time.Second)
	r.accrueSince("k", now)

	now = now.Add(recidivismWindow + time.Second)
	r.prune()
	r.accrueSince("k", now)

	if got := r.debt("k"); got != 0 {
		t.Errorf("debt = %s, want 0 — a pruned bucket loses its mark with the rest of its state", got)
	}
}

// TestRecidivismForgetMarksExceptKeepsTheDebt pins the other half of the mark's contract:
// an identity that was not observed this poll must not bank the interval when it comes
// back, but it keeps every second it has already served.
func TestRecidivismForgetMarksExceptKeepsTheDebt(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)
	r.accrueSince("seen", now)
	r.accrueSince("gone", now)
	now = now.Add(10 * time.Second)
	r.accrueSince("seen", now)
	r.accrueSince("gone", now)

	r.forgetMarksExcept(map[string]bool{"seen": true})

	now = now.Add(30 * time.Second)
	r.accrueSince("seen", now)
	r.accrueSince("gone", now)

	if got := r.debt("seen"); got != 40*time.Second {
		t.Errorf("debt(seen) = %s, want 40s", got)
	}
	if got := r.debt("gone"); got != 10*time.Second {
		t.Errorf("debt(gone) = %s, want the 10s already served and nothing for the polls it was absent from", got)
	}
}

func TestRecidivismPrunesOnlyIdleBuckets(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)
	r.accrue("stale", 30*time.Second)

	now = now.Add(recidivismWindow - time.Second)
	r.accrue("fresh", 30*time.Second) // touches "fresh" only
	r.prune()
	if r.debt("stale") != 30*time.Second {
		t.Errorf("a bucket idle for less than the window must survive, debt = %s", r.debt("stale"))
	}

	now = now.Add(2 * time.Second) // "stale" is now window+1s idle, "fresh" is 1s idle
	r.prune()
	if r.debt("stale") != 0 {
		t.Errorf("a bucket idle beyond the window must be pruned, debt = %s", r.debt("stale"))
	}
	if r.debt("fresh") != 30*time.Second {
		t.Errorf("the recently touched bucket must survive, debt = %s", r.debt("fresh"))
	}
}

func TestRecidivismKillBudget(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)

	if n := r.recordKill("k"); n != 1 {
		t.Errorf("first recordKill = %d, want 1", n)
	}
	if n := r.recordKill("k"); n != 2 {
		t.Errorf("second recordKill = %d, want 2", n)
	}
	r.undoKill("k")
	if got := r.kills("k"); got != 1 {
		t.Errorf("undoKill must give the budget back, kills = %d, want 1", got)
	}
	r.undoKill("k")
	r.undoKill("k") // one withdrawal too many must not go negative
	if got := r.kills("k"); got != 0 {
		t.Errorf("kills = %d, want 0 and never negative", got)
	}
}

func TestRecidivismEscalatesOncePerBucket(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)
	r.recordKill("k")

	if !r.escalate("k") {
		t.Fatal("the first escalate must return true")
	}
	if r.escalate("k") {
		t.Error("escalate must return false on every later call for the same bucket")
	}

	// A pruned bucket is a new episode of trouble: it may warn again.
	now = now.Add(recidivismWindow + time.Second)
	r.prune()
	if !r.escalate("k") {
		t.Error("a bucket forgotten by prune must be able to escalate again")
	}
}

func TestRecidivismResetDropsEverything(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)
	r.accrue("k", time.Minute)
	r.recordKill("k")

	r.reset()

	if r.debt("k") != 0 || r.kills("k") != 0 {
		t.Errorf("reset must clear every bucket, debt = %s kills = %d", r.debt("k"), r.kills("k"))
	}
}
