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

// Manifest-related sentinel errors.
var (
	// ErrUnknownOperation is returned when an operation discriminator is not a
	// supported command type.
	ErrUnknownOperation = errors.New("unknown operation type")
	// ErrInvalidManifest is returned for a structurally valid YAML that violates
	// manifest rules (no operations, missing required fields, etc.).
	ErrInvalidManifest = errors.New("invalid manifest")
)

// ObjectRef identifies the database object an operation targets.
type ObjectRef struct {
	Schema string
	Table  string
	Name   string // index, column, or constraint name depending on the operation
}

// Operation is the closed set of supported DDL operations. Each kind is its own
// small struct; callers switch on the concrete type rather than a string.
type Operation interface {
	// CommandType returns the compatibility-matrix key (e.g. "rebuild_index").
	CommandType() string
	// Target returns the object the operation acts on, for logging and preflight.
	Target() ObjectRef
	// Validate reports whether the operation's required fields are present.
	Validate() error
}

// OptionOverrides carries per-operation overrides for injectable options.
// A nil pointer means "auto" (resolve from the compatibility matrix and config).
type OptionOverrides struct {
	Online            *bool `yaml:"online"`
	Resumable         *bool `yaml:"resumable"`
	WaitAtLowPriority *bool `yaml:"wait_at_low_priority"`
	SortInTempDB      *bool `yaml:"sort_in_tempdb"`
	MaxDOP            *int  `yaml:"maxdop"`
}

// Literal is a constant scalar default value as written in the manifest. It
// preserves whether the source scalar was a string so SQL generation can quote
// it correctly. Only constant defaults are supported in v1.
type Literal struct {
	Raw    string // literal text, e.g. "0" or "active"
	String bool   // true if the YAML scalar resolved to a string
}

// UnmarshalYAML captures a scalar constant, rejecting non-scalar defaults.
func (l *Literal) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("default must be a scalar constant: %w", ErrInvalidManifest)
	}
	l.Raw = value.Value
	l.String = value.Tag == "!!str"
	return nil
}

// Manifest is one task: an ordered list of operations run sequentially.
type Manifest struct {
	Description string
	Database    string
	Operations  []Operation
}

// Validate checks the manifest has at least one operation and each is valid.
func (m *Manifest) Validate() error {
	if len(m.Operations) == 0 {
		return fmt.Errorf("no operations: %w", ErrInvalidManifest)
	}
	for i, op := range m.Operations {
		if err := op.Validate(); err != nil {
			return fmt.Errorf("operation %d: %w", i, err)
		}
	}
	return nil
}

