package tui_test

import (
	"fmt"
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

func TestModelOperationsPanel(t *testing.T) {
	m := tui.New("(running)", nil)
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = send(m, tui.OperationsMsg{Ops: []tui.OperationRow{
		{Index: 1, Label: "shrink_data all", Status: "TO RUN"},
		{Index: 2, Label: "rebuild_index dbo.T.IX", Status: "TO RUN"},
	}})

	// Op 1 starts, then finishes; op 2 starts.
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning, StepIndex: 1, StepTotal: 2})
	if v := m.View(); !strings.Contains(v, "1 - shrink_data all") || !strings.Contains(v, "[RUNNING]") {
		t.Fatalf("op 1 should read RUNNING:\n%s", v)
	}
	m, _ = send(m, tui.StepDoneMsg{Index: 1, Outcome: "success"})
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning, StepIndex: 2, StepTotal: 2})
	v := m.View()
	for _, want := range []string{"1 - shrink_data all", "[DONE]", "2 - rebuild_index dbo.T.IX", "[RUNNING]"} {
		if !strings.Contains(v, want) {
			t.Errorf("operations panel missing %q:\n%s", want, v)
		}
	}

	// The running op reads SUSPENDED while we are a victim.
	m, _ = send(m, tui.BlockedByMsg{Blocked: true, SPID: 104})
	if v := m.View(); !strings.Contains(v, "[SUSPENDED]") {
		t.Errorf("running op should read SUSPENDED while blocked:\n%s", v)
	}

	// A failed op shows FAILED.
	m, _ = send(m, tui.StepDoneMsg{Index: 2, Outcome: "failed"})
	if v := m.View(); !strings.Contains(v, "[FAILED]") {
		t.Errorf("op 2 should read FAILED:\n%s", v)
	}
}

func TestModelOperationsRowShowsDraining(t *testing.T) {
	// Regression: once the operations panel is populated, the running row must still reflect
	// the lifecycle status (DRAINING/etc.), not stay stuck on [RUNNING].
	m := tui.New("(running)", nil)
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = send(m, tui.OperationsMsg{Ops: []tui.OperationRow{{Index: 1, Label: "shrink_data all", Status: "TO RUN"}}})
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning, StepIndex: 1, StepTotal: 1})
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusDraining}) // operator pressed 'd'
	if v := m.View(); !strings.Contains(v, "[DRAINING]") {
		t.Errorf("running row should read DRAINING once the drain is requested:\n%s", v)
	}
}

func TestModelResetsBlockIndicatorOnNewOp(t *testing.T) {
	// Regression: a new operation must not inherit the previous op's block indicator.
	m := tui.New("(running)", nil)
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning, StepIndex: 1, StepTotal: 2, StartedAt: time.Unix(1000, 0)})
	m, _ = send(m, tui.BlockedByMsg{Blocked: true, SPID: 104, WaitType: "LCK_M_SCH_M"})
	if v := m.View(); !strings.Contains(v, "BLOCKED by SPID 104") {
		t.Fatalf("op 1 should show the block:\n%s", v)
	}
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning, StepIndex: 2, StepTotal: 2, StartedAt: time.Unix(1100, 0)})
	if v := m.View(); strings.Contains(v, "BLOCKED by SPID 104") {
		t.Errorf("op 2 must not inherit op 1's stale block indicator:\n%s", v)
	}
}

func TestModelExpandSQLResetsWhenBlockerGone(t *testing.T) {
	const sql = "SELECT * FROM dbo.BIG WHERE REGIONID = 5 AND STATUS = 'ACTIVE_LONG_TAIL'"
	m := tui.New("op", nil)
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 104, Login: "SVC", Query: sql}}})
	m, _ = send(m, key("enter"))
	if v := m.View(); !strings.Contains(v, "ACTIVE_LONG_TAIL") {
		t.Fatalf("SPID 104 SQL should be expanded:\n%s", v)
	}
	// A poll where 104 is gone, replaced by a different session: the expand must reset so the
	// new session's full SQL is not auto-shown.
	const other = "UPDATE dbo.OTHER SET X = 1 WHERE Y = 2 AND Z = 'DIFFERENT_TAIL_MARKER'"
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 200, Login: "SVC2", Query: other}}})
	if v := m.View(); strings.Contains(v, "DIFFERENT_TAIL_MARKER") {
		t.Errorf("expand must reset when the pinned blocker disappears; SPID 200 full SQL leaked:\n%s", v)
	}
}

