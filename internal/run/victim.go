package run

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// killGraceWindow is how long a killed victim stays suppressed after a successful
// KILL, so a rollback still visible in the snapshot does not instantly trip the yield
// timer. Time-based rather than a poll count: the blocking poll interval is
// configurable, so "two polls" would mean anything from two seconds to a minute. Not
// configurable — a killed lock request clears from the wait queue almost immediately,
// and this window exists only to absorb one stale snapshot.
const killGraceWindow = 15 * time.Second

// AmplifierPolicy is the armed kill policy for one manifest.
type AmplifierPolicy struct {
	MinBlockedBehind int           // sessions that must be queued behind the victim
	After            time.Duration // how long eligibility must persist before the kill
	Commands         []string      // replaces the built-in allow-list when non-empty
}

// AmplifierKillEvent describes a maintenance victim the run terminated because it was
// amplifying our block. Login/Host/Program/Statement are always populated, whether or
// not Job resolved: when attribution fails they are all the operator has.
type AmplifierKillEvent struct {
	SPID          int
	Command       string
	Statement     string
	Database      string
	Login         string
	Host          string
	Program       string
	BlockedBehind int
	WaitedMS      int64         // the victim's own wait_time, from the DMV
	FirstEligible time.Time     // when it first met every eligibility condition
	Waited        time.Duration // how long this identity has been kill-eligible, accumulated across sessions
	Job           mssql.AgentJob
}

// victimEpisode is the per-SPID state of one blocking episode.
type victimEpisode struct {
	since      time.Time // when this victim first became kill-eligible
	lastPoll   time.Time // previous observation of this episode; zero on its first poll
	killed     bool      // a KILL was issued successfully
	killedAt   time.Time // when, for the grace window
	killFailed bool      // the KILL errored: stop suppressing, fall back to yielding
}

// sincePoll returns the time to bank for this poll: zero on the episode's first
// observation, the inter-poll gap afterwards.
func (e *victimEpisode) sincePoll(now time.Time) time.Duration {
	if e.lastPoll.IsZero() {
		return 0
	}
	return now.Sub(e.lastPoll)
}

// victimKey identifies the offender behind a victim session: the Agent job step when the
// program name resolves to one, else the connection triplet. It deliberately excludes the
// command verb and the statement text — a job that runs a different statement after a
// restart, or the same statement against another object, is the same offender.
func victimKey(v mssql.Session) string {
	if hex, step, ok := mssql.ParseJobStepProgram(v.Program); ok {
		return fmt.Sprintf("job:%s:%d", hex, step)
	}
	return fmt.Sprintf("sess:%s|%s|%s", v.Login, v.Host, v.Program)
}

// VictimKiller terminates maintenance statements our operation blocks that have other
// sessions queued behind them, turning an online operation into a full-table outage.
// It is the mirror of BlockerKiller: that one kills sessions blocking us, this one
// kills sessions we block. The two are disjoint by construction — a session appearing
// as our own direct blocker is never a candidate here (see eligible) — so they share
// no episode state and the order they are consulted in does not matter.
//
// It is consulted from ServerSampler.Blocking on the snapshot that poll already reads.
// The mutable state is guarded by mu: consider runs on the pump goroutine while
// Arm/Disarm/SetSink run on the engine goroutine between manifests. Nothing that talks
// to the server is done while holding it — see consider.
type VictimKiller struct {
	kill        func(context.Context, int) error
	resolve     func(context.Context, string, int) (mssql.AgentJob, error)
	onKill      func(AmplifierKillEvent) // presentation only (console + TUI); called unlocked, see consider
	clk         Clock
	selfProgram string // our own application name prefix; never kill a session running it

	mu       sync.Mutex
	policy   AmplifierPolicy
	armed    bool
	sink     ReactionSink
	rec      *recidivism // blocking debt per offender identity, so a job that restarts under a new SPID keeps its dwell
	episodes map[int]*victimEpisode
	jobs     map[string]mssql.AgentJob // attribution cache, keyed by "hex:step"
}

