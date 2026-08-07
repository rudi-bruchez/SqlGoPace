package run

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// KillEvent describes a blocker the engine terminated, for console/log notification.
type KillEvent struct {
	SPID  int
	Login string
	// Waited is the matched rule's accumulated blocking debt, summed over every session that
	// matched it inside the window — NOT how long SPID itself blocked us, which for a
	// recidivist is a fraction of it. Anything rendering this must attribute it to the rule,
	// or it states something an operator cannot reconcile against sys.dm_exec_requests.
	Waited time.Duration
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

// BlockerKiller terminates sessions that block this run's DDL and match a kill rule, once the
// identity that rule names has blocked us for the rule's delay — time banked poll by poll and
// accumulated across sessions, so a killed blocker returning under a new SPID does not restart
// the clock (see recidivism.go). It is consulted from
// ServerSampler.Blocking on every blocking poll, reusing that snapshot so no extra DMV read
// is issued. It is a no-op until the engine gives it a per-manifest rule source (SetSource),
// which is swapped between operations; the killer is only constructed when the feature is
// armed in config, so no separate enabled flag is needed. All state is guarded by mu because
// Blocking (pump goroutine) and SetSource (engine goroutine, between operations) both touch it.
type BlockerKiller struct {
	kill   func(context.Context, int) error
	onKill func(KillEvent)
	now    func() time.Time

	mu       sync.Mutex
	src      KillSource
	sink     ReactionSink // per-operation run-report sink; nil between operations
	rec      *recidivism  // blocking debt per matched rule, so a returning blocker keeps its dwell
	current  int          // SPID of the blocker in the current episode, 0 when unblocked
	// lastPoll is the previous observation of the current episode, zero on its first poll.
	// consider advances it on every poll that sees the blocker, whether or not a rule
	// matched, so an interval where no rule matched is banked nowhere.
	lastPoll time.Time
	killed   bool // already issued KILL for the current episode
}

// NewBlockerKiller builds a killer. kill terminates a SPID (mssql.Conn.Kill on the monitoring
// pool); onKill is notified after a successful kill (may be nil); now defaults to time.Now.
func NewBlockerKiller(kill func(context.Context, int) error, onKill func(KillEvent), now func() time.Time) *BlockerKiller {
	if now == nil {
		now = time.Now
	}
	return &BlockerKiller{kill: kill, onKill: onKill, now: now, rec: newRecidivism(now)}
}

// resetEpisode clears the current-blocker tracking (called when unblocked or re-sourced).
// It deliberately does not touch the debt: debt is banked poll by poll while the blocker is
// observed, so there is never an unflushed remainder to fold in here.
func (k *BlockerKiller) resetEpisode() {
	k.current, k.lastPoll, k.killed = 0, time.Time{}, false
}

// sincePoll returns the time to bank for this poll: zero on an episode's first observation
// (we do not know how long the blocker was there before we saw it, exactly as the previous
// per-episode timer assumed), the inter-poll gap afterwards. One imprecision is accepted: a
// poll where the blocker switches from matching rule A to matching rule B directly (no
// unmatched poll between them) banks that whole gap into B, because a single episode-wide
// lastPoll cannot tell which rule owned it. A poll interval of slack does not justify a
// second per-key map.
func (k *BlockerKiller) sincePoll(now time.Time) time.Duration {
	if k.lastPoll.IsZero() {
		return 0
	}
	return now.Sub(k.lastPoll)
}

// SetSource swaps the per-manifest kill-rule source, resets the episode state and clears the
// sink. Called by the engine before each manifest's operations (nil disarms the killer between
// manifests). Because it clears the sink, SetSink must be called after it, not before.
func (k *BlockerKiller) SetSource(src KillSource) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.src = src
	k.sink = nil
	k.rec.reset()
	k.resetEpisode()
	k.mu.Unlock()
}

// SetSink installs the reaction sink the cap escalation is emitted on. The engine sets it
// per operation and SetSource clears it between manifests, for the same reason
// VictimKiller.SetSink exists: a late event must never be attributed to the next
// operation's report. Kills themselves keep reporting through onKill to the console.
func (k *BlockerKiller) SetSink(sink ReactionSink) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.sink = sink
	k.mu.Unlock()
}

