package run

import (
	"testing"
)

func TestTailWalkPages(t *testing.T) {
	tests := []struct {
		name   string
		freeMB int
		want   int
	}{
		{"small free space uses free pages plus margin", 10, 10*128 + 512},
		{"zero free still allows the margin", 0, 512},
		{"negative free clamps to the margin floor", -5, 512},
		{"huge free space clamps to the absolute cap", 1_000_000, 262144},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tailWalkPages(tt.freeMB); got != tt.want {
				t.Errorf("tailWalkPages(%d) = %d, want %d", tt.freeMB, got, tt.want)
			}
		})
	}
}
