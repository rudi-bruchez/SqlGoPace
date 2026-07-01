package main

import (
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

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