// NewVictimKiller builds a killer. kill terminates a SPID (mssql.Conn.Kill on the
// monitoring pool); resolve attributes a program_name to an Agent job (may be nil);
// onKill narrates a successful kill to the console and TUI (may be nil) — a separate
// route from the sink, because in TUI mode the engine's run output is io.Discard;
// clk defaults to System; selfProgram is our own application-name prefix, which the
// caller reads off the live connection ((*mssql.Conn).AppNamePrefix) because a DSN can
// override it — it falls back to mssql.AppNamePrefix only when empty. It is matched by
// prefix because program_name carries the build version, which also excludes a
// different SqlGoPace version running concurrently.
func NewVictimKiller(
	kill func(context.Context, int) error,
	resolve func(context.Context, string, int) (mssql.AgentJob, error),
	onKill func(AmplifierKillEvent),
	clk Clock,
	selfProgram string,
) *VictimKiller {
	if clk == nil {
		clk = System
	}
	if selfProgram == "" {
		selfProgram = mssql.AppNamePrefix
	}
	return &VictimKiller{
		kill: kill, resolve: resolve, onKill: onKill, clk: clk, selfProgram: selfProgram,
		rec:      newRecidivism(clk.Now),
		episodes: make(map[int]*victimEpisode),
		// jobs deliberately outlives Arm/Disarm: Agent job ids are globally unique, so
		// an attribution cached under one manifest is still correct under the next, and
		// re-reading msdb per manifest would buy nothing.
		jobs: make(map[string]mssql.AgentJob),
	}
}

// SetSink installs the reaction sink kills are emitted on. The engine sets it per
// operation, where it builds the sink, and clears it (nil) between operations so a
// late kill cannot be attributed to the next operation's report.
func (k *VictimKiller) SetSink(sink ReactionSink) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.sink = sink
	k.mu.Unlock()
}

// Arm enables the killer with p for the current manifest and clears episode state.
func (k *VictimKiller) Arm(p AmplifierPolicy) {
	if k == nil {
		return
	}
	// A fan-out of zero is not an amplifier — the whole premise of this feature — so
	// MinBlockedBehind < 1 can only mean "unset", never "kill with nothing queued
	// behind it". Floor it here rather than depend on every caller (config included)
	// supplying a positive value.
	if p.MinBlockedBehind < 1 {
		p.MinBlockedBehind = 1
	}
	k.mu.Lock()
	k.policy, k.armed = p, true
	k.episodes = make(map[int]*victimEpisode)
	k.rec.reset()
	k.mu.Unlock()
}

// Disarm disables the killer between manifests and clears episode state and the sink.
func (k *VictimKiller) Disarm() {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.armed, k.sink = false, nil
	k.episodes = make(map[int]*victimEpisode)
	k.rec.reset()
	k.mu.Unlock()
}

// Suppressed reports whether spid must not count toward BlockState.Unignored right
// now: a kill is pending, or it was killed within the grace window. A failed KILL
// withdraws suppression immediately, so the feature can never make us block longer
// than it would without it.
func (k *VictimKiller) Suppressed(spid int) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.armed {
		return false
	}
	ep, ok := k.episodes[spid]
	switch {
	case !ok, ep.killFailed:
		return false
	case ep.killed:
		return k.clk.Since(ep.killedAt) < killGraceWindow
	default:
		return true
	}
}

// killTarget is one victim the locked scan selected and already marked killed. The
// KILL itself is issued by consider with the lock released; ep is carried so a failure
// can withdraw that optimistic mark. kill holds the not-yet-attributed event for a
// successful KILL: its Job field is unresolved (attribute makes an msdb round trip) and
// the narration is not yet rendered (AmplifierDetail needs the resolved Job).
type killTarget struct {
	ep      *victimEpisode
	key     string // the offender identity this kill is charged to
	spid    int
	command string
	kill    *AmplifierKillEvent
}

