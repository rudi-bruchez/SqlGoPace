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
