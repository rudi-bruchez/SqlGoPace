package mssql_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

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