// UnmarshalYAML decodes the manifest, dispatching each operation to its concrete
// type based on the "operation" discriminator field.
func (m *Manifest) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Description string      `yaml:"description"`
		Database    string      `yaml:"database"`
		Operations  []yaml.Node `yaml:"operations"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}

	m.Description = raw.Description
	m.Database = raw.Database
	m.Operations = make([]Operation, 0, len(raw.Operations))
	for i := range raw.Operations {
		op, err := decodeOperation(&raw.Operations[i])
		if err != nil {
			return fmt.Errorf("operation %d: %w", i, err)
		}
		m.Operations = append(m.Operations, op)
	}
	return nil
}

// ParseManifest decodes and validates a manifest from YAML.
func ParseManifest(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := yaml.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadManifestFile loads and validates a manifest from a file path.
func LoadManifestFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()

	return ParseManifest(f)
}

// decodeOperation reads the discriminator and decodes into the matching type.
func decodeOperation(node *yaml.Node) (Operation, error) {
	var disc struct {
		Operation string `yaml:"operation"`
	}
	if err := node.Decode(&disc); err != nil {
		return nil, err
	}

	switch disc.Operation {
	case "":
		return nil, fmt.Errorf("missing %q field: %w", "operation", ErrInvalidManifest)
	case "rebuild_index":
		return decodeInto[RebuildIndex](node)
	case "create_index":
		return decodeInto[CreateIndex](node)
	case "alter_column":
		return decodeInto[AlterColumn](node)
	case "add_column":
		return decodeInto[AddColumn](node)
	case "add_constraint":
		return decodeInto[AddConstraint](node)
	case "drop_index":
		return decodeInto[DropIndex](node)
	case "drop_column":
		return decodeInto[DropColumn](node)
	case "drop_constraint":
		return decodeInto[DropConstraint](node)
	default:
		return nil, fmt.Errorf("%q: %w", disc.Operation, ErrUnknownOperation)
	}
}

// decodeInto decodes a YAML node into the concrete operation type T.
func decodeInto[T Operation](node *yaml.Node) (Operation, error) {
	var op T
	if err := node.Decode(&op); err != nil {
		return nil, err
	}
	return op, nil
}

// requireFields returns an ErrInvalidManifest error naming any blank fields.
func requireFields(opType string, fields map[string]string) error {
	var missing []string
	for name, val := range fields {
		if strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return fmt.Errorf("%s: missing required field(s): %s: %w",
		opType, strings.Join(missing, ", "), ErrInvalidManifest)
}

// --- Concrete operations -------------------------------------------------

// RebuildIndex is ALTER INDEX ... REBUILD. Index may be "ALL" to expand to one
// operation per index (resolved later against sys.indexes).
type RebuildIndex struct {
	Schema          string          `yaml:"schema"`
	Table           string          `yaml:"table"`
	Index           string          `yaml:"index"`
	DataCompression string          `yaml:"data_compression"`
	Options         OptionOverrides `yaml:"options"`
}

func (o RebuildIndex) CommandType() string { return "rebuild_index" }
func (o RebuildIndex) Target() ObjectRef   { return ObjectRef{o.Schema, o.Table, o.Index} }
func (o RebuildIndex) Validate() error {
	return requireFields("rebuild_index", map[string]string{
		"schema": o.Schema, "table": o.Table, "index": o.Index,
	})
}

// CreateIndex is CREATE [UNIQUE] INDEX.
type CreateIndex struct {
	Schema          string          `yaml:"schema"`
	Table           string          `yaml:"table"`
	Index           string          `yaml:"index"`
	Columns         []string        `yaml:"columns"`
	Unique          bool            `yaml:"unique"`
	DataCompression string          `yaml:"data_compression"`
	Options         OptionOverrides `yaml:"options"`
}

func (o CreateIndex) CommandType() string { return "create_index" }
func (o CreateIndex) Target() ObjectRef   { return ObjectRef{o.Schema, o.Table, o.Index} }
func (o CreateIndex) Validate() error {
	if err := requireFields("create_index", map[string]string{
		"schema": o.Schema, "table": o.Table, "index": o.Index,
	}); err != nil {
		return err
	}
	if len(o.Columns) == 0 {
		return fmt.Errorf("create_index: at least one column required: %w", ErrInvalidManifest)
	}
	return nil
}

// AlterColumn is ALTER TABLE ALTER COLUMN. v1 supports type + nullability only.
type AlterColumn struct {
	Schema   string          `yaml:"schema"`
	Table    string          `yaml:"table"`
	Column   string          `yaml:"column"`
	DataType string          `yaml:"type"`
	Nullable bool            `yaml:"nullable"`
	Options  OptionOverrides `yaml:"options"`
}

func (o AlterColumn) CommandType() string { return "alter_column" }
func (o AlterColumn) Target() ObjectRef   { return ObjectRef{o.Schema, o.Table, o.Column} }
func (o AlterColumn) Validate() error {
	return requireFields("alter_column", map[string]string{
		"schema": o.Schema, "table": o.Table, "column": o.Column, "type": o.DataType,
	})
}

// AddColumn is ALTER TABLE ADD. v1 supports type + nullability + constant default.
type AddColumn struct {
	Schema   string   `yaml:"schema"`
	Table    string   `yaml:"table"`
	Column   string   `yaml:"column"`
	DataType string   `yaml:"type"`
	Nullable bool     `yaml:"nullable"`
	Default  *Literal `yaml:"default"`
}

func (o AddColumn) CommandType() string { return "add_column" }
func (o AddColumn) Target() ObjectRef   { return ObjectRef{o.Schema, o.Table, o.Column} }
func (o AddColumn) Validate() error {
	return requireFields("add_column", map[string]string{
		"schema": o.Schema, "table": o.Table, "column": o.Column, "type": o.DataType,
	})
}

// AddConstraint is ALTER TABLE ADD CONSTRAINT (primary key or unique).
type AddConstraint struct {
	Schema     string          `yaml:"schema"`
	Table      string          `yaml:"table"`
	Constraint string          `yaml:"constraint"`
	Kind       string          `yaml:"kind"` // "primary_key" | "unique"
	Columns    []string        `yaml:"columns"`
	Options    OptionOverrides `yaml:"options"`
}

func (o AddConstraint) CommandType() string { return "add_constraint" }
func (o AddConstraint) Target() ObjectRef   { return ObjectRef{o.Schema, o.Table, o.Constraint} }
func (o AddConstraint) Validate() error {
	if err := requireFields("add_constraint", map[string]string{
		"schema": o.Schema, "table": o.Table, "constraint": o.Constraint, "kind": o.Kind,
	}); err != nil {
		return err
	}
	if len(o.Columns) == 0 {
		return fmt.Errorf("add_constraint: at least one column required: %w", ErrInvalidManifest)
	}
	return nil
}

// DropIndex is DROP INDEX.
type DropIndex struct {
	Schema string `yaml:"schema"`
	Table  string `yaml:"table"`
	Index  string `yaml:"index"`
}

func (o DropIndex) CommandType() string { return "drop_index" }
func (o DropIndex) Target() ObjectRef   { return ObjectRef{o.Schema, o.Table, o.Index} }
func (o DropIndex) Validate() error {
	return requireFields("drop_index", map[string]string{
		"schema": o.Schema, "table": o.Table, "index": o.Index,
	})
}

// DropColumn is ALTER TABLE DROP COLUMN.
type DropColumn struct {
	Schema string `yaml:"schema"`
	Table  string `yaml:"table"`
	Column string `yaml:"column"`
}

func (o DropColumn) CommandType() string { return "drop_column" }
func (o DropColumn) Target() ObjectRef   { return ObjectRef{o.Schema, o.Table, o.Column} }
func (o DropColumn) Validate() error {
	return requireFields("drop_column", map[string]string{
		"schema": o.Schema, "table": o.Table, "column": o.Column,
	})
}

// DropConstraint is ALTER TABLE DROP CONSTRAINT.
type DropConstraint struct {
	Schema     string `yaml:"schema"`
	Table      string `yaml:"table"`
	Constraint string `yaml:"constraint"`
}

func (o DropConstraint) CommandType() string { return "drop_constraint" }
func (o DropConstraint) Target() ObjectRef   { return ObjectRef{o.Schema, o.Table, o.Constraint} }
func (o DropConstraint) Validate() error {
	return requireFields("drop_constraint", map[string]string{
		"schema": o.Schema, "table": o.Table, "constraint": o.Constraint,
	})
}
