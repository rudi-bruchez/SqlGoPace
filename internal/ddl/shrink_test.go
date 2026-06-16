package ddl_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestParseManifestShrink(t *testing.T) {
	const y = `
operations:
  - operation: shrink
    type: data
    files: MyDb_Data
    targetfreespace: 10%
    options:
      wait_at_low_priority: true
  - operation: shrink
    type: log
    targetfreespace: 100MB
`
	m, err := ddl.ParseManifest(strings.NewReader(y))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v, want nil", err)
	}
	if len(m.Operations) != 2 {
		t.Fatalf("got %d operations, want 2", len(m.Operations))
	}

	data, ok := m.Operations[0].(ddl.Shrink)
	if !ok {
		t.Fatalf("op 0 type = %T, want ddl.Shrink", m.Operations[0])
	}
	if data.CommandType() != "shrink_data" {
		t.Errorf("data CommandType = %q, want shrink_data", data.CommandType())
	}
	if got := data.Target().Name; got != "MyDb_Data" {
		t.Errorf("data Target().Name = %q, want MyDb_Data", got)
	}
	if data.Options.WaitAtLowPriority == nil || !*data.Options.WaitAtLowPriority {
		t.Errorf("data WaitAtLowPriority = %v, want true", data.Options.WaitAtLowPriority)
	}

	log, ok := m.Operations[1].(ddl.Shrink)
	if !ok {
		t.Fatalf("op 1 type = %T, want ddl.Shrink", m.Operations[1])
	}
	if log.CommandType() != "shrink_log" {
		t.Errorf("log CommandType = %q, want shrink_log", log.CommandType())
	}
	if got := log.FilesOrAll(); got != "all" {
		t.Errorf("log FilesOrAll() = %q, want all (default)", got)
	}
}

func TestParseTargetFreeSpace(t *testing.T) {
	pct := func(n int) ddl.TargetSpec { return ddl.TargetSpec{Percent: &n} }
	mb := func(n int) ddl.TargetSpec { return ddl.TargetSpec{AbsoluteMB: &n} }

	tests := []struct {
		in      string
		want    ddl.TargetSpec
		wantErr bool
	}{
		{"10%", pct(10), false},
		{"  25 % ", pct(25), false}, // surrounding and pre-% whitespace tolerated
		{"150%", pct(150), false},
		{"100MB", mb(100), false},
		{"100mb", mb(100), false},
		{"100 MB", mb(100), false},
		{"0%", ddl.TargetSpec{}, true},
		{"-5%", ddl.TargetSpec{}, true},
		{"", ddl.TargetSpec{}, true},
		{"100", ddl.TargetSpec{}, true},  // no unit
		{"10GB", ddl.TargetSpec{}, true}, // unsupported unit
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ddl.ParseTargetFreeSpace(tt.in)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("ParseTargetFreeSpace(%q) error = %v, want error presence = %t", tt.in, err, tt.wantErr)
			}
			if tt.wantErr {
				if !errors.Is(err, ddl.ErrInvalidManifest) {
					t.Errorf("ParseTargetFreeSpace(%q) error = %v, want errors.Is ErrInvalidManifest", tt.in, err)
				}
				return
			}
			if !specEqual(got, tt.want) {
				t.Errorf("ParseTargetFreeSpace(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func specEqual(a, b ddl.TargetSpec) bool {
	eq := func(x, y *int) bool {
		switch {
		case x == nil && y == nil:
			return true
		case x == nil || y == nil:
			return false
		default:
			return *x == *y
		}
	}
	return eq(a.Percent, b.Percent) && eq(a.AbsoluteMB, b.AbsoluteMB)
}

func TestFinalTargetMB(t *testing.T) {
	pct := func(n int) ddl.TargetSpec { return ddl.TargetSpec{Percent: &n} }
	mb := func(n int) ddl.TargetSpec { return ddl.TargetSpec{AbsoluteMB: &n} }

	tests := []struct {
		name string
		used int
		spec ddl.TargetSpec
		want int
	}{
		{"10 percent of 1000", 1000, pct(10), 1100},
		{"10 percent rounds up", 1001, pct(10), 1102}, // ceil(1001*1.1)=ceil(1101.1)=1102
		{"absolute adds mb", 1000, mb(250), 1250},
		{"percent of zero used", 0, pct(50), 0},
		{"empty spec clamps to used", 800, ddl.TargetSpec{}, 800},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ddl.FinalTargetMB(tt.used, tt.spec); got != tt.want {
				t.Errorf("FinalTargetMB(%d, %+v) = %d, want %d", tt.used, tt.spec, got, tt.want)
			}
		})
	}
}
