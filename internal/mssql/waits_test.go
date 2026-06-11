package mssql_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestCategorizeWaitsGroupsAndDropsNoise(t *testing.T) {
	waits := []mssql.SessionWait{
		{WaitType: "LCK_M_SCH_M", WaitTimeMS: 500, WaitingTasksCount: 2},
		{WaitType: "LCK_M_IX", WaitTimeMS: 100, WaitingTasksCount: 3},
		{WaitType: "PAGEIOLATCH_SH", WaitTimeMS: 300, WaitingTasksCount: 10},
		{WaitType: "WRITELOG", WaitTimeMS: 700, WaitingTasksCount: 50},
		{WaitType: "SOS_SCHEDULER_YIELD", WaitTimeMS: 40, SignalWaitTimeMS: 38, WaitingTasksCount: 9},
		{WaitType: "SLEEP_TASK", WaitTimeMS: 9999, WaitingTasksCount: 1}, // noise -> dropped
		{WaitType: "WAITFOR", WaitTimeMS: 9999, WaitingTasksCount: 1},    // noise -> dropped
	}

	cats, total := mssql.CategorizeWaits(waits)

	// Sorted by wait time desc: Transaction log (700), Locking (600), Data I/O (300), CPU (40).
	wantNames := []string{"Transaction log", "Locking", "Data I/O", "CPU & scheduling"}
	var gotNames []string
	for _, c := range cats {
		gotNames = append(gotNames, c.Name)
	}
	if diff := cmp.Diff(wantNames, gotNames); diff != "" {
		t.Errorf("category order mismatch (-want +got):\n%s", diff)
	}

	if total != 1640 {
		t.Errorf("total = %d, want 1640 (700+600+300+40, noise excluded)", total)
	}

	// Locking aggregates both LCK_M_* types.
	if cats[1].Name != "Locking" || cats[1].WaitTimeMS != 600 || cats[1].Tasks != 5 {
		t.Errorf("Locking = %+v, want WaitTimeMS=600 Tasks=5", cats[1])
	}
	// CPU category carries the signal portion.
	if cats[3].SignalMS != 38 {
		t.Errorf("CPU SignalMS = %d, want 38", cats[3].SignalMS)
	}
}

func TestCategorizeWaitsEmpty(t *testing.T) {
	cats, total := mssql.CategorizeWaits([]mssql.SessionWait{
		{WaitType: "XE_TIMER_EVENT", WaitTimeMS: 1000}, // all noise
	})
	if len(cats) != 0 || total != 0 {
		t.Errorf("CategorizeWaits(noise) = (%v, %d), want (empty, 0)", cats, total)
	}
}

func TestDiffWaits(t *testing.T) {
	before := []mssql.SessionWait{
		{WaitType: "WRITELOG", WaitTimeMS: 100, WaitingTasksCount: 5},
		{WaitType: "LCK_M_SCH_M", WaitTimeMS: 50, WaitingTasksCount: 1},
	}
	after := []mssql.SessionWait{
		{WaitType: "WRITELOG", WaitTimeMS: 400, WaitingTasksCount: 20},     // +300 / +15
		{WaitType: "LCK_M_SCH_M", WaitTimeMS: 50, WaitingTasksCount: 1},    // unchanged -> dropped
		{WaitType: "PAGEIOLATCH_EX", WaitTimeMS: 80, WaitingTasksCount: 4}, // new -> +80 / +4
	}

	delta := mssql.DiffWaits(before, after)

	got := map[string]mssql.SessionWait{}
	for _, d := range delta {
		got[d.WaitType] = d
	}
	if len(got) != 2 {
		t.Fatalf("delta has %d entries, want 2 (unchanged dropped)", len(got))
	}
	if w := got["WRITELOG"]; w.WaitTimeMS != 300 || w.WaitingTasksCount != 15 {
		t.Errorf("WRITELOG delta = %+v, want WaitTimeMS=300 Tasks=15", w)
	}
	if w := got["PAGEIOLATCH_EX"]; w.WaitTimeMS != 80 {
		t.Errorf("PAGEIOLATCH_EX delta = %+v, want WaitTimeMS=80", w)
	}
}
