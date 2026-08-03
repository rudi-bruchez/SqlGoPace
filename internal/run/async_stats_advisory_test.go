package run

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestAsyncStatsAdvisory(t *testing.T) {
	reorg := ddl.ReorganizeIndex{Schema: "dbo", Table: "MEASUREMENT"}

	tests := []struct {
		name        string
		op          ddl.Operation
		setting     AsyncStatsSetting
		wantEmit    bool
		mustContain []string
		mustNotHave []string
	}{
		{
			name:     "absent on an older server emits nothing",
			op:       reorg,
			setting:  AsyncStatsAbsent,
			wantEmit: false,
		},
		{
			name:        "off emits the recommendation and the limitation",
			op:          reorg,
			setting:     AsyncStatsOff,
			wantEmit:    true,
			mustContain: []string{"is OFF", "ALTER DATABASE SCOPED CONFIGURATION", "does NOT cover", "PRODDB", "dbo.MEASUREMENT"},
		},
		{
			name:        "on emits the limitation alone",
			op:          reorg,
			setting:     AsyncStatsOn,
			wantEmit:    true,
			mustContain: []string{"does NOT cover"},
			mustNotHave: []string{"ALTER DATABASE SCOPED CONFIGURATION"},
		},
		{
			name:     "not a reorganize emits nothing",
			op:       ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT"},
			setting:  AsyncStatsOff,
			wantEmit: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, ok := asyncStatsAdvisory(tt.op, "PRODDB", tt.setting)
			if ok != tt.wantEmit {
				t.Fatalf("asyncStatsAdvisory() emit = %v, want %v (msg = %q)", ok, tt.wantEmit, msg)
			}
			if !ok {
				return
			}
			for _, want := range tt.mustContain {
				if !strings.Contains(msg, want) {
					t.Errorf("advisory %q does not contain %q", msg, want)
				}
			}
			for _, bad := range tt.mustNotHave {
				if strings.Contains(msg, bad) {
					t.Errorf("advisory %q unexpectedly contains %q", msg, bad)
				}
			}
		})
	}
}
