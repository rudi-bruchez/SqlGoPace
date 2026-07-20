package tui_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rudi-bruchez/SqlGoPace/internal/tui"
)

func key(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func send(m tui.Model, msg tea.Msg) (tui.Model, tea.Cmd) {
	updated, cmd := m.Update(msg)
	return updated.(tui.Model), cmd
}

func drain(t *testing.T, actions <-chan tui.Action) (tui.Action, bool) {
	t.Helper()
	select {
	case a := <-actions:
		return a, true
	default:
		return tui.Action{}, false
	}
}

func TestModelUpdatesFromMessages(t *testing.T) {
	m := tui.New("rebuild_index dbo.T.IX", true, nil)
	m, _ = send(m, tui.ProgressMsg{Percent: 42, ETASeconds: 120})
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{
		{SPID: 58, Login: "app", Host: "web01", WaitType: "LCK_M_SCH_M", WaitMS: 4500},
	}})

	view := m.View()
	for _, want := range []string{"rebuild_index dbo.T.IX", "42%", "SPID 58", "LCK_M_SCH_M"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() missing %q\n%s", want, view)
		}
	}
}

func TestModelRendersWaits(t *testing.T) {
	m := tui.New("rebuild_index dbo.T.IX", true, nil)
	m, _ = send(m, tui.WaitsMsg{
		TotalMS: 800,
		Categories: []tui.WaitCategory{
			{Name: "Transaction log", WaitMS: 600, Tasks: 45},
			{Name: "Locking", WaitMS: 200, Tasks: 2},
		},
	})

	view := m.View()
	for _, want := range []string{"waits slowing the DDL (total 800ms)", "Transaction log", "Locking", "600ms"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() missing %q\n%s", want, view)
		}
	}
}

func TestModelShowsStepCounter(t *testing.T) {
	m := tui.New("(running)", false, nil)
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning, Operation: "rebuild_index dbo.T.IX", StepIndex: 12, StepTotal: 74})
	v := m.View()
	for _, want := range []string{"12/74", "rebuild_index dbo.T.IX"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q:\n%s", want, v)
		}
	}
}

func TestModelShowsBatchProgress(t *testing.T) {
	m := tui.New("batch_update dbo.Orders", false, nil)
	m, _ = send(m, tui.BatchMsg{
		Verb: "update", Table: "dbo.Orders",
		RowsDone: 1_200_000, EstRows: 5_000_000, Percent: 0.24, BatchRows: 4000, RowsPerSec: 8500,
	})
	v := m.View()
	for _, want := range []string{"batch update dbo.Orders", "24%", "batch=4000", "rows/s"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q:\n%s", want, v)
		}
	}
}

func TestModelShowsShrinkProgress(t *testing.T) {
	m := tui.New("shrink DataFile", false, nil)
	m, _ = send(m, tui.ShrinkMsg{
		File: "DataFile", CurrentMB: 6_000_000, StartMB: 8_388_608, FinalMB: 900_000, Percent: 0.32,
	})
	v := m.View()
	for _, want := range []string{"shrink DataFile", "6000000", "900000 MB target", "32%"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q:\n%s", want, v)
		}
	}
}

func TestModelResetsShrinkOnNewOperation(t *testing.T) {
	m := tui.New("shrink DataFile", false, nil)
	m, _ = send(m, tui.ShrinkMsg{File: "DataFile", CurrentMB: 6_000_000, StartMB: 8_388_608, FinalMB: 900_000, Percent: 0.32})
	// A new operation starts (StartedAt set): the stale shrink line must disappear.
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning, Operation: "rebuild_index dbo.T.IX",
		StepIndex: 2, StepTotal: 5, StartedAt: time.Unix(1000, 0)})
	if v := m.View(); strings.Contains(v, "shrink DataFile") {
		t.Errorf("shrink line should clear when a new operation starts:\n%s", v)
	}
}

func TestModelShowsAlertAndKeepsItSticky(t *testing.T) {
	m := tui.New("shrink DataFile", false, nil)
	m, _ = send(m, tui.AlertMsg{
		Title: "manifest failed: 020_shrink.yaml — preflight failed",
		Lines: []string{"permissions: shrink/check_db require db_owner (in this database) or sysadmin"},
	})
	// A later, unrelated update must not clear the alert (it is sticky, unlike LogMsg).
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 9}}})

	v := m.View()
	for _, want := range []string{"manifest failed: 020_shrink.yaml", "db_owner", "⚠"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing alert %q:\n%s", want, v)
		}
	}
}

