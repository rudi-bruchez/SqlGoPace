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
func (m Model) View() string {
	w := m.width
	if w <= 0 {
		w = 100 // before the first WindowSizeMsg (alt-screen sends one promptly)
	}
	full := max(w-boxChrome, 20) // inner width of a full-width bordered panel

	var b strings.Builder

	// Alerts stay prominent, above the dashboard.
	for _, a := range m.alerts {
		fmt.Fprintf(&b, "%s\n", alertStyle.Render("⚠ "+a.Title))
		for _, line := range a.Lines {
			fmt.Fprintf(&b, "%s\n", alertStyle.Render("    "+line))
		}
	}

	// 1. Header — name+version box | server-info box (the server box fills the rest).
	nameBox := panel("", titleStyle.Render("SqlGoPace")+"  "+m.server.App, accentColor, 0)
	rightW := max(w-lipgloss.Width(nameBox)-colGap-boxChrome, 20)
	serverBox := panel("", m.serverBanner(), accentColor, rightW)
	b.WriteString(joinRow(w, nameBox, serverBox))
	b.WriteByte('\n')

	// 2. operations
	b.WriteString(panel("operations", m.operationsBody(), accentColor, full))
	b.WriteByte('\n')

	// 3. op N status
	statusTitle := "op status"
	if m.stepTotal > 0 {
		statusTitle = fmt.Sprintf("op %d/%d status", m.stepIndex, m.stepTotal)
	}
	b.WriteString(panel(statusTitle, m.opStatusBody(), accentColor, full))
	b.WriteByte('\n')

	// 4. blocked | waits — one column width; joinRow places them side by side (wide) or
	// stacked (narrow) on the same threshold.
	col := full
	if w >= sideBySideMin {
		col = max((w-colGap)/2-boxChrome, 20)
	}
	blocked := panel("", m.blockedBody(col), secondaryColor, col)
	waits := panel("", m.waitsBody(), secondaryColor, col)
	b.WriteString(joinRow(w, blocked, waits))
	b.WriteByte('\n')

	// 5. shortcuts footer (toggle with '?').
	if m.showHelp {
		b.WriteString(panel("", m.helpBody(), secondaryColor, full))
	}
	if m.notice != "" {
		b.WriteString("\n" + m.notice)
	}
	return b.String()
}

// serverBanner renders the two-line server-info body of the header's right box.
func (m Model) serverBanner() string {
	s := m.server
	if s.Name == "" && s.Product == "" && s.Database == "" {
		return "server info pending…"
	}
	l1 := fmt.Sprintf("%s, %s — %s", s.Name, s.Product, s.Database)
	l2 := fmt.Sprintf("ed=%s  adr=%s  recovery=%s  rcsi=%s  si=%s",
		s.Edition, tf(s.ADR), s.Recovery, tf(s.RCSI), tf(s.SnapshotIso))
	return l1 + "\n" + l2
}

// operationsBody renders every operation of the running manifest with its status. Before the
// full list arrives it falls back to the single current operation.
func (m Model) operationsBody() string {
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
	var b strings.Builder
	for i, o := range m.ops {
		if i > 0 {
			b.WriteByte('\n')
		}
		status := o.Status
		if status == "" {
			status = "TO RUN"
		}
		// The current running op shows SUSPENDED while we are the victim of a block.
		if o.Index == m.stepIndex && status == "RUNNING" && m.blockedBy.Blocked {
			status = "SUSPENDED"
		}
		fmt.Fprintf(&b, "%d - %s   %s", o.Index, o.Label, opStatusStyled(status))
		if o.Index == m.stepIndex {
			if m.spid > 0 {
				fmt.Fprintf(&b, "   SPID %d", m.spid)
			}
			if m.elapsed > 0 {
				fmt.Fprintf(&b, "   elapsed %s", formatElapsed(m.elapsed))
			}
		}
	}
	return b.String()
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
		if q := m.blockedBy.Query; q != "" {
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
	if len(s.Blockers) > 0 {
		parts := make([]string, 0, len(s.Blockers))
		for _, bl := range s.Blockers {
			who := fmt.Sprintf("SPID %d", bl.SPID)
			if bl.Login != "" {
				who += " " + bl.Login
			}
			parts = append(parts, fmt.Sprintf("%s (%d×, %s)", who, bl.Count, humanizeMS(bl.TotalMS)))
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
		if bl.Query != "" {
			if i == m.cursor && m.expandSQL {
				fmt.Fprintf(&b, "\n    %s", wrapText(bl.Query, qw))
			} else {
				fmt.Fprintf(&b, "\n    %s", truncate(bl.Query, qw))
			}
		}
	}
	if len(m.blockers) == 0 {
		b.WriteString("\n  (none)")
	}
	b.WriteString("\n" + helpStyle.Render("[enter] toggle full sql"))
	return b.String()
}

// waitsBody renders the wait-category table (what is slowing the DDL).
func (m Model) waitsBody() string {
	var b strings.Builder
	fmt.Fprintf(&b, "waits slowing the DDL (total %s):", humanizeMS(m.waitTotalMS))
	for _, wc := range m.waits {
		fmt.Fprintf(&b, "\n  %-20s %10s  %d tasks", wc.Name, humanizeMS(wc.WaitMS), wc.Tasks)
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
	return helpStyle.Render("[↑/↓] select  [enter] sql  [i] ignore  [x] kill  [X] kill+auto  [k] kill DDL  [d] drain  [?] help  [q] quit")
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
