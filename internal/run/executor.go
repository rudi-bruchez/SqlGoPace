package run

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// Executor runs and controls a single DDL operation against the server. Pausing a
// resumable operation is done by aborting the running statement (canceling its
// context), not by a separate ALTER INDEX PAUSE, so the interface only needs to
// run, identify, and kill the execution session.
type Executor interface {
	SPID() int
	ExecDDL(ctx context.Context, sql string) error
	Kill(ctx context.Context, spid int) error
}

var _ Executor = (*mssql.Conn)(nil)

// Sample is one monitoring snapshot taken while a DDL operation runs. Blocking and
// log state are polled on independent intervals, so a snapshot carries the latest
// known value of each.
type Sample struct {
	BlockingOthers bool   // our DDL is blocking other sessions
	LogOverCap     bool   // the transaction log is over its configured cap
	LogReuseWait   string // why the log cannot truncate (only set when over cap)
}

// LogSample is the transaction-log half of a monitoring snapshot.
type LogSample struct {
	OverCap   bool
	ReuseWait string // log_reuse_wait_desc, populated only when over cap
}

// Sampler reads the two monitored dimensions on independent cadences: blocking
// (frequent) and transaction-log pressure (less frequent). Blocking is told which
// blocked sessions to ignore, so a session the operator allows to stay blocked does
// not count as pressure.
type Sampler interface {
	Blocking(ctx context.Context, ignore IgnoredSessions) (bool, error)
	Log(ctx context.Context) (LogSample, error)
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

// ErrCancelled signals the operation was canceled under pressure and may be
// retried by the caller.
var ErrCancelled = errors.New("operation canceled under pressure")

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
	var blockingStart time.Time

	for {
		select {
		case <-ctx.Done():
			return Continue, Pressure{}, ctx.Err()
		case err := <-done:
			return Continue, Pressure{}, err
		case s := <-samples:
			if s.BlockingOthers {
				if blockingStart.IsZero() {
					blockingStart = clk.Now()
				}
			} else {
				blockingStart = time.Time{}
			}

			pressure := Pressure{
				// IgnoreBlocking holds the lock through blocking: the blocking
				// reaction is suppressed, but transaction-log pressure still applies.
				BlockingOthers: !caps.IgnoreBlocking && !blockingStart.IsZero() && clk.Since(blockingStart) >= blockingTimeout,
				LogOverCap:     s.LogOverCap,
				LogReuseWait:   s.LogReuseWait,
			}
			if action := DecideReaction(pressure, caps); action != Continue {
				return action, pressure, nil
			}
		}
	}
}

// pumpSamples polls blocking and transaction-log state on independent cadences and
// forwards a combined snapshot (the latest known value of each) whenever either poll
// fires. Both MonitoredRunner and ShrinkRunner drive their monitoring through it.
func pumpSamples(ctx context.Context, samples chan<- Sample, sampler Sampler, blockEvery, logEvery time.Duration, ignore IgnoredSessions) {
	blockTicker := time.NewTicker(blockEvery)
	defer blockTicker.Stop()
	logTicker := time.NewTicker(logEvery)
	defer logTicker.Stop()

	var cur Sample
	send := func() {
		select {
		case samples <- cur:
		case <-ctx.Done():
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-blockTicker.C:
			if b, err := sampler.Blocking(ctx, ignore); err == nil {
				cur.BlockingOthers = b
				send()
			}
		case <-logTicker.C:
			if l, err := sampler.Log(ctx); err == nil {
				cur.LogOverCap = l.OverCap
				cur.LogReuseWait = l.ReuseWait
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
	spid          int
	logMaxBytes   int64
	logMaxPercent int
}

// NewServerSampler returns a sampler for the given DDL session and log thresholds.
func NewServerSampler(probe sampleProbe, spid int, logMaxBytes int64, logMaxPercent int) *ServerSampler {
	return &ServerSampler{probe: probe, spid: spid, logMaxBytes: logMaxBytes, logMaxPercent: logMaxPercent}
}

var _ Sampler = (*ServerSampler)(nil)

// Blocking reports whether any active session is blocked by our DDL, excluding
// sessions the operator allows to stay blocked (the ignore matcher). When the only
// blocked sessions match an ignore rule, it reports false so the operation holds its
// lock instead of yielding.
func (s *ServerSampler) Blocking(ctx context.Context, ignore IgnoredSessions) (bool, error) {
	sessions, err := s.probe.ActiveSessions(ctx)
	if err != nil {
		return false, err
	}
	for _, sess := range sessions {
		if sess.BlockingSPID == s.spid && !ignore.ignores(sess) {
			return true, nil
		}
	}
	return false, nil
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
