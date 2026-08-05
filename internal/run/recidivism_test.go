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