// consider inspects one snapshot, updates episode state for every direct victim, and
// kills the ones that have been eligible for the policy's dwell. A no-op when
// disarmed. Victims that are no longer eligible — gone from the snapshot, or still
// blocked but no longer matching every condition — have their episodes dropped, so the
// dwell restarts if they qualify again.
//
// It runs on the PUMP goroutine, from ServerSampler.Blocking, and is the only caller of
// considerLocked, so the mutex is not there to serialize two scans: it guards
// episodes/policy/armed/sink against Arm, Disarm and SetSink on the engine goroutine.
// Everything that can block on the server — the KILL, the msdb attribution lookup — and
// the sink/onKill callbacks therefore run HERE, after considerLocked returns and the
// lock is released. Were any of them held under mu, Disarm (called at manifest end)
// would block behind it for as long as the server takes, and a KILL against a dying
// connection can take a while.
//
// At-most-one-kill-per-episode survives that split because the locked scan marks each
// selected episode killed optimistically; a failed KILL withdraws the mark here and
// sets killFailed, which the scan treats as terminal for the episode either way.
//
// ctx is the pump's, which dies with the DDL statement: a kill is either skipped (the
// statement already finished) or detached from it — see the two blocks below.
func (k *VictimKiller) consider(ctx context.Context, sessions []mssql.Session, ddlSPID int, ignore IgnoredSessions) {
	if k == nil {
		return
	}
	targets, sink, onKill := k.considerLocked(sessions, ddlSPID, ignore)
	if len(targets) == 0 {
		return
	}
	// The pump's context dies the moment the DDL statement returns (runStatement cancels
	// sampleCtx on return), which can be the very poll a victim reached its dwell on.
	// Starting a KILL on a context that is already done would fail instantly with
	// "context canceled" and report a kill failure that never happened, so skip it and
	// leave the episode unkilled — the victim is simply retried on a later run.
	if ctx.Err() != nil {
		for _, t := range targets {
			k.abandonKill(t.ep)
		}
		return
	}
	// The KILL and the attribution behind it must outlive that cancellation: they are
	// control work about the statement that just ended, not part of it. Detaching them
	// mirrors runStatement's own fallback KILL on context.Background().
	detached := context.WithoutCancel(ctx)
	for _, t := range targets {
		if err := k.killDetached(detached, t.spid); err != nil {
			k.withdrawKill(t.ep)
			// Reached only on the failure transition: killFailed is terminal for the
			// episode, so the scan short-circuits before selecting this SPID again and
			// this fires exactly once per episode rather than spamming the run log.
			// Amplifier stays nil — that field means a kill happened, and here it did
			// not — but a failed KILL must still be operator-visible, not a silent
			// fallback to the normal yield timer.
			if sink != nil {
				sink(ReactionEvent{
					Kind: "warn",
					Detail: fmt.Sprintf("failed to kill amplifying maintenance session SPID %d (%s): %v",
						t.spid, t.command, err),
				})
			}
			continue
		}
		job, err := k.attribute(detached, t.kill.Program)
		t.kill.Job = job
		if sink != nil {
			if err != nil {
				sink(ReactionEvent{Kind: "warn", Detail: fmt.Sprintf(
					"SQL Agent job attribution unavailable for SPID %d (program %q): %v",
					t.spid, t.kill.Program, err)})
			}
			sink(ReactionEvent{
				Kind:      "kill",
				Detail:    AmplifierDetail(*t.kill),
				Amplifier: t.kill,
			})
		}
		if onKill != nil {
			onKill(*t.kill)
		}
	}
}

// considerLocked runs the eligibility scan under k.mu and returns the victims due to be
// killed — already marked killed, so a concurrent Suppressed keeps suppressing them
// while the KILL is in flight — plus a snapshot of k.sink and k.onKill (both mutable via
// SetSink, so they must be read under the same lock). It issues no KILL, calls no
// callback and reads no msdb: see consider for why that matters.
func (k *VictimKiller) considerLocked(sessions []mssql.Session, ddlSPID int, ignore IgnoredSessions) (targets []killTarget, sink ReactionSink, onKill func(AmplifierKillEvent)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	sink, onKill = k.sink, k.onKill
	if !k.armed {
		return nil, sink, onKill
	}

	now := k.clk.Now()
	k.rec.prune() // before any debt is read: a quiet window restores the full dwell
	seen := make(map[int]bool)
	banked := make(map[string]bool) // identities already charged for this poll
	for _, v := range DirectVictims(sessions, ddlSPID) {
		if !k.eligible(v, sessions, ddlSPID, ignore) {
			continue
		}
		key := victimKey(v)
		seen[v.SPID] = true
		ep, ok := k.episodes[v.SPID]
		if !ok {
			ep = &victimEpisode{since: now}
			k.episodes[v.SPID] = ep
		}
		if !banked[key] {
			// Once per identity per poll, not once per victim: one offender running two
			// concurrent sessions would otherwise bank the inter-poll gap twice into the
			// same bucket, reaching the dwell in After/N wall-clock and over-reporting
			// Waited N-fold. The lastPoll mark below still advances for every victim —
			// each episode owns its own — so nothing is banked twice and nothing is lost.
			//
			// One bounded imprecision is accepted: an episode kept alive by the kill grace
			// window whose SPID SQL Server has since recycled onto a different, eligible
			// amplifier banks now-lastPoll into the NEW session's identity. The mis-banked
			// interval is at most killGraceWindow, and the stale ep.killed keeps that
			// victim out of the kill path anyway.
			k.rec.accrue(key, ep.sincePoll(now))
			banked[key] = true
		}
		ep.lastPoll = now
		if ep.killed || ep.killFailed || k.rec.debt(key) < k.policy.After {
			continue
		}
		ep.killed, ep.killedAt = true, now
		targets = append(targets, killTarget{
			ep: ep, key: key, spid: v.SPID, command: v.Command,
			kill: killEvent(v, sessions, ep.since, k.rec.debt(key)),
		})
	}
	// Drop episodes for victims no longer eligible, except those inside their grace
	// window — a killed victim leaves the snapshot, and dropping it early would end
	// the grace suppression the moment it worked.
	for spid, ep := range k.episodes {
		if seen[spid] || (ep.killed && k.clk.Since(ep.killedAt) < killGraceWindow) {
			continue
		}
		delete(k.episodes, spid)
	}
	return targets, sink, onKill
}

