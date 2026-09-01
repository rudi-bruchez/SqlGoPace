package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// blockedCaptureSuffix names the advisory capture file written next to a manifest.
const blockedCaptureSuffix = ".blocked.yaml"

// capturedBlocker is one session our DDL was blocking, recorded when the engine
// reacted, with how often and when it was seen across the run.
type capturedBlocker struct {
	session   mssql.Session
	firstSeen string
	lastSeen  string
	count     int
}

// blockerCapture accumulates the distinct sessions our DDL blocked across the
// reactions of one run, in first-seen order, keyed by a session signature.
type blockerCapture struct {
	order []string
	byKey map[string]*capturedBlocker
}

func (c *blockerCapture) add(s mssql.Session, now string) {
	if c.byKey == nil {
		c.byKey = make(map[string]*capturedBlocker)
	}
	key := fmt.Sprintf("%d|%s|%s|%s|%s", s.SPID, s.Login, s.Host, s.Program, s.ActiveQuery)
	b, ok := c.byKey[key]
	if !ok {
		b = &capturedBlocker{session: s, firstSeen: now}
		c.byKey[key] = b
		c.order = append(c.order, key)
	}
	b.lastSeen = now
	b.count++
}

func (c *blockerCapture) len() int { return len(c.order) }

// captureBlockers records the sessions our DDL is currently blocking — excluding the
// ones the operator allows to stay blocked — into acc, flushes the capture file, and
// returns how many such sessions it saw this call (the current simultaneous count,
// which drives the per-operation peak and the reaction narration). Best-effort: 0
// without a blocker reader or an execution session.
//
// The condition mirrors ServerSampler.Blocking's, suppression included: a victim pending
// a kill (or inside its post-kill grace window) is deliberately excluded from
// BlockState.Unignored, so counting it here would put it in peakBlocked anyway — and
// writing it into .blocked.yaml would offer the operator an ignore_blocked_sessions entry
// for the very session about to be listed in .amplifiers.yaml as killed. The two files
// carry opposite instructions; a session must never appear in both.
func (e *Engine) captureBlockers(ctx context.Context, ignore IgnoreSource, acc *blockerCapture, name string) int {
	if e.blockers == nil || e.session == nil {
		return 0
	}
	sessions, err := e.blockers.ActiveSessions(ctx)
	if err != nil {
		return 0
	}
	rules := currentRules(ignore)
	spid := e.session.SPID()
	now := e.now()
	blocked := 0
	for _, s := range sessions {
		if !s.BlockedBy(spid) || rules.ignores(s) || e.victims.Suppressed(s.SPID) {
			continue
		}
		acc.add(s, now)
		blocked++
	}
	if blocked > 0 {
		e.flushCapture(name, acc)
	}
	return blocked
}

// narrateHeld periodically narrates, once each, the ignored sessions our DDL is
// holding its lock through, so the run log shows we are deliberately blocking them
// (otherwise it is a silent non-event). It runs for the duration of one operation and
// is a no-op without a blocker reader, a session, or a positive poll interval.
func (e *Engine) narrateHeld(ctx context.Context, ignore IgnoreSource, sink ReactionSink) {
	if e.blockers == nil || e.session == nil || e.holdPoll <= 0 {
		return
	}
	ticker := time.NewTicker(e.holdPoll)
	defer ticker.Stop()
	seen := make(map[string]bool)
	spid := e.session.SPID()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sessions, err := e.blockers.ActiveSessions(ctx)
			if err != nil {
				continue
			}
			for _, s := range newHeldBlockers(sessions, spid, currentRules(ignore), seen) {
				sink(ReactionEvent{Kind: "hold", Detail: fmt.Sprintf(
					"holding the lock through ignored session SPID %d (login=%s host=%s app=%s)",
					s.SPID, s.Login, s.Host, s.Program)})
			}
		}
	}
}