func TestModelOperationsWindowsToFitViewport(t *testing.T) {
	// A manifest expanded to many ops (index: ALL) must not push the lower panels off the
	// fixed alt-screen: the operations panel windows around the running op with a summary.
	const rows = 40
	m := tui.New("(running)", nil)
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: rows})
	ops := make([]tui.OperationRow, 60)
	for i := range ops {
		ops[i] = tui.OperationRow{Index: i + 1, Label: fmt.Sprintf("rebuild_index dbo.T.IX%03d", i+1), Status: "TO RUN"}
	}
	m, _ = send(m, tui.OperationsMsg{Ops: ops})
	m, _ = send(m, tui.StatusMsg{Status: tui.StatusRunning, StepIndex: 30, StepTotal: 60})
	for i := 1; i <= 29; i++ {
		m, _ = send(m, tui.StepDoneMsg{Index: i, Outcome: "success"})
	}
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 58, Login: "app"}}})
	m, _ = send(m, tui.WaitsMsg{TotalMS: 1000, Categories: []tui.WaitCategory{{Name: "Locking", WaitMS: 1000, Tasks: 3}}})
	v := m.View()

	// The whole dashboard fits the viewport — the lower, actionable panels are not clipped.
	if h := strings.Count(v, "\n") + 1; h > rows {
		t.Fatalf("view is %d lines, exceeds the %d-row terminal (lower panels clip):\n%s", h, rows, v)
	}
	for _, want := range []string{"30 - rebuild_index dbo.T.IX030", "60 ops", "29 done", "blocked sessions", "waits slowing the DDL", "[q] quit"} {
		if !strings.Contains(v, want) {
			t.Errorf("windowed view missing %q:\n%s", want, v)
		}
	}
	// Ops far from the running one are windowed out.
	for _, gone := range []string{"IX001", "IX060"} {
		if strings.Contains(v, gone) {
			t.Errorf("op %q should be windowed out of the operations panel:\n%s", gone, v)
		}
	}
}

func TestModelServerBanner(t *testing.T) {
	m := tui.New("(running)", nil)
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = send(m, tui.ServerInfoMsg{
		App: "0.3.0", Name: "SQLPROD01", Product: "SQL Server 2022", Database: "PRODDB",
		Edition: "standard", Recovery: "SIMPLE", ADR: false, RCSI: false, SnapshotIso: false,
	})
	v := m.View()
	for _, want := range []string{"SqlGoPace", "0.3.0", "SQLPROD01", "SQL Server 2022", "PRODDB", "ed=standard", "recovery=SIMPLE", "adr=f", "rcsi=f", "si=f"} {
		if !strings.Contains(v, want) {
			t.Errorf("server banner missing %q:\n%s", want, v)
		}
	}
}

func TestModelExpandSQL(t *testing.T) {
	const sql = "SELECT DL.SETTLEMENTDATE, V.VERSION FROM dbo.MEASUREMENT DL JOIN dbo.VERSIONS V ON V.ID = DL.VID WHERE DL.REGIONID = 5"
	m := tui.New("op", nil)
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 104, Login: "SVC_OBS", Query: sql}}})

	// Collapsed: the query is truncated (ends with the ellipsis, full text absent).
	if v := m.View(); strings.Contains(v, sql) {
		t.Fatalf("collapsed: full SQL should be truncated, not shown in full:\n%s", v)
	}
	// Enter expands: the full SQL becomes visible.
	m, _ = send(m, key("enter"))
	if v := m.View(); !strings.Contains(v, "SETTLEMENTDATE") || !strings.Contains(v, "REGIONID = 5") {
		t.Errorf("expanded: full SQL should be shown:\n%s", v)
	}
	// Enter again collapses.
	m, _ = send(m, key("enter"))
	if v := m.View(); strings.Contains(v, "REGIONID = 5") {
		t.Errorf("collapsed again: full SQL tail should be hidden:\n%s", v)
	}
}

func TestModelResponsiveStacksNarrow(t *testing.T) {
	m := tui.New("op", nil)
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{{SPID: 58}}})
	m, _ = send(m, tui.WaitsMsg{TotalMS: 1000, Categories: []tui.WaitCategory{{Name: "Locking", WaitMS: 1000, Tasks: 3}}})

	// Both a wide and a narrow terminal must render both panels (side by side vs stacked).
	for _, w := range []int{140, 50} {
		m, _ = send(m, tea.WindowSizeMsg{Width: w, Height: 40})
		v := m.View()
		if !strings.Contains(v, "blocked sessions") || !strings.Contains(v, "waits slowing the DDL") {
			t.Errorf("width %d: both panels should render:\n%s", w, v)
		}
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

func TestModelKillBlockerAutoAction(t *testing.T) {
	actions := make(chan tui.Action, 4)
	m := tui.New("op", actions)
	m, _ = send(m, tui.BlockersMsg{Blockers: []tui.Blocker{
		{SPID: 104, Login: "SVC_RPT", Host: "rpt01", Program: "Reporting"},
	}})
	m, _ = send(m, key("X")) // open the kill+remember criterion prompt
	// The prompt should ask which criterion to match on.
	if !strings.Contains(m.View(), "kill+auto-kill SPID 104") {
		t.Fatalf("expected kill criterion prompt, view:\n%s", m.View())
	}
	_, _ = send(m, key("l")) // remember by login
	a, ok := drain(t, actions)
	if !ok {
		t.Fatalf("no action emitted")
	}
	if a.Kind != tui.ActionKillBlockerAuto || a.SPID != 104 || a.Criterion != "login_name" || a.Value != "SVC_RPT" {
		t.Errorf("action = %+v, want KillBlockerAuto SPID 104 login_name=SVC_RPT", a)
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