func TestModelKillBlockerAction(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", true, actions)
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 58}, {SPID: 61}}})
	_, _ = send(m, key("x")) // kill selected (first) blocker; model not needed after

	a, ok := drain(t, actions)
	if !ok {
		t.Fatalf("no action emitted")
	}
	if a.Kind != tui.ActionKillBlocker || a.SPID != 58 {
		t.Errorf("action = %+v, want KillBlocker SPID 58", a)
	}
}

func TestModelKillDDLAndPause(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", true, actions)

	if _, _ = send(m, key("k")); true {
		if a, ok := drain(t, actions); !ok || a.Kind != tui.ActionKillDDL {
			t.Errorf("k = %+v ok=%t, want KillDDL", a, ok)
		}
	}
	if _, _ = send(m, key("p")); true {
		if a, ok := drain(t, actions); !ok || a.Kind != tui.ActionPause {
			t.Errorf("p = %+v ok=%t, want Pause", a, ok)
		}
	}
}

func TestModelDrainAction(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", true, actions)
	m, _ = send(m, key("d"))

	a, ok := drain(t, actions)
	if !ok || a.Kind != tui.ActionDrain {
		t.Errorf("d = %+v ok=%t, want ActionDrain", a, ok)
	}
	if v := m.View(); !strings.Contains(v, "DRAINING") {
		t.Errorf("View() should show DRAINING after 'd':\n%s", v)
	}
}

func TestModelPauseIgnoredWhenNotResumable(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", false, actions) // not resumable
	send(m, key("p"))
	if a, ok := drain(t, actions); ok {
		t.Errorf("pause emitted %+v on a non-resumable op, want none", a)
	}
}

func TestModelQuit(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", true, actions)
	_, cmd := send(m, key("q"))
	if cmd == nil {
		t.Errorf("q should return a quit command")
	}
	if a, ok := drain(t, actions); !ok || a.Kind != tui.ActionQuit {
		t.Errorf("q action = %+v ok=%t, want Quit", a, ok)
	}
}

func TestModelIgnoreFlowEmitsAction(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", false, actions)
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{
		{SPID: 53, Program: "ReportingService", Login: "svc", Host: "BATCH01"},
	}})
	m, _ = send(m, key("i")) // open the ignore-criterion prompt
	_, _ = send(m, key("a")) // ignore by app_name

	a, ok := drain(t, actions)
	if !ok {
		t.Fatal("no action emitted")
	}
	if a.Kind != tui.ActionIgnoreBlocker || a.Criterion != "app_name" || a.Value != "ReportingService" || a.SPID != 53 {
		t.Errorf("emitted %+v, want app_name ignore for SPID 53 ReportingService", a)
	}
}

func TestModelIgnorePromptCancel(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", false, actions)
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 53, Program: "X"}}})
	m, _ = send(m, key("i"))
	m, _ = send(m, tea.KeyMsg{Type: tea.KeyEsc}) // cancel the prompt

	if a, ok := drain(t, actions); ok {
		t.Errorf("cancel should emit nothing, got %+v", a)
	}
	if v := m.View(); !strings.Contains(v, "[i] ignore") {
		t.Errorf("after cancel the normal help (with [i] ignore) should show:\n%s", v)
	}
}

func TestModelIgnorePromptShowsCriteria(t *testing.T) {
	m := tui.New("op", false, make(chan tui.Action, 1))
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 7, Program: "Rep", Login: "svc", Host: "h1"}}})
	m, _ = send(m, key("i"))
	v := m.View()
	for _, want := range []string{"ignore SPID 7 as:", "[a] app=Rep", "[l] login=svc", "[h] host=h1"} {
		if !strings.Contains(v, want) {
			t.Errorf("criterion prompt missing %q:\n%s", want, v)
		}
	}
}

func TestModelLogMsgShownAsNotice(t *testing.T) {
	m := tui.New("op", false, nil)
	m, _ = send(m, tui.LogMsg{Line: "ignoring SPID 53 by app_name"})
	if v := m.View(); !strings.Contains(v, "ignoring SPID 53 by app_name") {
		t.Errorf("LogMsg should appear in the view:\n%s", v)
	}
}
