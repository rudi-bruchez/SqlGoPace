package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// View implements tea.Model. It renders the incident console as a bordered multi-panel
// dashboard: a header banner (app + server), an operations panel (all ops with status), an
// op-status panel (current op detail), a blocked | waits row, and a shortcuts footer. The
// two-panel row sits side by side on a wide terminal and stacks on a narrow one.
//
// Every block except the operations panel is rendered first and measured; the operations
// panel then takes the leftover terminal height and, when its list is taller than that,
// shows a window around the running op with a summary line (see operationsBody). This keeps
// the lower, actionable panels (blocked sessions, footer) on screen for a manifest expanded
// to many operations (index: ALL), which would otherwise overflow the fixed alt-screen.
func (m Model) View() string {
	if m.rosterOpen {
		return m.rosterView()
	}
	w := m.width
	if w <= 0 {
		w = 100 // before the first WindowSizeMsg (alt-screen sends one promptly)
	}
	full := max(w-boxChrome, 20) // inner width of a full-width bordered panel

	// Every block other than the operations panel, so its body can take the remaining height.
	alerts := m.alertsBlock()

	nameBox := panel("", titleStyle.Render("SqlGoPace")+"  "+m.server.App, accentColor, 0)
	rightW := max(w-lipgloss.Width(nameBox)-colGap-boxChrome, 20)
	// The banner right-aligns against the right box's inner text area (contentWidth minus the
	// two padding columns), so the server identity hugs the terminal's right edge.
	header := joinRow(w, nameBox, panel("", m.serverBanner(rightW-2), accentColor, rightW))

	statusTitle := "op status"
	if m.stepTotal > 0 {
		statusTitle = fmt.Sprintf("op %d/%d status", m.stepIndex, m.stepTotal)
	}
	opStatus := panel(statusTitle, m.opStatusBody(), accentColor, full)

	col := full
	if w >= sideBySideMin {
		col = max((w-colGap)/2-boxChrome, 20)
	}
	blockedWaits := joinRow(w,
		panel("", m.blockedBody(col), secondaryColor, col),
		panel("", m.waitsBody(), secondaryColor, col))

	footer := ""
	if m.showHelp {
		footer = panel("", m.helpBody(), secondaryColor, full)
	}

	// Height budget for the operations panel body: whatever the terminal has left after the
	// other blocks, the single-newline separators between all blocks, this panel's own chrome
	// (title + top/bottom border = 3), and a small safety margin. 0 means "no cap, show all".
	others := []string{alerts, header, opStatus, blockedWaits, footer}
	opsBudget := 0
	if m.height > 0 {
		used, blocks := 0, 1 // the operations panel itself is always one block
		for _, blk := range others {
			if blk != "" {
				used += lipgloss.Height(blk)
				blocks++
			}
		}
		if m.notice != "" {
			used++
		}
		opsBudget = max(m.height-used-(blocks-1)-3-1, minOpsRows)
	}
	ops := panel("operations", m.operationsBody(opsBudget), accentColor, full)

	// Assemble top to bottom, blocks separated by a single newline.
	var b strings.Builder
	for _, blk := range []string{alerts, header, ops, opStatus, blockedWaits, footer} {
		if blk == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(blk)
	}
	if m.notice != "" {
		b.WriteString("\n" + m.notice)
	}
	return b.String()
}

