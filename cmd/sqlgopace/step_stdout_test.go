package main

import (
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

func TestBlockerGateDebounces(t *testing.T) {
	gate := newBlockerGate()
	base := time.Unix(1000, 0)
	const timeout = 30 * time.Second
	blocked := []mssql.Session{{SPID: 52, BlockingSPID: 102}}

	// Freshly seen, then still within the window: hidden both times.
	if got := gate.persistent(blocked, 102, base, timeout); len(got) != 0 {
		t.Fatalf("a freshly-seen blocker must be hidden, got %+v", got)
	}
	if got := gate.persistent(blocked, 102, base.Add(10*time.Second), timeout); len(got) != 0 {
		t.Fatalf("a blocker below the timeout must stay hidden, got %+v", got)
	}
	// Past the timeout: shown.
	if got := gate.persistent(blocked, 102, base.Add(31*time.Second), timeout); len(got) != 1 || got[0].SPID != 52 {
		t.Fatalf("a blocker past the timeout must show, got %+v", got)
	}
	// It clears, then reappears: the debounce timer resets, so it is hidden again.
	if got := gate.persistent(nil, 102, base.Add(40*time.Second), timeout); len(got) != 0 {
		t.Fatalf("no sessions → nothing shown, got %+v", got)
	}
	if got := gate.persistent(blocked, 102, base.Add(41*time.Second), timeout); len(got) != 0 {
		t.Fatalf("a reappearing blocker restarts its debounce, got %+v", got)
	}
	// A session not blocked by us (bs_id 0) is never shown, even with no debounce.
	idle := []mssql.Session{{SPID: 60, BlockingSPID: 0}}
	if got := gate.persistent(idle, 102, base.Add(100*time.Second), 0); len(got) != 0 {
		t.Fatalf("an unblocked session must never show, got %+v", got)
	}
}

func TestStepSinkToFormatsStartedAndFinished(t *testing.T) {
	var b strings.Builder
	sink := stepSinkTo(&b)

	sink(run.StepEvent{Index: 12, Total: 74, Command: "rebuild_index", Target: "dbo.T.IX", Phase: run.StepStarted})
	sink(run.StepEvent{Index: 12, Total: 74, Command: "rebuild_index", Target: "dbo.T.IX",
		Phase: run.StepFinished, Outcome: "success", Duration: 3*time.Minute + 20*time.Second})

	got := b.String()
	want := "-- [12/74] rebuild_index dbo.T.IX — started\n" +
		"-- [12/74] rebuild_index dbo.T.IX — success in 3m20s\n"
	if got != want {
		t.Errorf("stepSinkTo output =\n%q\nwant\n%q", got, want)
	}
}

func TestStepStatusMsgMapsOnlyStarted(t *testing.T) {
	started := run.StepEvent{Index: 3, Total: 9, Command: "batch_update", Target: "dbo.Orders",
		Phase: run.StepStarted, StartedAt: time.Unix(100, 0)}
	msg, ok := stepStatusMsg(started)
	if !ok {
		t.Fatal("started event should map to a status message")
	}
	if msg.StepIndex != 3 || msg.StepTotal != 9 || msg.Operation != "batch_update dbo.Orders" || msg.StartedAt.IsZero() {
		t.Errorf("started map = %+v, want 3/9 batch_update dbo.Orders with StartedAt set", msg)
	}

	finished := run.StepEvent{Index: 3, Total: 9, Phase: run.StepFinished, Outcome: "success"}
	if _, ok := stepStatusMsg(finished); ok {
		t.Error("finished event should not map to a status message")
	}
}

func TestBatchMsgMapsProgress(t *testing.T) {
	msg := batchMsg(run.BatchDMLProgress{
		Verb: "update", Schema: "dbo", Table: "Orders",
		RowsDone: 1_200_000, EstRows: 5_000_000, BatchRows: 4000, RowsPerSec: 8500,
	})
	if msg.Table != "dbo.Orders" || msg.Verb != "update" || msg.BatchRows != 4000 || msg.RowsPerSec != 8500 {
		t.Errorf("batchMsg = %+v, want dbo.Orders update batch=4000 rate=8500", msg)
	}
	if got := msg.Percent; got < 0.23 || got > 0.25 {
		t.Errorf("Percent = %v, want ~0.24", got)
	}
}

func TestShrinkMsgMapsProgress(t *testing.T) {
	msg := shrinkMsg(run.ShrinkProgress{
		File: "DataFile", Type: "data", StartMB: 8_388_608, CurrentMB: 6_000_000, FinalMB: 900_000,
		StepMB: 512, Chunks: 10, ChunksRemaining: 20, ETASeconds: 300, AvgChunkSeconds: 30,
	})
	if msg.File != "DataFile" || msg.Type != "data" || msg.StartMB != 8_388_608 || msg.CurrentMB != 6_000_000 ||
		msg.FinalMB != 900_000 || msg.StepMB != 512 || msg.Chunks != 10 || msg.ChunksRemaining != 20 ||
		msg.ETASeconds != 300 || msg.AvgChunkSeconds != 30 {
		t.Errorf("shrinkMsg = %+v, want the fields mapped through including chunks/ETA/avg", msg)
	}
	// Percent is (start-current)/(start-final) = 2388608 / 7488608 ≈ 0.319.
	if got := msg.Percent; got < 0.31 || got > 0.33 {
		t.Errorf("Percent = %v, want ~0.32", got)
	}
}
