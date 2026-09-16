package main

import (
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
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

func TestStepDoneMsgCarriesTheOperationDuration(t *testing.T) {
	// The engine already measures each operation; the console row shows that total, so the
	// forwarder must not drop it.
	msg := stepDoneMsg(run.StepEvent{
		Index: 3, Total: 9, Phase: run.StepFinished, Outcome: "success",
		Detail: "8.1 GB -> 5.4 GB (-33.3%)", Duration: 94 * time.Minute,
	})
	if msg.Index != 3 || msg.Outcome != "success" || msg.Detail != "8.1 GB -> 5.4 GB (-33.3%)" {
		t.Errorf("stepDoneMsg = %+v, want index/outcome/detail mapped through", msg)
	}
	if msg.Duration != 94*time.Minute {
		t.Errorf("Duration = %v, want 94m", msg.Duration)
	}
}

func TestOperationsMsgNamesTheManifest(t *testing.T) {
	// The console titles its operations panel with the manifest name, so the forwarder has to
	// carry it alongside the rows it builds.
	msg := operationsMsg("030_compress_indexes.yaml", []run.OpInfo{
		{Index: 1, Command: "rebuild_index", Target: "dbo.T.IX", Detail: "cancel only"},
		{Index: 2, Command: "shrink_data", Target: "all"},
	})
	if msg.Manifest != "030_compress_indexes.yaml" {
		t.Errorf("Manifest = %q, want the manifest file name", msg.Manifest)
	}
	if len(msg.Ops) != 2 {
		t.Fatalf("Ops = %d rows, want 2", len(msg.Ops))
	}
	if msg.Ops[0].Label != "rebuild_index dbo.T.IX" || msg.Ops[0].Status != "TO RUN" || msg.Ops[0].Detail != "cancel only" {
		t.Errorf("Ops[0] = %+v, want the label, TO RUN and the manifest-start detail", msg.Ops[0])
	}
}

func TestBlockersSurfaceOnSight(t *testing.T) {
	// BLOCKER-VISIBILITY.md: a blocker is shown on the poll that sees it. The minute-long
	// debounce this replaces hid the chain an operator was working, and reset itself after
	// every kill, so each kill bought another minute of blindness.
	blocked := []mssql.Session{{SPID: 52, BlockingSPID: 102}}
	got := blockersOf(blocked, 102)
	if len(got) != 1 || got[0].SPID != 52 {
		t.Fatalf("a freshly-seen blocker must be shown at once, got %+v", got)
	}
	// A session blocked by somebody else, or by nobody, is never ours to show.
	mixed := []mssql.Session{{SPID: 60, BlockingSPID: 0}, {SPID: 61, BlockingSPID: 999}, {SPID: 62, BlockingSPID: 102}}
	got = blockersOf(mixed, 102)
	if len(got) != 1 || got[0].SPID != 62 {
		t.Fatalf("only sessions blocked by our SPID may show, got %+v", got)
	}
	if got := blockersOf(nil, 102); len(got) != 0 {
		t.Fatalf("no sessions -> nothing shown, got %+v", got)
	}
}
