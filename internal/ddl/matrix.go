// Package ddl is the pure DDL core of SqlGoPace: it parses operation manifests,
// resolves which DDL options a target server supports via the compatibility
// matrix, and generates the final T-SQL. It performs no I/O beyond reading the
// YAML it is handed, which makes it the most heavily unit-tested layer.
package ddl

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tier is the capability tier of a SQL Server edition. Online and resumable DDL
// operations are gated by tier as much as by version.
type Tier int

const (
	// TierUnknown is an unrecognized or unsupported edition.
	TierUnknown Tier = iota
	// TierEnterprise covers Enterprise, Developer, and Evaluation (EngineEdition 3).
	TierEnterprise
	// TierStandard covers Standard, Web, and Business Intelligence (EngineEdition 2).
	TierStandard
	// TierExpress covers all Express editions (EngineEdition 4).
	TierExpress
	// TierAzure covers Azure SQL Database and Managed Instance (EngineEdition 5, 8),
	// which are evergreen and treated as a very high pseudo-version.
	TierAzure
)

// ErrUnknownTier is returned by ParseTier for an unrecognized edition name.
var ErrUnknownTier = errors.New("unknown edition tier")

// String returns the lowercase canonical name of the tier.
func (t Tier) String() string {
	switch t {
	case TierEnterprise:
		return "enterprise"
	case TierStandard:
		return "standard"
	case TierExpress:
		return "express"
	case TierAzure:
		return "azure"
	default:
		return "unknown"
	}
}

// ParseTier converts an edition name (as written in the compatibility matrix)
// to a Tier. It is case-insensitive and trims surrounding whitespace.
func ParseTier(s string) (Tier, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "enterprise":
		return TierEnterprise, nil
	case "standard":
		return TierStandard, nil
	case "express":
		return TierExpress, nil
	case "azure":
		return TierAzure, nil
	default:
		return TierUnknown, fmt.Errorf("%q: %w", s, ErrUnknownTier)
	}
}

// TierFromEngineEdition maps SERVERPROPERTY('EngineEdition') to a Tier.
// Editions outside the supported set (Synapse, SQL Edge, Fabric) map to TierUnknown.
func TierFromEngineEdition(engineEdition int) Tier {
	switch engineEdition {
	case 2:
		return TierStandard
	case 3:
		return TierEnterprise
	case 4:
		return TierExpress
	case 5, 8:
		return TierAzure
	default:
		return TierUnknown
	}
}

// UnmarshalYAML decodes an edition name into a Tier, rejecting unknown names.
func (t *Tier) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := ParseTier(s)
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

// OptionRule declares, for one DDL option on one command, the minimum SQL Server
// major version and the editions that support it, plus any option dependencies.
type OptionRule struct {
	MinMajor int      `yaml:"min_major"`
	Editions []Tier   `yaml:"editions"`
	Requires []string `yaml:"requires"`
}

// CommandRules maps an option name (e.g. "online") to its rule for one command.
type CommandRules map[string]OptionRule

// Matrix is the data-driven compatibility matrix loaded from ddl_compatibility.yaml.
// It carries no SQL Server business logic in code: capability is pure lookup.
type Matrix struct {
	MajorToYear      map[int]int             `yaml:"major_to_year"`
	AzurePseudoMajor int                     `yaml:"azure_pseudo_major"`
	Commands         map[string]CommandRules `yaml:"commands"`
}

// Load decodes a Matrix from YAML, rejecting unknown fields so typos surface early.
func Load(r io.Reader) (*Matrix, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var m Matrix
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode compatibility matrix: %w", err)
	}
	return &m, nil
}

// LoadFile loads a Matrix from a file path.
func LoadFile(path string) (*Matrix, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open compatibility matrix: %w", err)
	}
	defer f.Close()

	return Load(f)
}

// Rule returns the rule for option on command, and whether it exists.
func (m *Matrix) Rule(command, option string) (OptionRule, bool) {
	rules, ok := m.Commands[command]
	if !ok {
		return OptionRule{}, false
	}
	rule, ok := rules[option]
	return rule, ok
}

// Applicable reports whether option is supported for command on a target server
// of the given major version and edition tier. Azure tiers are evergreen and use
// the configured pseudo-major, so any version-gated option is available.
func (m *Matrix) Applicable(major int, tier Tier, command, option string) bool {
	rule, ok := m.Rule(command, option)
	if !ok {
		return false
	}

	effectiveMajor := major
	if tier == TierAzure {
		effectiveMajor = m.AzurePseudoMajor
	}
	if effectiveMajor < rule.MinMajor {
		return false
	}
	return slices.Contains(rule.Editions, tier)
}