// alertsBlock renders the sticky failure alerts shown above the dashboard, or "" when none.
func (m Model) alertsBlock() string {
	if len(m.alerts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, a := range m.alerts {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(alertStyle.Render("⚠ " + a.Title))
		for _, line := range a.Lines {
			b.WriteString("\n" + alertStyle.Render("    "+line))
		}
	}
	return b.String()
}

// serverBanner renders the two-line server-info body of the header's right box, right-aligned
// to width columns (the box's inner text area) so it hugs the right edge; width <= 0 leaves it
// unaligned (used before the box width is known).
func (m Model) serverBanner(width int) string {
	s := m.server
	body := "server info pending…"
	if s.Name != "" || s.Product != "" || s.Database != "" {
		l1 := fmt.Sprintf("%s, %s — %s", s.Name, s.Product, s.Database)
		l2 := fmt.Sprintf("ed=%s  adr=%s  recovery=%s  rcsi=%s  si=%s",
			s.Edition, tf(s.ADR), s.Recovery, tf(s.RCSI), tf(s.SnapshotIso))
		body = l1 + "\n" + l2
	}
	if width <= 0 {
		return body
	}
	return lipgloss.NewStyle().Width(width).Align(lipgloss.Right).Render(body)
}

// minOpsRows is the fewest operation rows the panel ever shows, even on a tiny terminal, so
// the running op and a summary always remain visible.
const minOpsRows = 3

// operationsBody renders the operations panel body. Before the full list arrives it falls
// back to the single current operation. When the list has more rows than budget allows
// (budget > 0), it shows a summary line plus a window of rows centered on the running op, so
// a long list (index: ALL) never pushes the lower panels off the fixed alt-screen.
func (m Model) operationsBody(budget int) string {
	if len(m.ops) == 0 {
		op := m.operation
		if op == "" {
			op = "(waiting)"
		}
		line := fmt.Sprintf("%s   %s", op, opStatusStyled(m.displayStatus().String()))
		if m.spid > 0 {
			line += fmt.Sprintf("   SPID %d", m.spid)
		}
		if m.elapsed > 0 {
			line += "   elapsed " + formatElapsed(m.elapsed)
		}
		return line
	}
	rows := make([]string, len(m.ops))
	for i, o := range m.ops {
		rows[i] = m.opRow(o)
	}
	if budget <= 0 || len(rows) <= budget {
		return strings.Join(rows, "\n")
	}

	// Too many ops for the viewport: one summary line + a window centered on the running op.
	win := max(budget-1, 1)
	start := max(m.stepIndex-1-win/2, 0) // stepIndex is 1-based; 0 when no op is running yet
	start = min(start, len(rows)-win)
	out := make([]string, 0, win+1)
	out = append(out, helpStyle.Render(m.opsSummary()))
	out = append(out, rows[start:start+win]...)
	return strings.Join(out, "\n")
}

// opRow renders one operation's row: "N - label  [STATUS]" plus SPID/elapsed on the running
// row. The running row reflects the live lifecycle status (SUSPENDED while blocked, or
// DRAINING/CANCELING/PAUSED when set), not just RUNNING.
func (m Model) opRow(o OperationRow) string {
	status := o.Status
	if status == "" {
		status = "TO RUN"
	}
	if o.Index == m.stepIndex && status == "RUNNING" {
		status = m.displayStatus().String()
	}
	line := fmt.Sprintf("%d - %s   %s", o.Index, o.Label, opStatusStyled(status))
	if o.Index == m.stepIndex {
		if m.spid > 0 {
			line += fmt.Sprintf("   SPID %d", m.spid)
		}
		if m.elapsed > 0 {
			line += "   elapsed " + formatElapsed(m.elapsed)
		}
	}
	return line
}

// opsSummary is the one-line roll-up shown when the operations list is windowed: total ops
// and how many are done / failed / still to run.
func (m Model) opsSummary() string {
	var done, failed, toRun int
	for _, o := range m.ops {
		switch o.Status {
		case "DONE", "SKIPPED":
			done++
		case "FAILED", "INTERRUPTED", "INCOMPLETE":
			failed++
		case "RUNNING": // the running op is shown in the window, not counted here
		default:
			toRun++ // "TO RUN" or unset
		}
	}
	s := fmt.Sprintf("… %d ops · %d done", len(m.ops), done)
	if failed > 0 {
		s += fmt.Sprintf(" · %d failed", failed)
	}
	return s + fmt.Sprintf(" · %d to run", toRun)
}

// opStatusBody renders the current operation's detail: the suspension summary, the live
// victim block (if any), and the shrink/batch/generic progress line.
func (m Model) opStatusBody() string {
	var b strings.Builder
	if m.suspension.Episodes > 0 {
		b.WriteString(m.suspensionLine())
		b.WriteByte('\n')
	}
	if m.blockedBy.Blocked {
		b.WriteString(alertStyle.Render(m.blockedByLine()))
		b.WriteByte('\n')
		if q := flatten(m.blockedBy.Query); q != "" {
			fmt.Fprintf(&b, "    %s\n", truncate(q, 100))
		}
	}
	switch {
	case m.hasShrink:
		b.WriteString(m.shrinkLines())
	case m.hasBatch:
		fmt.Fprintf(&b, "batch %s %s: %d/%d rows (%.0f%%)   batch=%d   %.0f rows/s",
			m.batch.Verb, m.batch.Table, m.batch.RowsDone, m.batch.EstRows,
			m.batch.Percent*100, m.batch.BatchRows, m.batch.RowsPerSec)
	default:
		fmt.Fprintf(&b, "progress: %.0f%%   ETA: %ds", m.percent, m.etaSeconds)
		if m.rollbackPercent > 0 {
			fmt.Fprintf(&b, "   rollback: %.0f%%", m.rollbackPercent)
		}
	}
	body := strings.TrimRight(b.String(), "\n")
	if body == "" {
		return "(no active operation)"
	}
	return body
}

// suspensionLine is the cumulative "suspended N×, … total — …" summary.
func (m Model) suspensionLine() string {
	s := m.suspension
	line := fmt.Sprintf("suspended %d×, %s total", s.Episodes, humanizeMS(s.TotalMS))
	groups := m.suspensionGroups()
	if len(groups) > 0 {
		parts := make([]string, 0, len(groups))
		for _, g := range groups {
			who := "SPID " + g.spids
			if g.login != "" {
				who += " " + g.login
			}
			killed := ""
			if g.killed > 0 {
				killed = fmt.Sprintf(", %d killed", g.killed)
			}
			parts = append(parts, fmt.Sprintf("%s (%d×, %s%s)", who, g.count, humanizeMS(g.totalMS), killed))
		}
		line += " — " + strings.Join(parts, " · ")
	}
	return line
}

// blockedByLine is the live "⚠ BLOCKED by SPID …" victim indicator.
func (m Model) blockedByLine() string {
	bb := m.blockedBy
	line := fmt.Sprintf("⚠ BLOCKED by SPID %d", bb.SPID)
	if bb.Login != "" {
		line += "  login=" + bb.Login
	}
	if bb.WaitType != "" {
		line += "  wait=" + bb.WaitType
	}
	return line + "  " + humanizeMS(bb.WaitMS)
}

// shrinkLines is the two-line shrink progress block (size/step, then chunks + dual ETA +
// blocked time).
func (m Model) shrinkLines() string {
	sh := m.shrink
	var b strings.Builder
	fmt.Fprintf(&b, "shrink %s (%s): %s → %s target (from %s, %.0f%%)   step %s\n",
		sh.File, sh.Type, HumanizeMB(sh.CurrentMB), HumanizeMB(sh.FinalMB), HumanizeMB(sh.StartMB),
		sh.Percent*100, HumanizeMB(sh.StepMB))
	fmt.Fprintf(&b, "  chunk %d done", sh.Chunks)
	if sh.ChunksRemaining > 0 {
		fmt.Fprintf(&b, " · ~%d left", sh.ChunksRemaining)
	}
	if sh.ETASeconds > 0 {
		fmt.Fprintf(&b, " · ETA %s", humanizeMS(int64(sh.ETASeconds)*1000))
		if sh.ETASecondsNoBlock > 0 && sh.ETASecondsNoBlock < sh.ETASeconds {
			fmt.Fprintf(&b, " (%s if unblocked)", humanizeMS(int64(sh.ETASecondsNoBlock)*1000))
		}
	}
	if sh.BlockedSeconds > 0 {
		fmt.Fprintf(&b, " · blocked %s", humanizeMS(int64(sh.BlockedSeconds)*1000))
	}
	// The literal statement in flight, so the operator sees exactly what runs each step: the
	// TRUNCATEONLY pass first, then each DBCC SHRINKFILE (file, target) chunk with its target.
	if sh.Statement != "" {
		fmt.Fprintf(&b, "\n  → %s", sh.Statement)
		if sh.ChunkTargetMB > 0 {
			fmt.Fprintf(&b, "   (shrink to %s)", HumanizeMB(sh.ChunkTargetMB))
		}
		// SQL Server's own percent_complete for the running chunk, when it reports one.
		if sh.PercentComplete > 0 {
			fmt.Fprintf(&b, "   server %.0f%%", sh.PercentComplete)
		}
	}
	return b.String()
}

// blockedBody renders the actionable blocked-session list (the sessions our DDL blocks). The
// selected row is highlighted; when expandSQL is on it shows that row's full SQL wrapped to
// the panel width, otherwise a truncated preview.
func (m Model) blockedBody(width int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "blocked sessions (%d):", len(m.blockers))
	qw := max(width-6, 20)
	for i, bl := range m.blockers {
		b.WriteByte('\n')
		marker := "  "
		line := fmt.Sprintf("SPID %d  login=%s  wait=%s  %s", bl.SPID, bl.Login, bl.WaitType, humanizeMS(bl.WaitMS))
		if i == m.cursor {
			marker = "> "
			line = selStyle.Render(line)
		}
		b.WriteString(marker + line)
		if q := flatten(bl.Query); q != "" {
			if i == m.cursor && m.expandSQL {
				fmt.Fprintf(&b, "\n    %s", wrapText(q, qw))
			} else {
				fmt.Fprintf(&b, "\n    %s", truncate(q, qw))
			}
		}
	}
	if len(m.blockers) == 0 {
		b.WriteString("\n  (none)")
	}
	b.WriteString("\n" + helpStyle.Render("[enter] toggle full sql"))
	return b.String()
}

