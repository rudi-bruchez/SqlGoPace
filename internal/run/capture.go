package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
// ones the operator allows to stay blocked — into acc, and flushes the capture file.
// Best-effort: a no-op without a blocker reader or an execution session.
func (e *Engine) captureBlockers(ctx context.Context, ignore IgnoredSessions, acc *blockerCapture, name string) {
	if e.blockers == nil || e.session == nil {
		return
	}
	sessions, err := e.blockers.ActiveSessions(ctx)
	if err != nil {
		return
	}
	spid := e.session.SPID()
	now := e.now()
	changed := false
	for _, s := range sessions {
		if s.BlockingSPID != spid || ignore.ignores(s) {
			continue
		}
		acc.add(s, now)
		changed = true
	}
	if changed {
		e.flushCapture(name, acc)
	}
}

// flushCapture writes the accumulated capture next to the manifest in processing, so
// it is available during the run; it is relocated to the manifest's final directory
// on finalize.
func (e *Engine) flushCapture(name string, acc *blockerCapture) {
	if acc.len() == 0 {
		return
	}
	path := filepath.Join(e.dirs.Processing, name+blockedCaptureSuffix)
	if err := os.WriteFile(path, renderCapture(name, acc), 0o644); err != nil {
		fmt.Fprintf(e.out, "write blocked-session capture %s: %v\n", name, err)
	}
}

// relocateCapture moves the capture from processing to dir (next to the run report)
// once a manifest reaches a terminal directory. No-op when none was written.
func (e *Engine) relocateCapture(name, dir string) {
	src := filepath.Join(e.dirs.Processing, name+blockedCaptureSuffix)
	if _, err := os.Stat(src); err != nil {
		return
	}
	if err := os.Rename(src, filepath.Join(dir, name+blockedCaptureSuffix)); err != nil {
		fmt.Fprintf(e.out, "relocate blocked-session capture %s: %v\n", name, err)
	}
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
