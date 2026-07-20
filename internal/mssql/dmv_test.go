package mssql_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
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
			s := mssql.Session{SPID: 52, BlockingSPID: tt.blockingSPID}
			if got := s.BlockedBy(tt.ddlSPID); got != tt.want {
				t.Errorf("BlockedBy(%d) with BlockingSPID=%d = %v, want %v", tt.ddlSPID, tt.blockingSPID, got, tt.want)
			}
		})
	}
}

func TestFindSelfBlock(t *testing.T) {
	// A snapshot: our DDL (119) is blocked by 104, which is running a SELECT; 134 is
	// blocked by us (irrelevant to self-block).
	blocker := mssql.Session{SPID: 104, Login: "SVC_OBS", Program: "app", ActiveQuery: "SELECT DL.SETTLEMENTDATE"}
	self := mssql.Session{SPID: 119, WaitType: "LCK_M_SCH_M", WaitMS: 120162, BlockingSPID: 104}
	victim := mssql.Session{SPID: 134, BlockingSPID: 119}
	snapshot := []mssql.Session{blocker, self, victim}

	t.Run("blocked with blocker in snapshot", func(t *testing.T) {
		sb := mssql.FindSelfBlock(snapshot, 119)
		if !sb.Blocked || sb.SPID != 104 || sb.WaitType != "LCK_M_SCH_M" || sb.WaitMS != 120162 {
			t.Fatalf("got %+v, want Blocked by 104 on LCK_M_SCH_M for 120162ms", sb)
		}
		if sb.Login != "SVC_OBS" || sb.Query != "SELECT DL.SETTLEMENTDATE" {
			t.Errorf("blocker identity = login %q query %q, want SVC_OBS / the SELECT", sb.Login, sb.Query)
		}
	})

	t.Run("blocker absent from snapshot still reports the block", func(t *testing.T) {
		sb := mssql.FindSelfBlock([]mssql.Session{self}, 119)
		if !sb.Blocked || sb.SPID != 104 || sb.WaitType != "LCK_M_SCH_M" {
			t.Fatalf("got %+v, want Blocked by 104 even without identity", sb)
		}
		if sb.Login != "" || sb.Query != "" {
			t.Errorf("identity should be empty when the blocker is absent, got login %q query %q", sb.Login, sb.Query)
		}
	})

	t.Run("idle-in-transaction blocker falls back to parent query", func(t *testing.T) {
		idle := mssql.Session{SPID: 104, Login: "SVC_OBS", ActiveQuery: "", ParentQuery: "UPDATE t SET x=1"}
		sb := mssql.FindSelfBlock([]mssql.Session{idle, self}, 119)
		if sb.Query != "UPDATE t SET x=1" {
			t.Errorf("Query = %q, want the parent batch when there is no active statement", sb.Query)
		}
	})

	t.Run("not blocked", func(t *testing.T) {
		running := mssql.Session{SPID: 119, BlockingSPID: 0}
		if sb := mssql.FindSelfBlock([]mssql.Session{running}, 119); sb.Blocked {
			t.Errorf("got %+v, want not blocked", sb)
		}
	})

	t.Run("our SPID unknown (0) is never blocked", func(t *testing.T) {
		if sb := mssql.FindSelfBlock(snapshot, 0); sb.Blocked {
			t.Errorf("got %+v, want not blocked for ddlSPID 0", sb)
		}
	})

	t.Run("our session absent from snapshot", func(t *testing.T) {
		if sb := mssql.FindSelfBlock([]mssql.Session{blocker, victim}, 119); sb.Blocked {
			t.Errorf("got %+v, want not blocked when our row is absent", sb)
		}
	})
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
			ls := mssql.LogSpace{TotalBytes: tt.totalBytes, UsedPercent: tt.usedPercent}
			if got := ls.UsedBytes(); got != tt.want {
				t.Errorf("UsedBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}