// rosterView renders the blocker-roster modal: every login (or host) that has stalled the DDL
// this run, with its episode count and total blocked time, and a ✓ on armed groups. It replaces
// the dashboard while open (the alt-screen is a fixed height, so a full-screen modal is simplest).
func (m Model) rosterView() string {
	groupBy, other := "login", "host"
	if m.rosterByHost {
		groupBy, other = "host", "login"
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("blockers that stalled this run") + " — grouped by " + groupBy + "\n\n")

	groups := m.rosterGroups()
	if len(groups) == 0 {
		b.WriteString("  (no blockers yet)\n")
	}
	for i, g := range groups {
		label := g.Value
		if label == "" {
			label = "(unknown)"
		}
		line := fmt.Sprintf("%-30s  %d×  blocked %s", label, g.Count, humanizeMS(g.TotalMS))
		if g.Value != "" && m.armed[rosterKey(g.Criterion, g.Value)] {
			line += "  " + okStyle.Render("[✓ armed]")
		}
		marker := "  "
		if i == m.rosterCursor {
			marker = "> "
			line = selStyle.Render(line)
		}
		b.WriteString(marker + line + "\n")
	}

	b.WriteString("\n")
	if !m.killerArmed {
		b.WriteString(alertStyle.Render("kill_blockers disabled in config — armed rules won't fire until it's enabled") + "\n")
	}
	b.WriteString(helpStyle.Render(fmt.Sprintf("[↑/↓] select  [space] arm/disarm  [g] group by %s  [b/esc/q] close", other)))
	return b.String()
}

