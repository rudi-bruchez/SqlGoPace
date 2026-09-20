package mssql

import (
	"database/sql"
	"testing"
)

func TestSessionBlockedBy(t *testing.T) {
	tests := []struct {
		name         string
		blockingSPID int
		ddlSPID      int
		want         bool
	}{
		{"blocked by our DDL", 102, 102, true},
		{"not blocked at all (bs_id 0) must never count", 0, 102, false},
		{"blocked by another session", 88, 102, false},
		{"our SPID unknown (0) must never match an idle session", 0, 0, false},
		{"our SPID unknown (0) must never match a blocked session", 88, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Session{SPID: 52, BlockingSPID: tt.blockingSPID}
			if got := s.BlockedBy(tt.ddlSPID); got != tt.want {
				t.Errorf("BlockedBy(%d) with BlockingSPID=%d = %v, want %v", tt.ddlSPID, tt.blockingSPID, got, tt.want)
			}
		})
	}
}

func TestFindSelfBlock(t *testing.T) {
	// A snapshot: our DDL (119) is blocked by 104, which is running a SELECT; 134 is
	// blocked by us (irrelevant to self-block).
	blocker := Session{SPID: 104, Login: "SVC_OBS", Program: "app", ActiveQuery: "SELECT DL.SETTLEMENTDATE"}
	self := Session{SPID: 119, WaitType: "LCK_M_SCH_M", WaitMS: 120162, BlockingSPID: 104}
	victim := Session{SPID: 134, BlockingSPID: 119}
	snapshot := []Session{blocker, self, victim}

	t.Run("blocked with blocker in snapshot", func(t *testing.T) {
		sb := FindSelfBlock(snapshot, 119)
		if !sb.Blocked || sb.SPID != 104 || sb.WaitType != "LCK_M_SCH_M" || sb.WaitMS != 120162 {
			t.Fatalf("got %+v, want Blocked by 104 on LCK_M_SCH_M for 120162ms", sb)
		}
		if sb.Login != "SVC_OBS" || sb.Query != "SELECT DL.SETTLEMENTDATE" {
			t.Errorf("blocker identity = login %q query %q, want SVC_OBS / the SELECT", sb.Login, sb.Query)
		}
	})

	t.Run("blocker absent from snapshot still reports the block", func(t *testing.T) {
		sb := FindSelfBlock([]Session{self}, 119)
		if !sb.Blocked || sb.SPID != 104 || sb.WaitType != "LCK_M_SCH_M" {
			t.Fatalf("got %+v, want Blocked by 104 even without identity", sb)
		}
		if sb.Login != "" || sb.Query != "" {
			t.Errorf("identity should be empty when the blocker is absent, got login %q query %q", sb.Login, sb.Query)
		}
	})

	t.Run("idle-in-transaction blocker falls back to parent query", func(t *testing.T) {
		idle := Session{SPID: 104, Login: "SVC_OBS", ActiveQuery: "", ParentQuery: "UPDATE t SET x=1"}
		sb := FindSelfBlock([]Session{idle, self}, 119)
		if sb.Query != "UPDATE t SET x=1" {
			t.Errorf("Query = %q, want the parent batch when there is no active statement", sb.Query)
		}
	})

	t.Run("not blocked", func(t *testing.T) {
		running := Session{SPID: 119, BlockingSPID: 0}
		if sb := FindSelfBlock([]Session{running}, 119); sb.Blocked {
			t.Errorf("got %+v, want not blocked", sb)
		}
	})

	t.Run("our SPID unknown (0) is never blocked", func(t *testing.T) {
		if sb := FindSelfBlock(snapshot, 0); sb.Blocked {
			t.Errorf("got %+v, want not blocked for ddlSPID 0", sb)
		}
	})

	t.Run("our session absent from snapshot", func(t *testing.T) {
		if sb := FindSelfBlock([]Session{blocker, victim}, 119); sb.Blocked {
			t.Errorf("got %+v, want not blocked when our row is absent", sb)
		}
	})
}

func TestFindSelfBlockCapturesHost(t *testing.T) {
	sessions := []Session{
		{SPID: 119, WaitType: "LCK_M_SCH_M", WaitMS: 5000, BlockingSPID: 104},
		{SPID: 104, Login: "app_login", Host: "APPSRV01", Program: "SQLCMD", Command: "ALTER INDEX"},
	}
	sb := FindSelfBlock(sessions, 119)
	if !sb.Blocked || sb.SPID != 104 {
		t.Fatalf("expected blocked by 104, got %+v", sb)
	}
	if sb.Host != "APPSRV01" {
		t.Errorf("Host = %q, want APPSRV01", sb.Host)
	}
	if sb.Command != "ALTER INDEX" {
		t.Errorf("SelfBlock.Command = %q, want ALTER INDEX", sb.Command)
	}
}

