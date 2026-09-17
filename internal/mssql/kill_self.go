package mssql

import (
	"context"
	"errors"
	"fmt"
)

// ErrKillDeclined reports that a self-KILL was not issued because the session could not be
// confirmed to still be ours. It is not a failure to retry past: the caller keeps waiting
// for the statement it was trying to stop, and says so.
var ErrKillDeclined = errors.New("fallback KILL declined")

// killSelfDecision reports whether the session with the recorded id may be killed as our
// own, and when it may not, why — for the operator, who is about to be told that the
// fallback did not fire.
//
// Anything but a positive match declines, which is the policy stopOrphan already applies
// to the same question: a probe that could not answer means "cannot tell", and cannot tell
// is not permission to end somebody's transaction. Liveness is deliberately not part of
// it — an own session with no current request is still ours, and a statement in rollback
// is exactly the case where the fallback is most wanted.
func killSelfDecision(id SessionIdentity, err error, wantLogin string) (bool, string) {
	switch {
	case err != nil:
		return false, fmt.Sprintf("the session could not be verified (%v)", err)
	case wantLogin == "":
		return false, "no login_time was recorded for the execution session, so it cannot be told from a reused session id"
	case !id.Exists:
		return false, "the session no longer exists"
	case id.LoginTime != wantLogin:
		return false, fmt.Sprintf("the session id now belongs to a different session (login_time %s, expected %s)", id.LoginTime, wantLogin)
	default:
		return true, ""
	}
}

// KillSelf issues KILL against our own execution session, and only against our own: it
// re-reads the session's signature and compares it with the login_time recorded when the
// session was pinned. The session id alone is not identity — a canceled statement can
// poison the pinned connection, which is then re-pinned onto a new session, and SQL Server
// hands the freed id to the next login — so killing on the number would end a stranger's
// transaction on a server that has just been under enough stress to need a fallback KILL.
//
// It returns an error wrapping ErrKillDeclined when it refuses, so the caller can tell a
// deliberate refusal from a server that rejected the KILL.
func (c *Conn) KillSelf(ctx context.Context) error {
	c.mu.Lock()
	spid, login := c.spid, c.loginTime
	c.mu.Unlock()

	if spid <= 0 {
		return fmt.Errorf("%w: no execution session id is known", ErrKillDeclined)
	}
	id, err := c.SessionIdentity(ctx, spid)
	if ok, why := killSelfDecision(id, err, login); !ok {
		return fmt.Errorf("%w for SPID %d: %s", ErrKillDeclined, spid, why)
	}
	return c.Kill(ctx, spid)
}
