package run

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// Executor runs and controls a single DDL operation against the server. Pausing a
// resumable operation is done by aborting the running statement (canceling its
// context), not by a separate ALTER INDEX PAUSE, so the interface only needs to
// run, identify, and kill the execution session.
type Executor interface {
	SessionID
	ExecDDL(ctx context.Context, sql string) error
	Kill(ctx context.Context, spid int) error
}

// SessionID reports the session id of the execution connection. It is an interface
// rather than an int because that id is not fixed for the life of a run: a pinned
// connection a canceled statement poisoned is re-pinned onto a new server session,
// and anything holding the old id would attribute no blocking to us at all. Read it
// on every use.
type SessionID interface {
	SPID() int
}

var _ Executor = (*mssql.Conn)(nil)

// Sample is one monitoring snapshot taken while a DDL operation runs. Blocking and
// log state are polled on independent intervals, so a snapshot carries the latest
// known value of each.
type Sample struct {
	BlockingOthers bool   // our DDL is blocking a session not allowed to stay blocked
	Blocking       bool   // our DDL is blocking any session, ignored or not (max_block cap)
	LogOverCap     bool   // the transaction log is over its configured cap
	LogReuseWait   string // why the log cannot truncate (only set when over cap)
	// Blind names the monitoring channel that has stopped answering ("" while both are
	// healthy). The other fields are the last values that channel reported, which is
	// exactly why this one exists: they keep looking reassuring after the channel that
	// produced them went silent.
	Blind string
}

// LogSample is the transaction-log half of a monitoring snapshot.
type LogSample struct {
	OverCap   bool
	ReuseWait string // log_reuse_wait_desc, populated only when over cap
}

// BlockState summarizes how our DDL is blocking other sessions in one poll: Any is
// true when it blocks at least one session (ignored or not), driving the max_block
// safety cap; Unignored is true when it blocks a session not allowed to stay blocked,
// driving the normal yield reaction.
type BlockState struct {
	Any       bool
	Unignored bool
}

// Sampler reads the two monitored dimensions on independent cadences: blocking
// (frequent) and transaction-log pressure (less frequent). Blocking is told which
// blocked sessions to ignore, so a session the operator allows to stay blocked does
// not count toward the yield reaction (but still counts toward the max_block cap).
type Sampler interface {
	Blocking(ctx context.Context, ignore IgnoredSessions) (BlockState, error)
	Log(ctx context.Context) (LogSample, error)
}

// blockCap converts a max_block_minutes value into a duration; 0 (or negative) means
// no cap.
func blockCap(minutes int) time.Duration {
	if minutes <= 0 {
		return 0
	}
	return time.Duration(minutes) * time.Minute
}

// sessionRule is one compiled ignore_blocked_sessions entry. A zero sessionID or a
// nil regexp means that field is unset (a wildcard). A session matches the rule when
// every field it sets matches (AND).
type sessionRule struct {
	sessionID int
	app       *regexp.Regexp
	host      *regexp.Regexp
	login     *regexp.Regexp
	stmt      *regexp.Regexp
}

// IgnoredSessions is the compiled, run-time form of []ddl.IgnoredSession: a session
// blocked by our DDL is ignored (allowed to stay blocked) when it matches any rule.
type IgnoredSessions []sessionRule

// CompileIgnoredSessions compiles manifest rules into a matcher. The regexps were
// already validated at manifest load, so an error here is defensive.
func CompileIgnoredSessions(rules []ddl.IgnoredSession) (IgnoredSessions, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make(IgnoredSessions, 0, len(rules))
	for i, r := range rules {
		var rule sessionRule
		if r.SessionID != nil {
			rule.sessionID = *r.SessionID
		}
		for _, f := range []struct {
			expr string
			dst  **regexp.Regexp
		}{
			{r.AppName, &rule.app},
			{r.HostName, &rule.host},
			{r.LoginName, &rule.login},
			{r.Statement, &rule.stmt},
		} {
			if f.expr == "" {
				continue
			}
			re, err := regexp.Compile(f.expr)
			if err != nil {
				return nil, fmt.Errorf("ignore_blocked_sessions[%d]: %w", i, err)
			}
			*f.dst = re
		}
		out = append(out, rule)
	}
	return out, nil
}

// ignores reports whether the session matches any rule and so is allowed to stay
// blocked.
func (rs IgnoredSessions) ignores(s mssql.Session) bool {
	for _, r := range rs {
		if r.matches(s) {
			return true
		}
	}
	return false
}

