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
	m := tui.New("rebuild_index dbo.T.IX", nil)
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

func TestModelShowsBlockedByAndSuspendsStatus(t *testing.T) {
	m := tui.New("shrink DataFile", nil)
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning})

	// Running and not blocked: RUNNING, no blocked line.
	if v := m.View(); !strings.Contains(v, "[RUNNING]") || strings.Contains(v, "BLOCKED by") {
		t.Fatalf("before block: want [RUNNING] and no blocked line\n%s", v)
	}

	// Blocked: status flips to SUSPENDED and the blocker is shown.
	m, _ = send(m, tui.BlockedByMsg{
		Blocked: true, SPID: 104, Login: "SVC_OBS", WaitType: "LCK_M_SCH_M",
		WaitMS: 120162, Query: "SELECT DL.SETTLEMENTDATE",
	})
	v := m.View()
	for _, want := range []string{"[SUSPENDED]", "BLOCKED by SPID 104", "LCK_M_SCH_M", "SVC_OBS", "SELECT DL.SETTLEMENTDATE"} {
		if !strings.Contains(v, want) {
			t.Errorf("while blocked: View() missing %q\n%s", want, v)
		}
	}
	if strings.Contains(v, "[RUNNING]") {
		t.Errorf("while blocked: status should not read RUNNING\n%s", v)
	}

	// Unblocked: reverts to RUNNING and the blocked line clears.
	m, _ = send(m, tui.BlockedByMsg{Blocked: false})
	if v := m.View(); !strings.Contains(v, "[RUNNING]") || strings.Contains(v, "BLOCKED by") {
		t.Errorf("after unblock: want [RUNNING] and no blocked line\n%s", v)
	}
}

func TestModelShowsSuspensionHistory(t *testing.T) {
	m := tui.New("shrink DataFile", nil)
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning})

	// No suspensions yet: no history line.
	if strings.Contains(m.View(), "suspended") {
		t.Fatalf("history line shown before any suspension\n%s", m.View())
	}

	// History persists even when not currently blocked.
	m, _ = send(m, tui.SuspensionMsg{
		Episodes: 4, TotalMS: 145000,
		Blockers: []tui.SuspensionBlocker{
			{SPID: 104, Login: "SVC_OBS", Count: 2, TotalMS: 110000},
			{SPID: 88, Login: "SVC_X", Count: 2, TotalMS: 35000},
		},
	})
	v := m.View()
	for _, want := range []string{"suspended 4×", "SPID 104 SVC_OBS (2×", "SPID 88 SVC_X (2×"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q\n%s", want, v)
		}
	}
}

func TestModelDrainStatusOutranksBlock(t *testing.T) {
	// A block substitutes SUSPENDED only while RUNNING; a draining operation keeps DRAINING.
	m := tui.New("shrink DataFile", nil)
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusDraining})
	m, _ = send(m, tui.BlockedByMsg{Blocked: true, SPID: 104, WaitType: "LCK_M_SCH_M"})
	v := m.View()
	if !strings.Contains(v, "[DRAINING]") || strings.Contains(v, "SUSPENDED") {
		t.Errorf("draining should outrank the block\n%s", v)
	}
	if !strings.Contains(v, "BLOCKED by SPID 104") {
		t.Errorf("the blocked line should still show while draining\n%s", v)
	}
}

func TestModelRendersWaits(t *testing.T) {
	m := tui.New("rebuild_index dbo.T.IX", nil)
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
	m := tui.New("(running)", nil)
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning, Operation: "rebuild_index dbo.T.IX", StepIndex: 12, StepTotal: 74})
	v := m.View()
	for _, want := range []string{"12/74", "rebuild_index dbo.T.IX"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q:\n%s", want, v)
		}
	}
}

