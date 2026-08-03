package run

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

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
	Waited        time.Duration // how long it was kill-eligible before we killed it
	Job           mssql.AgentJob
}

// victimEpisode is the per-SPID state of one blocking episode.
type victimEpisode struct {
	since      time.Time // when this victim first became kill-eligible
	killed     bool      // a KILL was issued successfully
	killedAt   time.Time // when, for the grace window
	killFailed bool      // the KILL errored: stop suppressing, fall back to yielding
}

// VictimKiller terminates maintenance statements our operation blocks that have other
// sessions queued behind them, turning an online operation into a full-table outage.
// It is the mirror of BlockerKiller: that one kills sessions blocking us, this one
// kills sessions we block. The two are disjoint by construction — a session appearing
// as our own direct blocker is never a candidate here (see eligible) — so they share
// no episode state and the order they are consulted in does not matter.
//
// It is consulted from ServerSampler.Blocking on the snapshot that poll already reads.
// All state is guarded by mu: consider runs on the pump goroutine while Arm/Disarm run
// on the engine goroutine between manifests.
type VictimKiller struct {
	kill        func(context.Context, int) error
	resolve     func(context.Context, string, int) (mssql.AgentJob, error)
	onKill      func(AmplifierKillEvent) // presentation only (console + TUI); see notify
	clk         Clock
	selfProgram string // our own application name prefix; never kill a session running it

	mu       sync.Mutex
	policy   AmplifierPolicy
	armed    bool
	sink     ReactionSink
	episodes map[int]*victimEpisode
	jobs     map[string]mssql.AgentJob // attribution cache, keyed by "hex:step"
}

// NewVictimKiller builds a killer. kill terminates a SPID (mssql.Conn.Kill on the
// monitoring pool); resolve attributes a program_name to an Agent job (may be nil);
// onKill narrates a successful kill to the console and TUI (may be nil) — a separate
// route from the sink, because in TUI mode the engine's run output is io.Discard;
// clk defaults to System; selfProgram is our own application-name prefix
// (mssql.AppNamePrefix), matched by prefix because program_name carries the build
// version — which also excludes a different SqlGoPace version running concurrently.
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

// consider inspects one snapshot, updates episode state for every direct victim, and
// kills the ones that have been eligible for the policy's dwell. A no-op when
// disarmed. Victims absent from this snapshot have their episodes dropped, so the
// dwell restarts if they come back.
//
// It runs on the PUMP goroutine, from ServerSampler.Blocking. A kill (and the msdb
// attribution lookup behind it) is synchronous server I/O on that path, so on a loaded
// server it can delay the next blocking sample past its interval. Acceptable for an
// opt-in feature that fires rarely; do not add per-poll work here casually.
func (k *VictimKiller) consider(ctx context.Context, sessions []mssql.Session, ddlSPID int, ignore IgnoredSessions) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.armed {
		return
	}

	now := k.clk.Now()
	seen := make(map[int]bool)
	for _, v := range DirectVictims(sessions, ddlSPID) {
		if !k.eligible(v, sessions, ddlSPID, ignore) {
			continue
		}
		seen[v.SPID] = true
		ep, ok := k.episodes[v.SPID]
		if !ok {
			ep = &victimEpisode{since: now}
			k.episodes[v.SPID] = ep
		}
		if ep.killed || ep.killFailed || k.clk.Since(ep.since) < k.policy.After {
			continue
		}
		if err := k.kill(ctx, v.SPID); err != nil {
			ep.killFailed = true
			// Reached only on the failure transition: once killFailed is set, the
			// guard above short-circuits before this point on every later poll, so
			// this fires exactly once per episode rather than spamming the run log.
			// Amplifier stays nil — that field means a kill happened, and here it did
			// not — but a failed KILL must still be operator-visible, not a silent
			// fallback to the normal yield timer.
			if k.sink != nil {
				k.sink(ReactionEvent{
					Kind: "warn",
					Detail: fmt.Sprintf("failed to kill amplifying maintenance session SPID %d (%s): %v",
						v.SPID, v.Command, err),
				})
			}
			continue
		}
		ep.killed, ep.killedAt = true, now
		k.notify(ctx, v, sessions, ep.since, now.Sub(ep.since))
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

// notify resolves the victim's Agent job (cached) and delivers the kill on both
// routes: the sink (engine — run report and sidecar) and onKill (presentation —
// console and TUI). Called with mu held.
func (k *VictimKiller) notify(ctx context.Context, v mssql.Session, sessions []mssql.Session, firstEligible time.Time, waited time.Duration) {
	stmt := v.ActiveQuery
	if stmt == "" {
		stmt = v.ParentQuery
	}
	ev := AmplifierKillEvent{
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
		Job:           k.attribute(ctx, v.Program),
	}
	if k.sink != nil {
		k.sink(ReactionEvent{Kind: "kill", Detail: AmplifierDetail(ev), Amplifier: &ev})
	}
	if k.onKill != nil {
		k.onKill(ev)
	}
}

// statementExcerpt trims a statement to a single readable line for the narration.
const statementExcerpt = 120

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
			stmt = stmt[:statementExcerpt] + "…"
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

// attribute resolves an Agent job from a program_name, caching per (job, step).
// An unparseable program name, a nil resolver, or an msdb failure all yield an
// unresolved AgentJob — the caller falls back to the raw program/login/host.
func (k *VictimKiller) attribute(ctx context.Context, program string) mssql.AgentJob {
	hex, step, ok := mssql.ParseJobStepProgram(program)
	if !ok || k.resolve == nil {
		return mssql.AgentJob{}
	}
	key := hex + ":" + strconv.Itoa(step)
	if job, hit := k.jobs[key]; hit {
		return job
	}
	job, err := k.resolve(ctx, hex, step)
	if err != nil {
		return mssql.AgentJob{}
	}
	k.jobs[key] = job
	return job
}
