package tui_test

import (
	"strings"
	"testing"

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