// matches reports whether every field the rule sets matches the session: session_id
// exactly, and each regexp against its attribute (statement against the active query,
// falling back to the parent batch).
func (r sessionRule) matches(s mssql.Session) bool {
	switch {
	case r.sessionID != 0 && s.SPID != r.sessionID:
		return false
	case r.app != nil && !r.app.MatchString(s.Program):
		return false
	case r.host != nil && !r.host.MatchString(s.Host):
		return false
	case r.login != nil && !r.login.MatchString(s.Login):
		return false
	case r.stmt != nil && !r.stmt.MatchString(s.ActiveQuery) && !r.stmt.MatchString(s.ParentQuery):
		return false
	}
	return true
}

// key renders the rule as a canonical map key for the blocking-debt accumulator: the
// session id and every regexp source, in a fixed order. Each field is quoted rather than
// raw-joined because a regexp source can contain the separator itself ("x|y" as an
// alternation), which would make an unquoted join collide with a two-field rule and merge
// two rules' debts. Derived from the rule's text, so it is stable across the hot reload in
// manifestKillSource.Current: appending a rule mid-run leaves the other keys untouched.
func (r sessionRule) key() string {
	return fmt.Sprintf("%d|%s|%s|%s|%s", r.sessionID,
		strconv.Quote(reSource(r.app)), strconv.Quote(reSource(r.host)),
		strconv.Quote(reSource(r.login)), strconv.Quote(reSource(r.stmt)))
}