// consider inspects one blocking snapshot and kills the session blocking ddlSPID when it
// matches a rule and the identity that rule names has blocked us for at least that rule's
// delay — accumulated across sessions, so a killed blocker returning under a new SPID does
// not restart the clock (see recidivism.go). Kills nothing when no source is set or no rule
// matches, but a poll that sees the blocker still advances the poll mark (see lastPoll).
// Each blocker is killed at most once per episode, and each identity at most maxRepeatKills
// times per recidivismWindow.
//
// The decision runs under mu in considerLocked; the cap escalation is emitted HERE, with the
// lock released. The sink is the engine's per-operation narration path — it formats, writes
// to the run log and can notify a webhook — and holding mu across it would make SetSource,
// which runs on the engine goroutine at every manifest boundary, wait on that work.
func (k *BlockerKiller) consider(ctx context.Context, sessions []mssql.Session, ddlSPID int) {
	if k == nil {
		return
	}
	if sink, detail := k.considerLocked(ctx, sessions, ddlSPID); sink != nil && detail != "" {
		sink(ReactionEvent{Kind: "warn", Detail: detail})
	}
}

// considerLocked makes the whole decision under mu and returns the sink to narrate on plus
// the cap-escalation text, empty when this poll has nothing to say. The KILL itself stays
// here, where it has always been: it is one attention on the monitoring pool, and moving it
// out would mean reserving the kill before issuing it — a different design than the one the
// budget is built on, where only a KILL that landed spends from it.
func (k *BlockerKiller) considerLocked(ctx context.Context, sessions []mssql.Session, ddlSPID int) (ReactionSink, string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.src == nil {
		return nil, ""
	}
	k.rec.prune() // before any debt is read, so a quiet window really restores the full dwell
	blocker, ok := blockerSession(sessions, ddlSPID)
	if !ok {
		k.resetEpisode()
		return nil, ""
	}
	now := k.now()
	if blocker.SPID != k.current {
		k.current, k.lastPoll, k.killed = blocker.SPID, time.Time{}, false
	}
	for _, r := range k.src.Current() {
		if !r.match.matches(blocker) {
			continue
		}
		k.rec.accrue(r.key, k.sincePoll(now))
		k.lastPoll = now
		if k.killed {
			// Already killed in this episode. The banking above still runs: a killed session
			// that lingers in rollback is genuinely still blocking us, and counting that time
			// keeps the identity's bucket alive and its debt honest. It cannot cause a second
			// kill — this return is what prevents that — and the debt is already past `after`
			// anyway, so it changes no decision.
			return nil, ""
		}
		if k.rec.debt(r.key) < r.after {
			return nil, "" // matched, but this identity has not blocked us long enough yet
		}
		if k.rec.kills(r.key) >= maxRepeatKills {
			// Capped: go quiet and let the normal reaction hierarchy take over — which is
			// exactly the behavior without this feature, so the run can never block longer
			// for having tried. The warn fires once, on the transition.
			if k.rec.escalate(r.key) {
				return k.sink, capDetail(r, blocker)
			}
			return nil, ""
		}
		if err := k.kill(ctx, blocker.SPID); err == nil {
			k.killed = true
			k.rec.recordKill(r.key)
			if k.onKill != nil {
				// Waited is the rule's debt, not this episode's elapsed time: for a
				// recidivist the episode is seconds old, and reporting that would misstate
				// why the kill was justified. It is the caller's job to say whose time it
				// is — see KillEvent.Waited.
				k.onKill(KillEvent{SPID: blocker.SPID, Login: blocker.Login, Waited: k.rec.debt(r.key)})
			}
		}
		return nil, "" // first matching rule decides; don't fall through to a longer-delay rule
	}
	// No rule matched this poll: advance the mark anyway, so the interval we just observed
	// is not banked into whatever rule matches next.
	k.lastPoll = now
	return nil, ""
}

// capDetail narrates a rule that hit the repeat-kill cap: what was killed, how often, and
// what the operator can do about it. It names the rule rather than only the last session,
// because with a population of offenders the individual SPIDs are noise.
func capDetail(r killRule, blocker mssql.Session) string {
	return fmt.Sprintf("stopped killing blockers matching rule %s: %d killed within %s and they keep returning "+
		"(last: SPID %d, login=%s host=%s program=%s) — the blocker is being restarted faster than it can be "+
		"cleared, so the run falls back to the normal blocking reaction; consider disabling the job behind it or "+
		"scheduling this run outside its window",
		r.match, maxRepeatKills, recidivismWindow, blocker.SPID, blocker.Login, blocker.Host, blocker.Program)
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