// newHeldBlockers returns the ignored sessions our DDL (spid) is currently blocking
// that have not been narrated yet, marking them seen so each is narrated once.
func newHeldBlockers(sessions []mssql.Session, spid int, ignore IgnoredSessions, seen map[string]bool) []mssql.Session {
	var out []mssql.Session
	for _, s := range sessions {
		if !s.BlockedBy(spid) || !ignore.ignores(s) {
			continue
		}
		key := fmt.Sprintf("%d|%s|%s|%s", s.SPID, s.Login, s.Host, s.Program)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

// flushCapture writes the accumulated capture next to the manifest in processing, so
// it is available during the run; it is relocated to the manifest's final directory
// on finalize.
func (e *Engine) flushCapture(name string, acc *blockerCapture) {
	if acc.len() == 0 {
		return
	}
	path := filepath.Join(e.dirs.Processing, name+blockedCaptureSuffix)
	// 0600, not 0644: this sidecar names other people's sessions — login, host, application
	// and the text of the statement they were running. On a shared administrative host that
	// is third-party data, readable by every local user at 0644.
	if err := os.WriteFile(path, renderCapture(name, acc), 0o600); err != nil {
		fmt.Fprintf(e.out, "write blocked-session capture %s: %v\n", name, err)
	}
}

// relocateSidecar moves a capture sidecar (identified by suffix) from processing to
// dir (next to the run report) once a manifest reaches a terminal directory. No-op
// when none was written. label names the sidecar in any error message.
func (e *Engine) relocateSidecar(name, dir, suffix, label string) {
	src := filepath.Join(e.dirs.Processing, name+suffix)
	if _, err := os.Stat(src); err != nil {
		return
	}
	if err := os.Rename(src, filepath.Join(dir, name+suffix)); err != nil {
		fmt.Fprintf(e.out, "relocate %s %s: %v\n", label, name, err)
	}
}

// relocateCaptures moves the three advisory sidecars — the blocked-session capture,
// the .contended.yaml and the .amplifiers.yaml — from processing to dir on finalize.
// Each is a no-op when absent.
func (e *Engine) relocateCaptures(name, dir string) {
	e.relocateSidecar(name, dir, blockedCaptureSuffix, "blocked-session capture")
	e.relocateSidecar(name, dir, contendedCaptureSuffix, "contended capture")
	e.relocateSidecar(name, dir, amplifierCaptureSuffix, "amplifier capture")
}

// renderCapture builds the advisory blocked-session capture file: a commented,
// ready-to-paste ignore_blocked_sessions block plus an observed: diagnostics block.
// SqlGoPace never reads this file back — promoting an entry into the manifest's
// ignore_blocked_sessions is a deliberate operator (or TUI) step.
func renderCapture(name string, acc *blockerCapture) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Blocked-session capture for %s\n", name)
	b.WriteString("# Advisory only — SqlGoPace never reads this file back.\n")
	b.WriteString("# To let one of these sessions stay blocked, copy its entry into the manifest's\n")
	b.WriteString("# ignore_blocked_sessions: list (keep only the fields you want; all are regexps).\n#\n")
	b.WriteString("# ignore_blocked_sessions:\n")
	for _, key := range acc.order {
		s := acc.byKey[key].session
		fmt.Fprintf(&b, "#   - session_id: %d\n", s.SPID)
		writeCommentedRule(&b, "app_name", s.Program)
		writeCommentedRule(&b, "login_name", s.Login)
		writeCommentedRule(&b, "host_name", s.Host)
	}
	b.WriteString("\nobserved:\n")
	for _, key := range acc.order {
		cb := acc.byKey[key]
		s := cb.session
		fmt.Fprintf(&b, "  - session_id: %d\n", s.SPID)
		writeYAMLString(&b, "    login_name", s.Login)
		writeYAMLString(&b, "    host_name", s.Host)
		writeYAMLString(&b, "    app_name", s.Program)
		writeYAMLString(&b, "    wait_type", s.WaitType)
		fmt.Fprintf(&b, "    wait_ms: %d\n", s.WaitMS)
		fmt.Fprintf(&b, "    open_transactions: %d\n", s.OpenTransactions)
		writeYAMLString(&b, "    active_query", s.ActiveQuery)
		writeYAMLString(&b, "    parent_query", s.ParentQuery)
		fmt.Fprintf(&b, "    times_blocked: %d\n", cb.count)
		writeYAMLString(&b, "    first_seen", cb.firstSeen)
		writeYAMLString(&b, "    last_seen", cb.lastSeen)
	}
	return []byte(b.String())
}

// writeCommentedRule emits a commented, anchored, literal-matching regexp rule field
// (skipping empty values), so the operator can paste it as-is or loosen it.
func writeCommentedRule(b *strings.Builder, field, val string) {
	if val == "" {
		return
	}
	fmt.Fprintf(b, "#     %s: %q\n", field, "^"+regexp.QuoteMeta(val)+"$")
}

// writeYAMLString writes "key: value" with a quoted scalar, omitting empty values.
func writeYAMLString(b *strings.Builder, key, val string) {
	if val == "" {
		return
	}
	fmt.Fprintf(b, "%s: %q\n", key, val)
}