func TestLogSpaceUsedBytes(t *testing.T) {
	tests := []struct {
		name        string
		totalBytes  int64
		usedPercent float64
		want        int64
	}{
		{"half full", 100, 50, 50},
		{"empty", 1000, 0, 0},
		{"80 percent of 50GB", 50 << 30, 80, int64(float64(50<<30) * 0.8)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ls := LogSpace{TotalBytes: tt.totalBytes, UsedPercent: tt.usedPercent}
			if got := ls.UsedBytes(); got != tt.want {
				t.Errorf("UsedBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestScanLockedObjectNullNameFallsBackToID(t *testing.T) {
	// A dropped object resolves OBJECT_NAME/OBJECT_SCHEMA_NAME to NULL; keep the id.
	got := scanLockedObject(261575970, sql.NullString{}, sql.NullString{}, "Sch-M")
	want := LockedObject{ObjectID: 261575970, Schema: "", Table: "", Mode: "Sch-M"}
	if got != want {
		t.Errorf("scanLockedObject NULL name = %+v, want %+v", got, want)
	}

	got = scanLockedObject(42, sql.NullString{String: "dbo", Valid: true},
		sql.NullString{String: "MEASUREMENT", Valid: true}, "Sch-M")
	want = LockedObject{ObjectID: 42, Schema: "dbo", Table: "MEASUREMENT", Mode: "Sch-M"}
	if got != want {
		t.Errorf("scanLockedObject = %+v, want %+v", got, want)
	}
}

func TestActiveRequestCount(t *testing.T) {
	// ActiveSessions also returns sessions that are idle with a transaction open (they
	// matter as blockers); they are not requests running right now, so the load figure
	// must not count them.
	snapshot := []Session{
		{SPID: 51, Status: "running"},
		{SPID: 52, Status: "runnable"},
		{SPID: 53, Status: "suspended"},
		{SPID: 54, Status: "sleeping", OpenTransactions: 1},
	}
	if got := ActiveRequestCount(snapshot); got != 3 {
		t.Errorf("ActiveRequestCount = %d, want 3 (the sleeping session must not count)", got)
	}
	if got := ActiveRequestCount(nil); got != 0 {
		t.Errorf("ActiveRequestCount(nil) = %d, want 0", got)
	}
}

// TestProgressETAIsMeasuredFromElapsed pins the ETA to a figure the tool measures
// itself. sys.dm_exec_requests.estimated_completion_time is documented "Internal
// only", and the console showed "ETA: 5s" on a rebuild that was 72% done after 27
// minutes because it forwarded that column verbatim.
func TestProgressETAIsMeasuredFromElapsed(t *testing.T) {
	// 30% done after 60s: the remaining 70% needs 140s at the same rate.
	p := Progress{PercentComplete: 30, ElapsedMS: 60_000, EstimatedCompletionMS: 5_000}
	got, ok := p.ETASeconds()
	if !ok || got != 140 {
		t.Errorf("ETASeconds() = %d, %v; want 140, true (measured, not estimated_completion_time)", got, ok)
	}
}

// TestProgressETAUnknownBeforeFirstPercent: with no percentage there is no rate, so
// the ETA is unknown and must be reported as such rather than as a number.
func TestProgressETAUnknownBeforeFirstPercent(t *testing.T) {
	p := Progress{PercentComplete: 0, ElapsedMS: 60_000, EstimatedCompletionMS: 5_000}
	if got, ok := p.ETASeconds(); ok {
		t.Errorf("ETASeconds() = %d, true at 0%%; want unknown", got)
	}
}

// TestProgressETAUnknownDuringRollback: percent_complete is rollback progress then,
// so treating it as forward progress would announce a finish time for the wrong work.
func TestProgressETAUnknownDuringRollback(t *testing.T) {
	p := Progress{PercentComplete: 60, ElapsedMS: 60_000, Command: "KILLED/ROLLBACK"}
	if got, ok := p.ETASeconds(); ok {
		t.Errorf("ETASeconds() = %d, true during rollback; want unknown", got)
	}
}

// TestProgressETAUnknownWithoutElapsed: without elapsed time there is no rate. Both
// external reviewers noted that every other ETA test supplies positive elapsed time, so
// deleting the ElapsedMS guard would have gone unnoticed.
func TestProgressETAUnknownWithoutElapsed(t *testing.T) {
	p := Progress{PercentComplete: 50, ElapsedMS: 0, EstimatedCompletionMS: 5_000}
	if got, ok := p.ETASeconds(); ok {
		t.Errorf("ETASeconds() = %d, true with no elapsed time; want unknown", got)
	}
}
