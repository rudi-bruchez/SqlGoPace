package ddl_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestTierFromEngineEdition(t *testing.T) {
	tests := []struct {
		engineEdition int
		want          ddl.Tier
	}{
		{2, ddl.TierStandard},
		{3, ddl.TierEnterprise},
		{4, ddl.TierExpress},
		{5, ddl.TierAzure},
		{8, ddl.TierAzure},
		{6, ddl.TierUnknown},
		{99, ddl.TierUnknown},
	}
	for _, tt := range tests {
		if got := ddl.TierFromEngineEdition(tt.engineEdition); got != tt.want {
			t.Errorf("TierFromEngineEdition(%d) = %v, want %v", tt.engineEdition, got, tt.want)
		}
	}
}

func TestParseTier(t *testing.T) {
	tests := []struct {
		in      string
		want    ddl.Tier
		wantErr bool
	}{
		{"enterprise", ddl.TierEnterprise, false},
		{"standard", ddl.TierStandard, false},
		{"express", ddl.TierExpress, false},
		{"azure", ddl.TierAzure, false},
		{"ENTERPRISE", ddl.TierEnterprise, false}, // case-insensitive
		{"  azure  ", ddl.TierAzure, false},       // trimmed
		{"bogus", ddl.TierUnknown, true},
		{"", ddl.TierUnknown, true},
	}
	for _, tt := range tests {
		got, err := ddl.ParseTier(tt.in)
		if gotErr := err != nil; gotErr != tt.wantErr {
			t.Errorf("ParseTier(%q) error = %v, want error presence = %t", tt.in, err, tt.wantErr)
			continue
		}
		if tt.wantErr {
			if !errors.Is(err, ddl.ErrUnknownTier) {
				t.Errorf("ParseTier(%q) error = %v, want errors.Is ErrUnknownTier", tt.in, err)
			}
			continue
		}
		if got != tt.want {
			t.Errorf("ParseTier(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// testMatrix builds a small in-memory matrix decoupled from any YAML file, so
// the resolution logic is tested in isolation.
func testMatrix() *ddl.Matrix {
	ent := []ddl.Tier{ddl.TierEnterprise, ddl.TierAzure}
	return &ddl.Matrix{
		AzurePseudoMajor: 9999,
		MajorToYear:      map[int]int{16: 2022},
		Commands: map[string]ddl.CommandRules{
			"rebuild_index": {
				"online":    {MinMajor: 9, Editions: ent},
				"resumable": {MinMajor: 14, Editions: ent, Requires: []string{"online"}},
			},
			"create_index": {
				"wait_at_low_priority": {MinMajor: 16, Editions: ent, Requires: []string{"online"}},
			},
			"shrink_data": {
				// Unlike online-index WALP, shrink WALP is not Enterprise-gated and has no
				// `requires: [online]` (DBCC has no ONLINE clause).
				"wait_at_low_priority": {MinMajor: 16, Editions: []ddl.Tier{
					ddl.TierEnterprise, ddl.TierStandard, ddl.TierExpress, ddl.TierAzure,
				}},
			},
			"shrink_log": {},
			"drop_index": {},
		},
	}
}

func TestMatrixApplicable(t *testing.T) {
	m := testMatrix()
	tests := []struct {
		name    string
		major   int
		tier    ddl.Tier
		command string
		option  string
		want    bool
	}{
		{"online enterprise ok", 16, ddl.TierEnterprise, "rebuild_index", "online", true},
		{"online standard blocked by edition", 16, ddl.TierStandard, "rebuild_index", "online", false},
		{"online express blocked", 16, ddl.TierExpress, "rebuild_index", "online", false},
		{"resumable below min version", 13, ddl.TierEnterprise, "rebuild_index", "resumable", false},
		{"resumable at min version", 14, ddl.TierEnterprise, "rebuild_index", "resumable", true},
		{"azure evergreen overrides major", 9, ddl.TierAzure, "rebuild_index", "resumable", true},
		{"create walp at min version", 16, ddl.TierEnterprise, "create_index", "wait_at_low_priority", true},
		{"create walp below min version", 15, ddl.TierEnterprise, "create_index", "wait_at_low_priority", false},
		{"shrink walp 2022 enterprise", 16, ddl.TierEnterprise, "shrink_data", "wait_at_low_priority", true},
		{"shrink walp 2022 standard (not edition-gated)", 16, ddl.TierStandard, "shrink_data", "wait_at_low_priority", true},
		{"shrink walp 2022 express (not edition-gated)", 16, ddl.TierExpress, "shrink_data", "wait_at_low_priority", true},
		{"shrink walp 2019 too old", 15, ddl.TierEnterprise, "shrink_data", "wait_at_low_priority", false},
		{"shrink_log has no walp", 16, ddl.TierEnterprise, "shrink_log", "wait_at_low_priority", false},
		{"unknown option", 16, ddl.TierEnterprise, "rebuild_index", "nope", false},
		{"unknown command", 16, ddl.TierEnterprise, "nope", "online", false},
		{"command with no rules", 16, ddl.TierEnterprise, "drop_index", "online", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.Applicable(tt.major, tt.tier, tt.command, tt.option)
			if got != tt.want {
				t.Errorf("Applicable(%d, %v, %q, %q) = %t, want %t",
					tt.major, tt.tier, tt.command, tt.option, got, tt.want)
			}
		})
	}
}

func TestMatrixRule(t *testing.T) {
	m := testMatrix()

	got, ok := m.Rule("rebuild_index", "resumable")
	if !ok {
		t.Fatalf("Rule(rebuild_index, resumable) ok = false, want true")
	}
	want := ddl.OptionRule{
		MinMajor: 14,
		Editions: []ddl.Tier{ddl.TierEnterprise, ddl.TierAzure},
		Requires: []string{"online"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Rule(rebuild_index, resumable) mismatch (-want +got):\n%s", diff)
	}

	if _, ok := m.Rule("rebuild_index", "nope"); ok {
		t.Errorf("Rule(rebuild_index, nope) ok = true, want false")
	}
}

const sampleMatrixYAML = `
major_to_year:
  16: 2022
azure_pseudo_major: 9999
commands:
  rebuild_index:
    online: { min_major: 9, editions: [enterprise, azure] }
    resumable: { min_major: 14, editions: [enterprise, azure], requires: [online] }
  drop_index: {}
`

func TestLoad(t *testing.T) {
	m, err := ddl.Load(strings.NewReader(sampleMatrixYAML))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if got, want := m.AzurePseudoMajor, 9999; got != want {
		t.Errorf("AzurePseudoMajor = %d, want %d", got, want)
	}
	if got, want := m.MajorToYear[16], 2022; got != want {
		t.Errorf("MajorToYear[16] = %d, want %d", got, want)
	}

	got, ok := m.Rule("rebuild_index", "resumable")
	if !ok {
		t.Fatalf("Rule(rebuild_index, resumable) ok = false, want true")
	}
	want := ddl.OptionRule{
		MinMajor: 14,
		Editions: []ddl.Tier{ddl.TierEnterprise, ddl.TierAzure},
		Requires: []string{"online"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("loaded Rule mismatch (-want +got):\n%s", diff)
	}

	if !m.Applicable(16, ddl.TierEnterprise, "rebuild_index", "online") {
		t.Errorf("Applicable(16, enterprise, rebuild_index, online) = false, want true")
	}
}

func TestLoadInvalidEdition(t *testing.T) {
	const badYAML = `
commands:
  rebuild_index:
    online: { min_major: 9, editions: [bogus] }
`
	if _, err := ddl.Load(strings.NewReader(badYAML)); err == nil {
		t.Errorf("Load() with invalid edition error = nil, want non-nil")
	}
}
