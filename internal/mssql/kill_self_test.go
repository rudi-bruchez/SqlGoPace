package mssql

import (
	"errors"
	"strings"
	"testing"
)

// The fallback KILL aims at a session number, and a session number is the one thing about
// our execution session that does not survive: a canceled statement can poison the pinned
// connection, which is re-pinned onto a new session, and SQL Server hands the freed number
// to whoever logs in next. Killing on the number alone can therefore end a stranger's
// transaction. stopOrphan has compared login_time since it was written; this is the same
// comparison, for the path that was still killing blind.
//
// The policy is the one stopOrphan already sets: anything but a positive match declines.
// An unanswerable probe is "cannot tell", and cannot tell is not permission.
func TestKillSelfDecision(t *testing.T) {
	const want = "2026-09-17T08:00:00"

	cases := []struct {
		name  string
		id    SessionIdentity
		err   error
		ok    bool
		wants string // substring the refusal must carry, when it refuses
	}{
		{
			name: "same session",
			id:   SessionIdentity{Exists: true, Active: true, LoginTime: want},
			ok:   true,
		},
		{
			name:  "session id reassigned to another login",
			id:    SessionIdentity{Exists: true, Active: true, LoginTime: "2026-09-17T09:15:00"},
			wants: "login_time",
		},
		{
			name:  "session is gone",
			id:    SessionIdentity{Exists: false},
			wants: "no longer exists",
		},
		{
			name:  "probe could not answer",
			err:   errors.New("read session identity: connection reset by peer"),
			wants: "could not be verified",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, why := killSelfDecision(c.id, c.err, want)
			if ok != c.ok {
				t.Fatalf("killSelfDecision() ok = %t, want %t (why=%q)", ok, c.ok, why)
			}
			if c.ok {
				return
			}
			if !strings.Contains(why, c.wants) {
				t.Errorf("refusal = %q, want it to mention %q", why, c.wants)
			}
		})
	}
}

// An idle session is still ours, and still worth killing: the statement we are trying to
// stop may have finished its forward work and be rolling back, which shows no request on
// some paths. Only identity decides.
func TestKillSelfDecisionAllowsAnIdleOwnSession(t *testing.T) {
	const want = "2026-09-17T08:00:00"
	if ok, why := killSelfDecision(SessionIdentity{Exists: true, Active: false, LoginTime: want}, nil, want); !ok {
		t.Errorf("killSelfDecision(idle own session) refused: %q", why)
	}
}

// Without a recorded login_time there is nothing to compare against, so there is no
// positive match to be had and the kill is declined rather than issued on hope.
func TestKillSelfDecisionRefusesWithoutARecordedLoginTime(t *testing.T) {
	ok, why := killSelfDecision(SessionIdentity{Exists: true, Active: true, LoginTime: "2026-09-17T08:00:00"}, nil, "")
	if ok {
		t.Fatal("killSelfDecision() allowed a kill with no recorded login_time to compare")
	}
	if !strings.Contains(why, "login_time") {
		t.Errorf("refusal = %q, want it to name the missing comparison", why)
	}
}