func TestModelShowsBatchProgress(t *testing.T) {
	m := tui.New("batch_update dbo.Orders", nil)
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

func TestModelShowsSPID(t *testing.T) {
	m := tui.New("shrink_data all", nil)
	m, _ = send(m, tui.SPIDMsg{SPID: 102})
	if v := m.View(); !strings.Contains(v, "SPID 102") {
		t.Errorf("View() should show the monitored SPID:\n%s", v)
	}
}

func TestModelHumanizesWaitDurations(t *testing.T) {
	m := tui.New("op", nil)
	m, _ = send(m, tui.WaitsMsg{
		TotalMS: 775170, // 12m55s
		Categories: []tui.WaitCategory{
			{Name: "Data I/O", WaitMS: 395329, Tasks: 496501}, // 6m35s
			{Name: "Transaction log", WaitMS: 2785, Tasks: 1393}, // 2.8s
			{Name: "Page latch (tempdb)", WaitMS: 220, Tasks: 26}, // 220ms
		},
	})
	v := m.View()
	for _, want := range []string{"total 12m55s", "6m35s", "2.8s", "220ms"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() should humanize waits, missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "775170ms") || strings.Contains(v, "395329ms") {
		t.Errorf("raw millisecond values should not appear:\n%s", v)
	}
}

func TestModelShowsShrinkProgress(t *testing.T) {
	m := tui.New("shrink DataFile", nil)
	m, _ = send(m, tui.ShrinkMsg{
		File: "DataFile", Type: "data", CurrentMB: 6_000_000, StartMB: 8_388_608, FinalMB: 900_000,
		StepMB: 512, Percent: 0.32, Chunks: 128, ChunksRemaining: 140, ETASeconds: 2832,
	})
	v := m.View()
	// Sizes are humanized to GB/TB; chunk count, estimate and ETA appear on the detail line.
	for _, want := range []string{"shrink DataFile (data)", "5.72 TB", "878.9 GB target", "32%",
		"step 512 MB", "chunk 128 done", "~140 left", "ETA 47m12s"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "6000000") {
		t.Errorf("raw megabyte values should be humanized:\n%s", v)
	}
}

func TestHumanizeMBRendersReadableSizes(t *testing.T) {
	// exercised through the shrink render; assert the boundary units directly here.
	m := tui.New("op", nil)
	m, _ = send(m, tui.ShrinkMsg{File: "F", Type: "data", CurrentMB: 500, StartMB: 500, FinalMB: 100, StepMB: 50})
	if v := m.View(); !strings.Contains(v, "500 MB") {
		t.Errorf("sub-GB should stay in MB:\n%s", v)
	}
}

func TestModelResetsShrinkOnNewOperation(t *testing.T) {
	m := tui.New("shrink DataFile", nil)
	m, _ = send(m, tui.ShrinkMsg{File: "DataFile", CurrentMB: 6_000_000, StartMB: 8_388_608, FinalMB: 900_000, Percent: 0.32})
	// A new operation starts (StartedAt set): the stale shrink line must disappear.
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning, Operation: "rebuild_index dbo.T.IX",
		StepIndex: 2, StepTotal: 5, StartedAt: time.Unix(1000, 0)})
	if v := m.View(); strings.Contains(v, "shrink DataFile") {
		t.Errorf("shrink line should clear when a new operation starts:\n%s", v)
	}
}

func TestModelShowsAlertAndKeepsItSticky(t *testing.T) {
	m := tui.New("shrink DataFile", nil)
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
	m := tui.New("op", actions)
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

func TestModelKillTracksSPIDNotIndex(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", actions)
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 51}, {SPID: 52}, {SPID: 53}}})
	m, _ = send(m, key("down")) // select SPID 52 (index 1)
	// A poll drops SPID 51 and reorders: SPID 52 is now at index 0. The cursor must follow
	// the identity, so `x` still targets 52 — not whatever now sits at the old index 1.
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 52}, {SPID: 53}}})
	_, _ = send(m, key("x"))
	a, ok := drain(t, actions)
	if !ok || a.Kind != tui.ActionKillBlocker || a.SPID != 52 {
		t.Errorf("kill = %+v ok=%t, want KillBlocker SPID 52 (selection tracked by identity)", a, ok)
	}
}

func TestModelKillDDL(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", actions)
	send(m, key("k"))
	if a, ok := drain(t, actions); !ok || a.Kind != tui.ActionKillDDL {
		t.Errorf("k = %+v ok=%t, want KillDDL", a, ok)
	}
}

func TestModelDrainAction(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", actions)
	m, _ = send(m, key("d"))

	a, ok := drain(t, actions)
	if !ok || a.Kind != tui.ActionDrain {
		t.Errorf("d = %+v ok=%t, want ActionDrain", a, ok)
	}
	if v := m.View(); !strings.Contains(v, "DRAINING") {
		t.Errorf("View() should show DRAINING after 'd':\n%s", v)
	}
}

func TestModelQuit(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", actions)
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
	m := tui.New("op", actions)
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
	m := tui.New("op", actions)
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
	m := tui.New("op", make(chan tui.Action, 1))
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
	m := tui.New("op", nil)
	m, _ = send(m, tui.LogMsg{Line: "ignoring SPID 53 by app_name"})
	if v := m.View(); !strings.Contains(v, "ignoring SPID 53 by app_name") {
		t.Errorf("LogMsg should appear in the view:\n%s", v)
	}
}
