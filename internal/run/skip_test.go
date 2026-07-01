package run

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestCompressionSatisfied(t *testing.T) {
	parts := func(descs ...string) []mssql.PartitionCompression {
		var out []mssql.PartitionCompression
		for i, d := range descs {
			out = append(out, mssql.PartitionCompression{Partition: i + 1, Desc: d})
		}
		return out
	}
	part := func(n int) *int { return &n }

	tests := []struct {
		name      string
		parts     []mssql.PartitionCompression
		target    string
		partition *int
		want      bool
	}{
		{"whole index all at target", parts("PAGE", "PAGE"), "PAGE", nil, true},
		{"whole index one off target", parts("PAGE", "NONE"), "PAGE", nil, false},
		{"case-insensitive desc vs keyword", parts("page"), "PAGE", nil, true},
		{"empty target never skips", parts("PAGE"), "", nil, false},
		{"empty read never skips", nil, "PAGE", nil, false},
		{"targeted partition matches", parts("NONE", "PAGE"), "PAGE", part(2), true},
		{"targeted partition off target", parts("PAGE", "NONE"), "PAGE", part(2), false},
		{"targeted partition not found", parts("PAGE"), "PAGE", part(5), false},
	}
	for _, tt := range tests {
		if got := compressionSatisfied(tt.parts, tt.target, tt.partition); got != tt.want {
			t.Errorf("%s: compressionSatisfied() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
