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
