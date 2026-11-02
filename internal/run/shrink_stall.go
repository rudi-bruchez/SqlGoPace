package run

import (
	"fmt"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// noGainCause says why a shrink chunk gained nothing.
type noGainCause int

const (
	causeYield  noGainCause = iota // the run yielded to a blocked session
	causeBackup                    // a data backup is running on the shrunk database
	causeStall                     // neither: the shrink itself is stuck
)

// classifyNoGain names the cause of a chunk with no gain. A yield wins over a
// running backup, and anything else is a stall.
func classifyNoGain(stopped, backupRunning bool) noGainCause {
	switch {
	case stopped:
		return causeYield
	case backupRunning:
		return causeBackup
	default:
		return causeStall
	}
}

// yieldSnapshot is what the monitor saw when the shrink yielded: the sessions our
// DDL was blocking, and our own session's blocker if any.
type yieldSnapshot struct {
	Blocked []mssql.Session
	Self    mssql.SelfBlock
}

// yieldTally counts the yields since the last reset and remembers who was involved.
type yieldTally struct {
	n        int
	logins   []string       // blocked logins, in order first seen
	freq     map[string]int // occurrences per blocked login
	openTx   map[string]bool
	lastSelf mssql.SelfBlock
	hasSelf  bool
}

func (t *yieldTally) add(s yieldSnapshot) {
	t.n++
	if t.freq == nil {
		t.freq = map[string]int{}
		t.openTx = map[string]bool{}
	}
	for _, sess := range s.Blocked {
		if sess.Login == "" {
			continue
		}
		if t.freq[sess.Login] == 0 {
			t.logins = append(t.logins, sess.Login)
		}
		t.freq[sess.Login]++
		if sess.OpenTransactions > 0 {
			t.openTx[sess.Login] = true
		}
	}
	if s.Self.Blocked {
		t.lastSelf, t.hasSelf = s.Self, true
	}
}

// describe summarizes the yields for the give-up reason; empty when there were none.
func (t yieldTally) describe() string {
	if t.n == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "yielded %d times", t.n)
	top, best := "", 0
	for _, l := range t.logins { // strict > keeps the login seen first on a tie
		if t.freq[l] > best {
			top, best = l, t.freq[l]
		}
	}
	if top != "" {
		fmt.Fprintf(&b, " (blocking login=%s", top)
		if t.openTx[top] {
			b.WriteString(", open transaction")
		}
		b.WriteString(")")
	}
	if t.hasSelf {
		fmt.Fprintf(&b, "; chunk itself blocked by session %d, login=%s", t.lastSelf.SPID, t.lastSelf.Login)
	}
	return b.String()
}

// giveUpDetail assembles the give-up reason in this order: who the run yielded to, the
// bound that tripped (with the last chunk error), the step floor when yields drove the
// step there, the tail object, then the promise.
func giveUpDetail(t yieldTally, bound string, chunkErr error, step, minStep int, tail *TailFinding) string {
	var parts []string
	if d := t.describe(); d != "" {
		parts = append(parts, d)
	}
	stalled := "no further progress after " + bound
	if chunkErr != nil {
		stalled = fmt.Sprintf("%s: %v", stalled, chunkErr)
	}
	parts = append(parts, stalled)
	if t.n > 0 && step == minStep {
		parts = append(parts, fmt.Sprintf("step at min_step_mb (%d MB)", step))
	}
	if tail != nil {
		parts = append(parts, fmt.Sprintf("tail object at give-up: %s.%s index_id=%d, %d pages from end",
			tail.Schema, tail.Table, tail.IndexID, tail.PageFromEnd))
	}
	return strings.Join(append(parts, "work preserved"), "; ")
}