// waitsBody renders the wait-category table (what is slowing the DDL).
func (m Model) waitsBody() string {
	var b strings.Builder
	fmt.Fprintf(&b, "waits slowing the DDL (total %s):", humanizeMS(m.waitTotalMS))
	for _, wc := range m.waits {
		fmt.Fprintf(&b, "\n  %-20s %10s  %s tasks", wc.Name, humanizeMS(wc.WaitMS), humanizeCount(wc.Tasks))
	}
	if len(m.waits) == 0 {
		b.WriteString("\n  (none)")
	}
	return b.String()
}

// helpBody is the footer: the normal shortcut line, or the active criterion sub-prompt.
func (m Model) helpBody() string {
	if m.inCriterionMode() && m.cursor < len(m.blockers) {
		bl := m.blockers[m.cursor]
		verb := "ignore"
		if m.mode == modeKillCriterion {
			verb = "kill+auto-kill"
		}
		return fmt.Sprintf("%s SPID %d as:  [s] session_id  [a] app=%s  [l] login=%s  [h] host=%s   [esc] cancel",
			verb, bl.SPID, bl.Program, bl.Login, bl.Host)
	}
	return helpStyle.Render("[↑/↓] select  [enter] sql  [i] ignore  [x] kill  [X] kill+auto  [b] blockers  [k] kill DDL  [d] drain  [?] help  [q] quit")
}

// opStatusStyled renders a bracketed, color-coded operation status label.
func opStatusStyled(status string) string {
	label := "[" + status + "]"
	switch status {
	case "SUSPENDED":
		return warnStyle.Render(label)
	case "DONE", "TO RUN":
		return okStyle.Render(label)
	case "FAILED", "INTERRUPTED", "INCOMPLETE":
		return alertStyle.Render(label)
	default:
		return titleStyle.Render(label) // RUNNING / SKIPPED / other
	}
}

// tf renders a boolean as the compact "t"/"f" the banner uses.
func tf(v bool) string {
	if v {
		return "t"
	}
	return "f"
}

// wrapText word-wraps s to width columns (used to show a blocker's full SQL).
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	return lipgloss.NewStyle().Width(width).Render(s)
}

// flatten collapses each run of whitespace (including the newlines and tabs common in
// DMV-captured SQL text) into a single space, so a query renders on one clean line before
// truncation or wrapping instead of scattering across rows.
func flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
