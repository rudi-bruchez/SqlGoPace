package mssql_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestServerInfoDerivation(t *testing.T) {
	tests := []struct {
		name          string
		engineEdition int
		major         int
		wantTier      ddl.Tier
		wantSupported bool
	}{
		{"enterprise 2022", 3, 16, ddl.TierEnterprise, true},
		{"standard 2019", 2, 15, ddl.TierStandard, true},
		{"azure sql db", 5, 0, ddl.TierAzure, true},
		{"managed instance", 8, 0, ddl.TierAzure, true},
		{"synapse unsupported", 6, 0, ddl.TierUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := mssql.ServerInfo{EngineEdition: tt.engineEdition, MajorVersion: tt.major}

			if got := info.Tier(); got != tt.wantTier {
				t.Errorf("Tier() = %v, want %v", got, tt.wantTier)
			}
			if got := info.Supported(); got != tt.wantSupported {
				t.Errorf("Supported() = %t, want %t", got, tt.wantSupported)
			}
			want := ddl.Target{MajorVersion: tt.major, Tier: tt.wantTier}
			if got := info.Target(); got != want {
				t.Errorf("Target() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestContextInfoLiteral(t *testing.T) {
	marker := [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	got := mssql.ContextInfoLiteral(marker)
	want := "0x0123456789abcdeffedcba9876543210"
	if got != want {
		t.Errorf("ContextInfoLiteral() = %q, want %q", got, want)
	}
}