// String renders the rule for an operator-facing message, naming only the fields it sets.
// It is deliberately a different rendering from key(): the key must be total and stable,
// this must be readable.
func (r sessionRule) String() string {
	var parts []string
	if r.sessionID != 0 {
		parts = append(parts, fmt.Sprintf("session_id=%d", r.sessionID))
	}
	for _, f := range []struct {
		name string
		re   *regexp.Regexp
	}{
		{"app", r.app}, {"host", r.host}, {"login", r.login}, {"statement", r.stmt},
	} {
		if f.re != nil {
			parts = append(parts, fmt.Sprintf("%s=~%q", f.name, f.re.String()))
		}
	}
	if len(parts) == 0 {
		return "{}" // a rule that sets nothing matches everything
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// reSource returns a regexp's source, empty for an unset (nil) field.
func reSource(re *regexp.Regexp) string {
	if re == nil {
		return ""
	}
	return re.String()
}

// ErrCancelled signals the operation was canceled under pressure and may be
// retried by the caller.
var ErrCancelled = errors.New("operation canceled under pressure")

// ErrStopped signals a resumable operation was paused on operator request (graceful
// stop) and the run should stop without resuming; the next run continues it via RESUME.
var ErrStopped = errors.New("operation paused on graceful stop")

// ErrMonitorBlind ends an operation whose monitoring stopped answering. It is
// deliberately not ErrCancelled: a retry would be one more unwatched attempt against a
// server that has just demonstrated it cannot answer a DMV read.
var ErrMonitorBlind = errors.New("monitoring stopped answering; operation stopped rather than run unwatched")

// stopRequested reports whether a graceful stop is currently requested. stop is the
// DrainFlag's Draining method (cancellable), read at each operation, chunk, and poll
// boundary. A nil predicate (no drain wired) is never draining.
func stopRequested(stop func() bool) bool { return stop != nil && stop() }

// supervise monitors one running statement and returns the reaction to take,
// along with the pressure that triggered it. It returns (Continue, _, err) when
// the statement finishes on its own (err is nil on success) or the context is
// canceled, and (Pause|Cancel, pressure, nil) when sustained pressure warrants
// stopping the statement. samples streams snapshots; done delivers the statement
// result. Blocking pressure is debounced over blockingTimeout.
func supervise(
	ctx context.Context,
	clk Clock,
	caps Capabilities,
	blockingTimeout time.Duration,
	samples <-chan Sample,
	done <-chan error,
) (Action, Pressure, error) {
	var blockingStart, blockedSince time.Time

	for {
		select {
		case <-ctx.Done():
			return Continue, Pressure{}, ctx.Err()
		case err := <-done:
			return Continue, Pressure{}, err
		case s := <-samples:
			// A graceful stop pauses a resumable operation, checked on each monitoring poll
			// so a Cancel before the next poll withdraws it. A non-resumable operation runs
			// to completion (the drain stops the run at the next operation boundary).
			if caps.Resumable && stopRequested(caps.Stop) {
				return Stop, Pressure{}, nil
			}
			// Timer for the normal reaction: blocking a session not allowed to stay
			// blocked, debounced over blockingTimeout.
			if s.BlockingOthers {
				if blockingStart.IsZero() {
					blockingStart = clk.Now()
				}
			} else {
				blockingStart = time.Time{}
			}
			// Timer for the max_block safety cap: blocking ANY session, ignored or not.
			if s.Blocking {
				if blockedSince.IsZero() {
					blockedSince = clk.Now()
				}
			} else {
				blockedSince = time.Time{}
			}

			pressure := Pressure{
				// IgnoreBlocking holds the lock through blocking: the blocking
				// reaction is suppressed, but transaction-log pressure still applies.
				BlockingOthers: !caps.IgnoreBlocking && !blockingStart.IsZero() && clk.Since(blockingStart) >= blockingTimeout,
				LogOverCap:     s.LogOverCap,
				LogReuseWait:   s.LogReuseWait,
				// Not gated on IgnoreBlocking: that option waives a reaction to something
				// seen, never the ability to see.
				Blind: s.Blind,
			}
			// The safety cap overrides every ignore policy: after MaxBlock of continuous
			// blocking, yield even if the blocker is ignored.
			if caps.MaxBlock > 0 && !blockedSince.IsZero() && clk.Since(blockedSince) >= caps.MaxBlock {
				pressure.BlockingOthers = true
				pressure.Capped = true
			}
			if action := DecideReaction(pressure, caps); action != Continue {
				return action, pressure, nil
			}
		}
	}
}

// pollHealth reports one monitoring channel going blind, and coming back. A failed poll
// used to be dropped by an `if err == nil` with no else: no log line, no counter, nothing
// in the run report. That silence is the defect, because pumpSamples keeps the last
// values it read — so a channel that stops answering while nothing was blocking leaves a
// reaction hierarchy that never fires again, under a statement that keeps running.
//
// It speaks once per outage rather than once per poll: a warning repeated every
// blocking_poll_seconds is one an operator learns to scroll past, and the thing worth
// knowing is that the channel went blind and when it came back.
type pollHealth struct {
	channel string
	sink    ReactionSink
	failing bool
	misses  int
	// lastOK is when this channel last produced a reading. A poll that hangs never
	// updates it and never reaches observe, which is the whole point: staleness is the
	// only evidence a hang leaves.
	lastOK     time.Time
	blindAfter time.Duration
	dark       bool // already reported blind, so the warning is not repeated
}

// checkBlind reports whether this channel has gone silent for longer than it is allowed
// to, narrating each transition exactly once.
func (h *pollHealth) checkBlind(at time.Time) bool {
	silent := at.Sub(h.lastOK)
	blind := h.blindAfter > 0 && silent >= h.blindAfter
	switch {
	case blind && !h.dark:
		h.dark = true
		if h.sink != nil {
			h.sink(ReactionEvent{Kind: "warn", Detail: fmt.Sprintf(
				"%s has not answered for %s — the operation is running unwatched and will be stopped",
				h.channel, silent.Round(time.Second))})
		}
	case !blind && h.dark:
		h.dark = false
		if h.sink != nil {
			h.sink(ReactionEvent{Kind: "info", Detail: h.channel + " is answering again"})
		}
	}
	return blind
}

// observe records the outcome of one poll and reports the transitions.
func (h *pollHealth) observe(err error) {
	if err == nil {
		h.lastOK = time.Now()
	}
	switch {
	case err != nil:
		h.misses++
		if h.failing {
			return
		}
		h.failing = true
		if h.sink != nil {
			h.sink(ReactionEvent{Kind: "warn", Detail: fmt.Sprintf(
				"%s failed: %v — monitoring is running on the last state it read, so no reaction can fire on this channel until it recovers",
				h.channel, err)})
		}
	case h.failing:
		if h.sink != nil {
			h.sink(ReactionEvent{Kind: "info", Detail: fmt.Sprintf(
				"%s recovered after %d failed poll(s)", h.channel, h.misses)})
		}
		h.failing, h.misses = false, 0
	}
}

// BlindAfter is how long a monitoring channel may go without a successful read before
// the operation running under it is stopped. Two minutes, the same number as a shrink's
// default max_block_minutes and for the same reason: it is how long an operation may act
// on a production server without anybody watching.
//
// A channel polled less often than this is judged on its own cadence instead
// (blindThreshold), so a deliberately slow log poll is not mistaken for a dead one.
const BlindAfter = 2 * time.Minute

// pumpSpec is everything one run of the monitoring pump needs. It is a struct rather than
// a parameter list because seven call sites pass it, two of the fields are adjacent
// durations that would swap silently, and tests need to shorten blindAfter.
type pumpSpec struct {
	sampler    Sampler
	blockEvery time.Duration
	logEvery   time.Duration
	ignore     IgnoreSource
	sink       ReactionSink
	blindAfter time.Duration // 0 means BlindAfter
}

// blockOutcome and logOutcome carry one poll's result from its own goroutine to the pump.
type blockOutcome struct {
	st  BlockState
	err error
}

type logOutcome struct {
	ls  LogSample
	err error
}

// blindThreshold is how long a channel may stay silent before it counts as blind: the
// global bound, unless this channel's own cadence is slower than that. A log poll set to
// five minutes is not blind at two; it has simply not been asked yet.
func blindThreshold(after, every time.Duration) time.Duration {
	if floor := 2 * every; floor > after {
		return floor
	}
	return after
}

// pumpSamples polls blocking and transaction-log state on independent cadences and
// forwards a combined snapshot (the latest known value of each) whenever either poll
// fires. Both MonitoredRunner and ShrinkRunner drive their monitoring through it. The
// ignore matcher is re-read from the source on every blocking poll, so a rule added to
// the manifest mid-run is honored before the operation would abort.
//
// Each channel gets its own goroutine. Until 0.41.0 one goroutine served both, so a poll
// that hung stopped the other channel too — and hanging is the likely failure, not
// erroring: a DMV read waits on THREADPOOL when the worker pool is exhausted, which is
// what a long blocking chain does, which is what this pump exists to detect. There is no
// query timeout anywhere by design, so nothing returns and nothing errors. What the pump
// watches instead is staleness: a channel that has produced no successful read for its
// blindThreshold is reported in Sample.Blind, and the supervisor stops the operation
// rather than let it keep acting unobserved.
func pumpSamples(ctx context.Context, samples chan<- Sample, spec pumpSpec) {
	blindAfter := spec.blindAfter
	if blindAfter <= 0 {
		blindAfter = BlindAfter
	}
	now := time.Now()
	blocking := &pollHealth{channel: "blocking poll", sink: spec.sink, lastOK: now,
		blindAfter: blindThreshold(blindAfter, spec.blockEvery)}
	logging := &pollHealth{channel: "log poll", sink: spec.sink, lastOK: now,
		blindAfter: blindThreshold(blindAfter, spec.logEvery)}

	blockCh := make(chan blockOutcome)
	logCh := make(chan logOutcome)

	go func() {
		t := time.NewTicker(spec.blockEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			st, err := spec.sampler.Blocking(ctx, currentRules(spec.ignore))
			select {
			case blockCh <- blockOutcome{st: st, err: err}:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		// Take one log sample immediately, before the ticker loop, so a statement never
		// runs blind to log pressure for up to log_poll_seconds (H1,
		// docs/specs/REVIEW-2026-09-15-harm.md): without this, a retry issued right after
		// a log-pressure cancel starts with LogOverCap assumed false and only learns
		// otherwise on the first tick, writing into an already-over-cap log for the whole
		// interval. Only the log sample jumps the queue — the blocking path's reaction is
		// already debounced by blocking_timeout, so an extra immediate blocking poll buys
		// nothing and would also drive the blocker/victim killers a poll early.
		ls, err := spec.sampler.Log(ctx)
		select {
		case logCh <- logOutcome{ls: ls, err: err}:
		case <-ctx.Done():
			return
		}
		t := time.NewTicker(spec.logEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			ls, err := spec.sampler.Log(ctx)
			select {
			case logCh <- logOutcome{ls: ls, err: err}:
			case <-ctx.Done():
				return
			}
		}
	}()

	var cur Sample
	send := func() {
		select {
		case samples <- cur:
		case <-ctx.Done():
		}
	}
	// blindChannel names the first channel that has gone silent, narrating each
	// transition once. Both are checked on every call so a channel that recovers stops
	// being reported even while the other one is still dark.
	blindChannel := func() string {
		at := time.Now()
		name := ""
		for _, h := range []*pollHealth{blocking, logging} {
			if h.checkBlind(at) && name == "" {
				name = h.channel
			}
		}
		return name
	}

	// The watchdog is what turns a hang into an event: a hung poll sends nothing, so
	// without it the pump would simply go quiet. It runs on the faster of the two
	// cadences, which is always at most the blindness threshold.
	watchEvery := spec.blockEvery
	if spec.logEvery < watchEvery {
		watchEvery = spec.logEvery
	}
	if blindAfter < watchEvery {
		watchEvery = blindAfter
	}
	watchdog := time.NewTicker(watchEvery)
	defer watchdog.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case o := <-blockCh:
			blocking.observe(o.err)
			if o.err == nil {
				cur.Blocking = o.st.Any
				cur.BlockingOthers = o.st.Unignored
				cur.Blind = blindChannel()
				send()
			}
		case o := <-logCh:
			logging.observe(o.err)
			if o.err == nil {
				cur.LogOverCap = o.ls.OverCap
				cur.LogReuseWait = o.ls.ReuseWait
				cur.Blind = blindChannel()
				send()
			}
		case <-watchdog.C:
			if blind := blindChannel(); blind != cur.Blind {
				cur.Blind = blind
				send()
			}
		}
	}
}

// sampleProbe is the narrow set of server reads ServerSampler needs.
type sampleProbe interface {
	LogSpace(ctx context.Context) (mssql.LogSpace, error)
	LogReuseWait(ctx context.Context) (string, error)
	ActiveSessions(ctx context.Context) ([]mssql.Session, error)
}

// ServerSampler builds a Sample from live server state for the DDL session.
type ServerSampler struct {
	probe         sampleProbe
	sess          SessionID
	logMaxBytes   int64
	logMaxPercent int
	killer        *BlockerKiller // optional: kills matching blockers, reusing the Blocking snapshot
	victims       *VictimKiller  // optional: kills amplifying maintenance victims we block
}

// NewServerSampler returns a sampler for the given DDL session and log thresholds.
func NewServerSampler(probe sampleProbe, sess SessionID, logMaxBytes int64, logMaxPercent int) *ServerSampler {
	return &ServerSampler{probe: probe, sess: sess, logMaxBytes: logMaxBytes, logMaxPercent: logMaxPercent}
}

var _ Sampler = (*ServerSampler)(nil)

// SetKiller attaches a blocker-killer, consulted on every Blocking poll using the same
// session snapshot. A nil killer (the default) leaves killing off.
func (s *ServerSampler) SetKiller(k *BlockerKiller) { s.killer = k }

// SetVictimKiller attaches the amplifying-victim killer, consulted on every Blocking
// poll using the same session snapshot. A nil killer (the default) leaves the feature
// off and Blocking behaves exactly as it did before.
func (s *ServerSampler) SetVictimKiller(k *VictimKiller) { s.victims = k }

// Blocking reports how our DDL is blocking other sessions: Any when it blocks at
// least one session (ignored or not), Unignored when it blocks a session the operator
// has not allowed to stay blocked. The yield reaction keys off Unignored; the
// max_block safety cap keys off Any.
func (s *ServerSampler) Blocking(ctx context.Context, ignore IgnoredSessions) (BlockState, error) {
	sessions, err := s.probe.ActiveSessions(ctx)
	if err != nil {
		return BlockState{}, err
	}
	spid := s.sess.SPID()
	// Update victim episodes and kill anything eligible before reading suppression, so
	// a victim that becomes eligible on this very poll is suppressed on this poll too.
	s.victims.consider(ctx, sessions, spid, ignore)

	var st BlockState
	for _, sess := range sessions {
		if !sess.BlockedBy(spid) {
			continue
		}
		st.Any = true
		if ignore.ignores(sess) || s.victims.Suppressed(sess.SPID) {
			continue
		}
		st.Unignored = true
	}
	// Reuse the same snapshot to kill any session blocking our DDL that matches a kill
	// rule (the inverse direction: here we are the victim). No-op when no killer is set.
	s.killer.consider(ctx, sessions, spid)
	return st, nil
}

// Log reports whether the transaction log is over its cap and, when it is, why it
// cannot truncate (log_reuse_wait_desc). The reuse-wait query is only run when
// over cap, to keep the steady-state poll cheap.
func (s *ServerSampler) Log(ctx context.Context) (LogSample, error) {
	ls, err := s.probe.LogSpace(ctx)
	if err != nil {
		return LogSample{}, err
	}
	overCap := ls.UsedBytes() >= s.logMaxBytes || int(ls.UsedPercent) >= s.logMaxPercent
	if !overCap {
		return LogSample{}, nil
	}
	reuseWait, err := s.probe.LogReuseWait(ctx)
	if err != nil {
		return LogSample{}, err
	}
	return LogSample{OverCap: true, ReuseWait: reuseWait}, nil
}
