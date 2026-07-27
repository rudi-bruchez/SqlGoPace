package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFormatElapsed(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "00:05"},
		{3*time.Minute + 20*time.Second, "03:20"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1:02:03"},
	}
	for _, tt := range tests {
		if got := formatElapsed(tt.d); got != tt.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestHelpBodyGatesShortcutsOnState(t *testing.T) {
	blockerKeys := []string{"[↑/↓] select", "[enter] sql", "[i] ignore", "[x] kill", "[X] kill+auto"}

	// No blockers and no suspension history: only the always-valid keys, none of the
	// blocker keys and no [b] blockers.
	idle := Model{}.helpBody()
	for _, k := range blockerKeys {
		if strings.Contains(idle, k) {
			t.Errorf("idle footer advertises no-op %q:\n%s", k, idle)
		}
	}
	if strings.Contains(idle, "[b] blockers") {
		t.Errorf("idle footer shows [b] blockers with no history:\n%s", idle)
	}
	for _, k := range []string{"[k] kill DDL", "[d] drain", "[?] help", "[q] quit"} {
		if !strings.Contains(idle, k) {
			t.Errorf("idle footer missing always-valid key %q:\n%s", k, idle)
		}
	}

	// A live blocker brings back the blocker keys.
	blocked := Model{blockers: []Blocker{{SPID: 51}}}.helpBody()
	for _, k := range blockerKeys {
		if !strings.Contains(blocked, k) {
			t.Errorf("blocked footer missing %q:\n%s", k, blocked)
		}
	}

	// Suspension history (but nothing blocking right now) enables [b] but not the
	// per-session action keys.
	history := Model{suspension: SuspensionMsg{Blockers: []SuspensionBlocker{{Login: "SVC_RPT"}}}}.helpBody()
	if !strings.Contains(history, "[b] blockers") {
		t.Errorf("footer with suspension history missing [b] blockers:\n%s", history)
	}
	if strings.Contains(history, "[x] kill") {
		t.Errorf("footer with no live blocker still shows [x] kill:\n%s", history)
	}

	// Draining flips [d] to a resume affordance.
	draining := Model{status: StatusDraining}.helpBody()
	if !strings.Contains(draining, "[d] resume") || strings.Contains(draining, "[d] drain") {
		t.Errorf("draining footer should offer [d] resume, not [d] drain:\n%s", draining)
	}
}

func TestHumanizeMS(t *testing.T) {
	const h = int64(3_600_000)
	tests := []struct {
		ms   int64
		want string
	}{
		{500, "500ms"},
		{2800, "2.8s"},
		{6*60_000 + 35_000, "6m35s"},
		{1*h + 4*60_000, "1h04m"},
		// Past 72h the unit rolls from hours to days.
		{71*h + 59*60_000, "71h59m"},
		{72 * h, "3d00h"},
		{774*h + 58*60_000, "32d06h"}, // the reported ETA: 774h58m
	}
	for _, tt := range tests {
		if got := humanizeMS(tt.ms); got != tt.want {
			t.Errorf("humanizeMS(%d) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestHumanizeCount(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{21, "21"},
		{999, "999"},
		{1000, "1.0k"},
		{1500, "1.5k"},
		{21000, "21k"},
		{768162, "768k"},
		{999999, "1000k"}, // rounds up within the thousands band
		{1_000_000, "1.0m"},
		{7446916, "7.4m"},
		{104_492_000, "104m"},
	}
	for _, tt := range tests {
		if got := humanizeCount(tt.n); got != tt.want {
			t.Errorf("humanizeCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestOpStatusLabel(t *testing.T) {
	tests := map[string]string{
		"success": "DONE", "failed": "FAILED", "interrupted": "INTERRUPTED",
		"incomplete": "INCOMPLETE", "skipped": "SKIPPED", "weird": "WEIRD",
	}
	for outcome, want := range tests {
		if got := opStatusLabel(outcome); got != want {
			t.Errorf("opStatusLabel(%q) = %q, want %q", outcome, got, want)
		}
	}
}

func TestJoinRowStacksWhenNarrow(t *testing.T) {
	a, b := "AAA", "BBB"
	// Wide: side by side, so the two panels share lines (their content is not separated by a
	// bare newline join). Narrow: stacked, so a newline separates them.
	if got := joinRow(sideBySideMin, a, b); got == a+"\n"+b {
		t.Errorf("at width %d joinRow should place panels side by side, got stacked", sideBySideMin)
	}
	if got := joinRow(sideBySideMin-1, a, b); got != a+"\n"+b {
		t.Errorf("below the threshold joinRow should stack: got %q", got)
	}
}

func TestSetOpStatus(t *testing.T) {
	m := Model{ops: []OperationRow{{Index: 1, Status: "TO RUN"}, {Index: 2, Status: "TO RUN"}}}
	m.setOpStatus(2, "RUNNING")
	if m.ops[1].Status != "RUNNING" || m.ops[0].Status != "TO RUN" {
		t.Errorf("setOpStatus targeted the wrong row: %+v", m.ops)
	}
	m.setOpStatus(99, "DONE") // unknown index: no-op, no panic
}

func TestTickUpdatesElapsed(t *testing.T) {
	start := time.Unix(1000, 0)
	m := Model{startedAt: start}
	updated, cmd := m.Update(tickMsg(start.Add(90 * time.Second)))
	m = updated.(Model)
	if m.elapsed != 90*time.Second {
		t.Errorf("elapsed = %v, want 90s", m.elapsed)
	}
	if cmd == nil {
		t.Error("tick should reschedule itself (non-nil cmd)")
	}
}

func TestSuspensionLineGroupsByLoginAndCountsKills(t *testing.T) {
	m := New("shrink DataFile", nil)
	m.suspension = SuspensionMsg{
		Episodes: 3, TotalMS: 238000,
		Blockers: []SuspensionBlocker{
			{SPID: 103, Login: "SVC_OBS", Count: 1, TotalMS: 60000},
			{SPID: 88, Login: "SVC_OBS", Count: 1, TotalMS: 89000},
			{SPID: 86, Login: "SVC_OBS", Count: 1, TotalMS: 89000},
		},
	}
	// Two of the three SPIDs from this login were killed; the group folds their SPIDs,
	// episode counts, blocked time, and kills into one entry.
	for _, spid := range []int{88, 86} {
		updated, _ := m.Update(KilledMsg{SPID: spid, Login: "SVC_OBS"})
		m = updated.(Model)
	}
	line := m.suspensionLine()
	if want := "SPID 103,88,86 SVC_OBS (3×, 3m58s, 2 killed)"; !strings.Contains(line, want) {
		t.Errorf("suspensionLine() = %q, want it to contain %q", line, want)
	}
}

func TestSuspensionLineKeepsDistinctLoginsSeparate(t *testing.T) {
	m := New("shrink DataFile", nil)
	m.suspension = SuspensionMsg{
		Episodes: 2, TotalMS: 120000,
		Blockers: []SuspensionBlocker{
			{SPID: 103, Login: "SVC_OBS", Count: 1, TotalMS: 60000},
			{SPID: 88, Login: "SVC_ARC", Count: 1, TotalMS: 60000},
		},
	}
	line := m.suspensionLine()
	for _, want := range []string{"SPID 103 SVC_OBS (1×, 1m00s)", "SPID 88 SVC_ARC (1×, 1m00s)"} {
		if !strings.Contains(line, want) {
			t.Errorf("suspensionLine() = %q, want it to contain %q", line, want)
		}
	}
	if strings.Contains(line, "killed") {
		t.Errorf("suspensionLine() = %q, want no kill annotation when nothing was killed", line)
	}
}

func TestRosterGroupsAggregate(t *testing.T) {
	m := New("op", nil)
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{
		{SPID: 104, Login: "APP01", Host: "SRV1", Count: 1, TotalMS: 1000},
		{SPID: 105, Login: "APP01", Host: "SRV2", Count: 2, TotalMS: 2000},
	}}
	// Default grouping is by login: one APP01 group, count 3, total 3000.
	g := m.rosterGroups()
	if len(g) != 1 || g[0].Criterion != "login_name" || g[0].Value != "APP01" || g[0].Count != 3 || g[0].TotalMS != 3000 {
		t.Fatalf("login groups = %+v", g)
	}
	// By host: two groups.
	m.rosterByHost = true
	if g := m.rosterGroups(); len(g) != 2 || g[0].Criterion != "host_name" {
		t.Fatalf("host groups = %+v, want 2 by host_name", g)
	}
}

func TestRosterOpenToggleAndClose(t *testing.T) {
	actions := make(chan Action, 8)
	m := New("op", actions)
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{
		{SPID: 104, Login: "APP01", Host: "SRV1", Count: 1, TotalMS: 1000},
		{SPID: 105, Login: "APP02", Host: "SRV2", Count: 1, TotalMS: 1000},
	}}
	// b opens.
	mo, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	m = mo.(Model)
	if !m.rosterOpen {
		t.Fatal("b should open the roster")
	}
	// g toggles grouping and resets the cursor.
	m.rosterCursor = 1
	mg, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	m = mg.(Model)
	if !m.rosterByHost || m.rosterCursor != 0 {
		t.Fatalf("g should group by host and reset cursor: byHost=%v cursor=%d", m.rosterByHost, m.rosterCursor)
	}
	// q closes the roster without quitting or emitting an action.
	mc, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = mc.(Model)
	if m.rosterOpen || m.quitting || cmd != nil {
		t.Fatalf("q should close roster only: open=%v quitting=%v cmd=%v", m.rosterOpen, m.quitting, cmd)
	}
	select {
	case a := <-actions:
		t.Errorf("closing roster emitted an action: %+v", a)
	default:
	}
}

func TestRosterArmThenDisarm(t *testing.T) {
	actions := make(chan Action, 8)
	m := New("op", actions)
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{
		{SPID: 104, Login: "APP01", Host: "SRV1", Count: 2, TotalMS: 20000},
	}}
	m.rosterOpen = true

	// space arms the selected (login) group.
	ma, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = ma.(Model)
	a := <-actions
	if a.Kind != ActionArmKillRule || a.Criterion != "login_name" || a.Value != "APP01" {
		t.Fatalf("arm action = %+v", a)
	}
	if !m.armed["login_name=APP01"] {
		t.Error("armed set not updated after arm")
	}

	// space again disarms.
	md, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = md.(Model)
	a = <-actions
	if a.Kind != ActionDisarmKillRule || a.Criterion != "login_name" || a.Value != "APP01" {
		t.Fatalf("disarm action = %+v", a)
	}
	if m.armed["login_name=APP01"] {
		t.Error("armed set not cleared after disarm")
	}
}

func TestRosterUnknownRowIsNotArmable(t *testing.T) {
	actions := make(chan Action, 8)
	m := New("op", actions)
	// A blocker with no login/host (blocker was not in the snapshot).
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{{SPID: 104, Count: 1, TotalMS: 1000}}}
	m.rosterOpen = true
	mu, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = mu.(Model)
	select {
	case a := <-actions:
		t.Errorf("arming the (unknown) row should emit nothing, got %+v", a)
	default:
	}
}

func TestRosterCtrlCQuits(t *testing.T) {
	actions := make(chan Action, 8)
	m := New("op", actions)
	m.rosterOpen = true
	mc, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = mc.(Model)
	if !m.quitting {
		t.Error("ctrl+c in the roster should quit the app")
	}
	if cmd == nil {
		t.Error("ctrl+c should return a quit command")
	}
}

func TestKillerArmedMsg(t *testing.T) {
	m := New("op", nil)
	mo, _ := m.Update(KillerArmedMsg{Armed: true})
	if !mo.(Model).killerArmed {
		t.Error("KillerArmedMsg{Armed:true} should set killerArmed")
	}
}

func TestRosterViewShowsArmedAndWarning(t *testing.T) {
	m := New("op", nil)
	m.width = 100
	m.killerArmed = false // config has kill_blockers disabled
	m.rosterOpen = true
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{
		{SPID: 104, Login: "APP01", Host: "SRV1", Count: 2, TotalMS: 20000},
	}}
	m.armed["login_name=APP01"] = true

	out := m.View()
	for _, want := range []string{"APP01", "armed", "kill_blockers disabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("roster view missing %q:\n%s", want, out)
		}
	}
}

func TestRosterViewEmpty(t *testing.T) {
	m := New("op", nil)
	m.width = 100
	m.killerArmed = true
	m.rosterOpen = true
	if out := m.View(); !strings.Contains(out, "no blockers yet") {
		t.Errorf("empty roster should say so:\n%s", out)
	}
}
