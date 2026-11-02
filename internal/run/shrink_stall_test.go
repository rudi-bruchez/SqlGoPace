package run

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestClassifyNoGain(t *testing.T) {
	cases := []struct {
		stopped, backup bool
		want            noGainCause
	}{
		{true, true, causeYield},
		{true, false, causeYield},
		{false, true, causeBackup},
		{false, false, causeStall},
	}
	for _, c := range cases {
		if got := classifyNoGain(c.stopped, c.backup); got != c.want {
			t.Errorf("classifyNoGain(%v, %v) = %v, want %v", c.stopped, c.backup, got, c.want)
		}
	}
}

func TestYieldTallyDescribe(t *testing.T) {
	var y yieldTally
	y.add(yieldSnapshot{Blocked: []mssql.Session{{Login: "svc_loader", OpenTransactions: 2}}})
	y.add(yieldSnapshot{Blocked: []mssql.Session{{Login: "svc_report"}, {Login: "svc_loader"}}})
	y.add(yieldSnapshot{Self: mssql.SelfBlock{Blocked: true, SPID: 60, Login: "svc_writer"}})

	got := y.describe()
	for _, want := range []string{"yielded 3 times", "login=svc_loader", "open transaction", "session 60", "login=svc_writer"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe() = %q, missing %q", got, want)
		}
	}
	if y.count() != 3 {
		t.Errorf("count() = %d, want 3", y.count())
	}

	t.Run("tie breaks to the login seen first", func(t *testing.T) {
		var y yieldTally
		y.add(yieldSnapshot{Blocked: []mssql.Session{{Login: "first"}, {Login: "second"}}})
		if got := y.describe(); !strings.Contains(got, "login=first") {
			t.Errorf("describe() = %q, want login=first", got)
		}
	})

	y.reset()
	if y.describe() != "" || y.count() != 0 {
		t.Errorf("after reset: describe=%q count=%d", y.describe(), y.count())
	}
}

func TestYieldTallyEmptySnapshot(t *testing.T) {
	var y yieldTally
	y.add(yieldSnapshot{})
	got := y.describe()
	if !strings.Contains(got, "yielded 1 times") || strings.Contains(got, "login=") {
		t.Errorf("describe() = %q", got)
	}
}
