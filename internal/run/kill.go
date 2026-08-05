package run

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// KillEvent describes a blocker the engine terminated, for console/log notification.
type KillEvent struct {
	SPID   int
	Login  string
	Waited time.Duration // how long it blocked the DDL before the kill
}

// killRule is one compiled kill_blocking_sessions entry: a session matcher (shared with the
// ignore rules), the delay the blocker must persist before it is killed, and the identity
// key its blocking debt accumulates under. The key is computed here, once per compile, not
// per poll: compileKilledSessions rebuilds every killRule on each reload, so a compile-time
// key has exactly the matcher's lifetime and cannot go stale.
type killRule struct {
	match sessionRule
	after time.Duration
	key   string
}

// KillSource provides the current compiled kill rules. Like IgnoreSource it is re-read live
// so a rule appended to the manifest mid-run (by hand or by the TUI) takes effect on the
// next blocking poll — before the operation would abort — without a restart.
type KillSource interface {
	Current() []killRule
}

// staticKill is a KillSource over a fixed compiled set: no live reload (reload disabled/tests).
type staticKill struct{ rules []killRule }

func (s staticKill) Current() []killRule { return s.rules }

// compileKilledSessions compiles manifest kill rules into matchers, applying def as the
// delay for any rule that sets none. The match projection reuses CompileIgnoredSessions so
// the kill and ignore rule sets share one matcher implementation.
func compileKilledSessions(rules []ddl.KilledSession, def time.Duration) ([]killRule, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	igs := make([]ddl.IgnoredSession, len(rules))
	for i, r := range rules {
		igs[i] = r.IgnoredSession
	}
	compiled, err := CompileIgnoredSessions(igs)
	if err != nil {
		return nil, err
	}
	out := make([]killRule, len(compiled))
	for i := range compiled {
		after := time.Duration(rules[i].AfterSeconds) * time.Second
		if after <= 0 {
			after = def
		}
		out[i] = killRule{match: compiled[i], after: after, key: compiled[i].key()}
	}
	return out, nil
}

// manifestKillSource re-reads a manifest's kill_blocking_sessions from disk on demand, so a
// rule added during the run (by hand or the TUI) is honored on the next poll. It is
// mtime-gated and keeps the last good rules when the file is unreadable or mid-write —
// the exact behavior of manifestSource for ignore rules.
type manifestKillSource struct {
	path string
	def  time.Duration
	mu   sync.Mutex
	mod  time.Time
	cur  []killRule
}

func newManifestKillSource(path string, def time.Duration, initial []killRule) *manifestKillSource {
	s := &manifestKillSource{path: path, def: def, cur: initial}
	if info, err := os.Stat(path); err == nil {
		s.mod = info.ModTime()
	}
	return s
}

func (s *manifestKillSource) Current() []killRule {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Stat(s.path)
	if err != nil {
		return s.cur // file gone/unreadable: keep the last good rules
	}
	mod := info.ModTime()
	if !mod.After(s.mod) {
		return s.cur // unchanged since last read
	}
	s.mod = mod
	m, err := ddl.LoadManifestFile(s.path)
	if err != nil {
		return s.cur // mid-edit / invalid: keep the last good rules
	}
	rules, err := compileKilledSessions(m.KillBlockingSessions, s.def)
	if err != nil {
		return s.cur
	}
	s.cur = rules
	return s.cur
}

// BlockerKiller terminates sessions that block this run's DDL and match a kill rule, once
// they have blocked continuously for the rule's delay. It is consulted from
// ServerSampler.Blocking on every blocking poll, reusing that snapshot so no extra DMV read
// is issued. It is a no-op until the engine gives it a per-manifest rule source (SetSource),
// which is swapped between operations; the killer is only constructed when the feature is
// armed in config, so no separate enabled flag is needed. All state is guarded by mu because
// Blocking (pump goroutine) and SetSource (engine goroutine, between operations) both touch it.
type BlockerKiller struct {
	kill   func(context.Context, int) error
	onKill func(KillEvent)
	now    func() time.Time

	mu      sync.Mutex
	src     KillSource
	current int       // SPID of the blocker in the current episode, 0 when unblocked
	since   time.Time // when the current blocker first blocked us
	killed  bool      // already issued KILL for the current episode
}

// NewBlockerKiller builds a killer. kill terminates a SPID (mssql.Conn.Kill on the monitoring
// pool); onKill is notified after a successful kill (may be nil); now defaults to time.Now.
func NewBlockerKiller(kill func(context.Context, int) error, onKill func(KillEvent), now func() time.Time) *BlockerKiller {
	if now == nil {
		now = time.Now
	}
	return &BlockerKiller{kill: kill, onKill: onKill, now: now}
}

// resetEpisode clears the current-blocker tracking (called when unblocked or re-sourced).
func (k *BlockerKiller) resetEpisode() {
	k.current, k.since, k.killed = 0, time.Time{}, false
}

// SetSource swaps the per-manifest kill-rule source and resets the episode state. Called by
// the engine before each manifest's operations (nil disarms the killer between manifests).
func (k *BlockerKiller) SetSource(src KillSource) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.src = src
	k.resetEpisode()
	k.mu.Unlock()
}

// consider inspects one blocking snapshot and kills the session blocking ddlSPID when it
// matches a rule and has blocked for at least that rule's delay. A no-op when no source is
// set or no rule matches. Each blocker is killed at most once per episode.
func (k *BlockerKiller) consider(ctx context.Context, sessions []mssql.Session, ddlSPID int) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.src == nil {
		return
	}
	blocker, ok := blockerSession(sessions, ddlSPID)
	if !ok {
		k.resetEpisode()
		return
	}
	now := k.now()
	if blocker.SPID != k.current {
		k.current, k.since, k.killed = blocker.SPID, now, false
	}
	if k.killed {
		return
	}
	for _, r := range k.src.Current() {
		if !r.match.matches(blocker) {
			continue
		}
		if now.Sub(k.since) < r.after {
			return // matched, but has not blocked long enough yet
		}
		if err := k.kill(ctx, blocker.SPID); err == nil {
			k.killed = true
			if k.onKill != nil {
				k.onKill(KillEvent{SPID: blocker.SPID, Login: blocker.Login, Waited: now.Sub(k.since)})
			}
		}
		return // first matching rule decides; don't fall through to a longer-delay rule
	}
}

// blockerSession returns the session directly blocking ddlSPID, found from the same
// snapshot, so matching can use its full identity (login/host/program/query).
func blockerSession(sessions []mssql.Session, ddlSPID int) (mssql.Session, bool) {
	if ddlSPID == 0 {
		return mssql.Session{}, false
	}
	var blockingSPID int
	for _, s := range sessions {
		if s.SPID == ddlSPID {
			blockingSPID = s.BlockingSPID
			break
		}
	}
	if blockingSPID == 0 {
		return mssql.Session{}, false
	}
	for _, s := range sessions {
		if s.SPID == blockingSPID {
			return s, true
		}
	}
	return mssql.Session{}, false
}