// killTimeout bounds one KILL against an amplifying victim.
//
// Like attributeTimeout, this is not the "no query timeout" rule of CLAUDE.md: that rule
// governs the executing DDL, whose duration must be decided by the monitoring loop and the
// reaction hierarchy. A KILL is a control command issued about another session, and it runs
// on the pump goroutine inside ServerSampler.Blocking — an unbounded one that hangs (a
// dying connection can take a while to accept an attention) would stop the pump emitting
// samples, and neither the blocking timeout nor max_block_minutes would advance while our
// DDL kept running and kept blocking. 30s is generous for an attention that normally
// returns at once, and still short enough to keep the loop reactive.
const killTimeout = 30 * time.Second

// killDetached issues one KILL on the detached context, bounded by killTimeout so a KILL
// that does not come back cannot stall the pump.
func (k *VictimKiller) killDetached(detached context.Context, spid int) error {
	ctx, cancel := context.WithTimeout(detached, killTimeout)
	defer cancel()
	return k.kill(ctx, spid)
}

// abandonKill undoes the optimistic mark considerLocked set for a kill that was never
// attempted (the run was shutting down). Unlike withdrawKill it does NOT set killFailed:
// nothing failed, so the episode stays open and the victim is reconsidered on a later poll
// or a later run instead of being permanently written off. No warn is emitted either.
func (k *VictimKiller) abandonKill(ep *victimEpisode) {
	k.mu.Lock()
	ep.killed = false
	k.mu.Unlock()
}

// withdrawKill undoes the optimistic mark considerLocked set when the KILL turns out to
// have failed: suppression stops immediately (so we yield on the normal timer, never
// blocking longer than we would without the feature) and killFailed makes the failure
// terminal for the episode, keeping the warn to one per episode. ep is mutated through
// the pointer the scan handed over: if Arm or Disarm replaced the episode map in the
// meantime the episode is already orphaned, and writing to it is correctly a no-op.
func (k *VictimKiller) withdrawKill(ep *victimEpisode) {
	k.mu.Lock()
	ep.killed, ep.killFailed = false, true
	k.mu.Unlock()
}

// eligible applies the six conditions of the design's §1.3, in cheapest-first order.
func (k *VictimKiller) eligible(v mssql.Session, sessions []mssql.Session, ddlSPID int, ignore IgnoredSessions) bool {
	switch {
	case k.selfProgram != "" && strings.HasPrefix(v.Program, k.selfProgram):
		return false // never kill another SqlGoPace session, whatever its version
	case !mssql.IsAmplifyingCommand(v.Command, k.policy.Commands):
		return false
	case ignore.ignores(v):
		return false // an explicitly ignored victim is never killed
	case k.blocksUs(v.SPID, sessions, ddlSPID):
		return false // our own direct blocker belongs to BlockerKiller
	case BlockedBehind(sessions, v.SPID) < k.policy.MinBlockedBehind:
		return false
	}
	return true
}

// blocksUs reports whether spid is the session our own DDL is waiting on, which makes
// it BlockerKiller's to handle and never this killer's.
func (k *VictimKiller) blocksUs(spid int, sessions []mssql.Session, ddlSPID int) bool {
	return mssql.FindSelfBlock(sessions, ddlSPID).SPID == spid
}

// killEvent builds the not-yet-attributed event for a kill about to be issued: everything
// except Job, which attribute fills once the lock from considerLocked is released. waited is
// the offender identity's accumulated debt, not this episode's elapsed time — for a job that
// restarts under a new SPID the episode is seconds old, and the debt is what justified the
// kill. firstEligible still dates this episode. It touches no VictimKiller state, so it needs
// no lock itself; it is called from inside considerLocked only because that is where the
// episode's values are at hand.
func killEvent(v mssql.Session, sessions []mssql.Session, firstEligible time.Time, waited time.Duration) *AmplifierKillEvent {
	stmt := v.ActiveQuery
	if stmt == "" {
		stmt = v.ParentQuery
	}
	return &AmplifierKillEvent{
		SPID:          v.SPID,
		Command:       v.Command,
		Statement:     stmt,
		Database:      v.Database,
		Login:         v.Login,
		Host:          v.Host,
		Program:       v.Program,
		BlockedBehind: BlockedBehind(sessions, v.SPID),
		WaitedMS:      v.WaitMS,
		FirstEligible: firstEligible,
		Waited:        waited,
	}
}

// statementExcerpt trims a statement to a single readable line for the narration.
const statementExcerpt = 120

// truncateBytes cuts s to at most n bytes without splitting a multi-byte rune. SQL
// Server allows Unicode identifiers, so a statement excerpt can end mid-rune; the
// resulting invalid UTF-8 lands in the run report and in the TUI, where lipgloss then
// mis-measures the line's width. Backing up to the nearest rune start costs at most
// three bytes of excerpt.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// AmplifierDetail narrates one kill for the console, the run report and the TUI feed.
// It names the statement, not just the command verb, because "UPDATE STATISTICS" alone
// does not tell the operator which object was involved — and the object is what they
// act on. The source clause names the Agent job when attribution resolved and falls
// back to the raw program/login/host when it did not; that fallback is the whole point
// of recording those fields unconditionally.
//
// Exported so the CLI's presentation callback renders exactly the same line as the
// run report, rather than a second, drifting copy.
func AmplifierDetail(ev AmplifierKillEvent) string {
	subject := ev.Command
	if stmt := strings.Join(strings.Fields(ev.Statement), " "); stmt != "" {
		if len(stmt) > statementExcerpt {
			stmt = truncateBytes(stmt, statementExcerpt) + "…"
		}
		subject = fmt.Sprintf("%s: %s", ev.Command, stmt)
	}
	d := fmt.Sprintf("killed amplifying maintenance session SPID %d (%s) — %d session(s) queued behind it",
		ev.SPID, subject, ev.BlockedBehind)
	switch {
	case ev.Job.Resolved:
		d += fmt.Sprintf("; source: SQL Agent job %q step %d", ev.Job.JobName, ev.Job.StepID)
		if stmt := ev.Job.DisableStatement(); stmt != "" {
			d += fmt.Sprintf(" — consider disabling it during maintenance: %s", stmt)
		}
	case ev.Program != "":
		d += fmt.Sprintf("; source: %s (login=%s host=%s)", ev.Program, ev.Login, ev.Host)
	}
	return d
}

// attributeTimeout bounds the msdb lookup behind a kill's job attribution.
//
// This is not the "no query timeout" rule of CLAUDE.md: that rule governs the executing
// DDL, whose duration must be decided by the monitoring loop and the reaction hierarchy
// rather than by a clock. Attribution is presentation only — it names the Agent job
// behind a kill that has already happened — and it runs on the pump goroutine, inside
// ServerSampler.Blocking. msdb is written to constantly by Agent itself, so a contended
// lookup that hangs would stop the pump emitting samples; supervise would then block on
// <-samples and neither the blocking timeout nor max_block_minutes would advance while
// the DDL kept running and kept blocking. A bounded miss costs a job name; an unbounded
// one costs the run its ability to react at all.
const attributeTimeout = 5 * time.Second

// attribute resolves an Agent job from a program_name, caching per (job, step). An
// unparseable program name or a nil resolver yields an unresolved AgentJob and no
// error; an msdb failure or a lookup that outruns attributeTimeout yields an unresolved
// AgentJob and the error, so the caller can say why the kill is reported with the raw
// program/login/host instead of a job name. The kill itself is never affected.
//
// The cache is read and written under k.mu, but resolve itself runs with the lock
// released: holding it across a server round trip would block Disarm (and any other
// lock waiter) for the duration of the query. A cache miss racing with another lookup
// for the same job just resolves it twice; both calls agree on the answer, so the
// second store simply overwrites the first with an equal value.
func (k *VictimKiller) attribute(ctx context.Context, program string) (mssql.AgentJob, error) {
	hex, step, ok := mssql.ParseJobStepProgram(program)
	if !ok || k.resolve == nil {
		return mssql.AgentJob{}, nil
	}
	key := hex + ":" + strconv.Itoa(step)
	k.mu.Lock()
	job, hit := k.jobs[key]
	k.mu.Unlock()
	if hit {
		return job, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, attributeTimeout)
	defer cancel()
	job, err := k.resolve(lookupCtx, hex, step)
	if err != nil {
		return mssql.AgentJob{}, err
	}
	k.mu.Lock()
	k.jobs[key] = job
	k.mu.Unlock()
	return job, nil
}
