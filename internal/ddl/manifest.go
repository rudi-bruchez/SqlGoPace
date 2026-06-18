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

// ObjectRef identifies the database object an operation targets. Most operations
// are object-scoped (Schema + Table + Name). A few are database-scoped (e.g.
// check_db): those set Database and leave Schema/Table/Name empty, so a database
// name is never smuggled into Table.
type ObjectRef struct {
	Schema   string
	Table    string
	Name     string // index, column, or constraint name depending on the operation
	Database string // set only for database-scoped operations (check_db); Schema/Table empty then
}

// String renders the reference for logs and reports: the database alone for a
// database-scoped operation, the logical file name for a file-scoped operation
// (shrink), "schema.table" for a table-level operation (no named object), else
// "schema.table.name".
func (r ObjectRef) String() string {
	switch {
	case r.Database != "" && r.Schema == "" && r.Table == "":
		return r.Database
	case r.Schema == "" && r.Table == "":
		return r.Name // file-scoped (shrink): the logical file name
	case r.Name == "":
		return r.Schema + "." + r.Table
	default:
		return r.Schema + "." + r.Table + "." + r.Name
	}
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

	// IgnoreBlocking is a reaction-policy override, NOT a T-SQL WITH option: when
	// true, the engine does not yield this operation when it blocks other sessions
	// (it holds its lock through to completion). Transaction-log protection still
	// applies. Use it to force an important rebuild through despite blocking.
	IgnoreBlocking *bool `yaml:"ignore_blocking"`
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

// OnFailure controls what the engine does when an operation fails. The empty
// value means stop (fail-fast), preserving the default behavior.
type OnFailure string

const (
	// OnFailureStop fails the whole manifest on the first failed operation.
	OnFailureStop OnFailure = "stop"
	// OnFailureContinue quarantines a failed operation, runs the rest, and writes
	// a re-runnable recovery manifest holding the failed operations.
	OnFailureContinue OnFailure = "continue"
)

// Manifest is one task: an ordered list of operations run sequentially.
type Manifest struct {
	Description string
	Database    string
	OnFailure   OnFailure // empty defaults to stop (fail-fast)
	Operations  []Operation
}

// Continue reports whether the manifest should keep going past a failed operation.
func (m *Manifest) Continue() bool { return m.OnFailure == OnFailureContinue }

// Validate checks the manifest has at least one operation and each is valid.
func (m *Manifest) Validate() error {
	switch m.OnFailure {
	case "", OnFailureStop, OnFailureContinue:
	default:
		return fmt.Errorf("on_failure must be %q or %q, got %q: %w",
			OnFailureStop, OnFailureContinue, m.OnFailure, ErrInvalidManifest)
	}
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
		OnFailure   string      `yaml:"on_failure"`
		Operations  []yaml.Node `yaml:"operations"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}

	m.Description = raw.Description
	m.Database = raw.Database
	m.OnFailure = OnFailure(strings.TrimSpace(raw.OnFailure))
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
	case "reorganize_index":
		return decodeInto[ReorganizeIndex](node)
	case "rebuild_heap":
		return decodeInto[RebuildHeap](node)
	case "update_statistics":
		return decodeInto[UpdateStatistics](node)
	case "check_db":
		return decodeInto[CheckDB](node)
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
	case "shrink":
		return decodeInto[Shrink](node)
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
// operation per index (resolved later against sys.indexes, see ExpandRebuildAll).
type RebuildIndex struct {
	Schema          string          `yaml:"schema"`
	Table           string          `yaml:"table"`
	Index           string          `yaml:"index"`
	Partition       *int            `yaml:"partition"` // nil = whole index; set = REBUILD PARTITION = n
	DataCompression string          `yaml:"data_compression"`
	Options         OptionOverrides `yaml:"options"`

	// Kind is the index's storage kind, populated only when this rebuild was
	// produced by expanding an "ALL" rebuild. It gates option resolution
	// (columnstore/XML/spatial reject ONLINE/RESUMABLE/WALP). KindUnknown for a
	// single named index loaded straight from YAML — no type gating is applied.
	Kind IndexKind `yaml:"-"`
}

func (o RebuildIndex) CommandType() string { return "rebuild_index" }
func (o RebuildIndex) Target() ObjectRef {
	return ObjectRef{Schema: o.Schema, Table: o.Table, Name: o.Index}
}
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
func (o CreateIndex) Target() ObjectRef {
	return ObjectRef{Schema: o.Schema, Table: o.Table, Name: o.Index}
}
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
func (o AlterColumn) Target() ObjectRef {
	return ObjectRef{Schema: o.Schema, Table: o.Table, Name: o.Column}
}
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
func (o AddColumn) Target() ObjectRef {
	return ObjectRef{Schema: o.Schema, Table: o.Table, Name: o.Column}
}
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
func (o AddConstraint) Target() ObjectRef {
	return ObjectRef{Schema: o.Schema, Table: o.Table, Name: o.Constraint}
}
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
func (o DropIndex) Target() ObjectRef {
	return ObjectRef{Schema: o.Schema, Table: o.Table, Name: o.Index}
}
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
func (o DropColumn) Target() ObjectRef {
	return ObjectRef{Schema: o.Schema, Table: o.Table, Name: o.Column}
}
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
func (o DropConstraint) Target() ObjectRef {
	return ObjectRef{Schema: o.Schema, Table: o.Table, Name: o.Constraint}
}
func (o DropConstraint) Validate() error {
	return requireFields("drop_constraint", map[string]string{
		"schema": o.Schema, "table": o.Table, "constraint": o.Constraint,
	})
}

// ReorganizeIndex is ALTER INDEX ... REORGANIZE: an always-online, incremental
// defragment. It cannot change data compression (that requires a REBUILD), so it
// carries no DataCompression. Partition nil means the whole index.
type ReorganizeIndex struct {
	Schema        string `yaml:"schema"`
	Table         string `yaml:"table"`
	Index         string `yaml:"index"`
	Partition     *int   `yaml:"partition"`      // nil = whole index
	LOBCompaction bool   `yaml:"lob_compaction"` // WITH (LOB_COMPACTION = ON)
}

func (o ReorganizeIndex) CommandType() string { return "reorganize_index" }
func (o ReorganizeIndex) Target() ObjectRef {
	return ObjectRef{Schema: o.Schema, Table: o.Table, Name: o.Index}
}
func (o ReorganizeIndex) Validate() error {
	return requireFields("reorganize_index", map[string]string{
		"schema": o.Schema, "table": o.Table, "index": o.Index,
	})
}

// RebuildHeap is ALTER TABLE ... REBUILD on a heap (a table with no clustered
// index). It clears forwarded records and reclaims space, and as a side effect
// rebuilds every nonclustered index on the table. It accepts ONLINE / MAXDOP /
// DATA_COMPRESSION but never RESUMABLE or WAIT_AT_LOW_PRIORITY (neither is valid
// for a heap rebuild). The target is the table itself — there is no index name.
type RebuildHeap struct {
	Schema          string          `yaml:"schema"`
	Table           string          `yaml:"table"`
	DataCompression string          `yaml:"data_compression"`
	Options         OptionOverrides `yaml:"options"`
}

func (o RebuildHeap) CommandType() string { return "rebuild_heap" }
func (o RebuildHeap) Target() ObjectRef   { return ObjectRef{Schema: o.Schema, Table: o.Table} }
func (o RebuildHeap) Validate() error {
	return requireFields("rebuild_heap", map[string]string{
		"schema": o.Schema, "table": o.Table,
	})
}

// UpdateStatistics is UPDATE STATISTICS. An empty Statistic targets every
// statistic on the table. Sampling comes from the maintenance rules: at most one
// of FullScan, SamplePercent, or Resample may be set.
type UpdateStatistics struct {
	Schema        string `yaml:"schema"`
	Table         string `yaml:"table"`
	Statistic     string `yaml:"statistic"`      // optional; empty = all statistics on the table
	FullScan      bool   `yaml:"full_scan"`      // WITH FULLSCAN
	SamplePercent *int   `yaml:"sample_percent"` // WITH SAMPLE n PERCENT (1..100)
	Resample      bool   `yaml:"resample"`       // WITH RESAMPLE
}

func (o UpdateStatistics) CommandType() string { return "update_statistics" }
func (o UpdateStatistics) Target() ObjectRef {
	return ObjectRef{Schema: o.Schema, Table: o.Table, Name: o.Statistic}
}
func (o UpdateStatistics) Validate() error {
	if err := requireFields("update_statistics", map[string]string{
		"schema": o.Schema, "table": o.Table,
	}); err != nil {
		return err
	}
	set := 0
	if o.FullScan {
		set++
	}
	if o.SamplePercent != nil {
		set++
	}
	if o.Resample {
		set++
	}
	if set > 1 {
		return fmt.Errorf("update_statistics: at most one of full_scan, sample_percent, resample: %w", ErrInvalidManifest)
	}
	if o.SamplePercent != nil && (*o.SamplePercent < 1 || *o.SamplePercent > 100) {
		return fmt.Errorf("update_statistics: sample_percent must be in 1..100: %w", ErrInvalidManifest)
	}
	return nil
}

// CheckDB is DBCC CHECKDB for an entire database. It is database-scoped: its
// Target carries the database name, never a table (see ObjectRef). NO_INFOMSGS and
// ALL_ERRORMSGS are always emitted; PHYSICAL_ONLY / DATA_PURITY are rule-driven
// switches, and only MAXDOP is resolved from the matrix.
type CheckDB struct {
	Database     string          `yaml:"database"`
	PhysicalOnly bool            `yaml:"physical_only"`
	DataPurity   bool            `yaml:"data_purity"`
	Options      OptionOverrides `yaml:"options"` // only MAXDOP applies
}

func (o CheckDB) CommandType() string { return "check_db" }
func (o CheckDB) Target() ObjectRef   { return ObjectRef{Database: o.Database} }
func (o CheckDB) Validate() error {
	return requireFields("check_db", map[string]string{"database": o.Database})
}

// Shrink is DBCC SHRINKFILE on a data or log file. It does not fit the
// "one operation = one statement" model: a dedicated runtime driver reads DMVs,
// builds the per-chunk SQL via the helpers in shrink.go, and runs its own loop.
// Like check_db it is file/database-scoped, so its Target carries the file name in
// Name and never a schema.table (see ObjectRef and the check_db target convention).
type Shrink struct {
	Type            string          `yaml:"type"`            // "data" | "log"
	Files           string          `yaml:"files"`           // "all" | logical file name; defaults to "all"
	EmptyFile       bool            `yaml:"emptyfile"`       // reserved for Phase 2; must be false in v1
	TargetFreeSpace string          `yaml:"targetfreespace"` // raw "10%" | "100MB"; parsed by ParseTargetFreeSpace
	Options         OptionOverrides `yaml:"options"`         // only WaitAtLowPriority is relevant
}

// FilesOrAll returns the configured logical file name, defaulting to "all".
func (o Shrink) FilesOrAll() string {
	if strings.TrimSpace(o.Files) == "" {
		return "all"
	}
	return o.Files
}

func (o Shrink) CommandType() string {
	if strings.EqualFold(strings.TrimSpace(o.Type), "log") {
		return "shrink_log"
	}
	return "shrink_data" // "data" and, pre-validation, any other value
}

func (o Shrink) Target() ObjectRef { return ObjectRef{Name: o.FilesOrAll()} }

func (o Shrink) Validate() error {
	switch strings.ToLower(strings.TrimSpace(o.Type)) {
	case "data", "log":
	default:
		return fmt.Errorf("shrink: type must be \"data\" or \"log\", got %q: %w", o.Type, ErrInvalidManifest)
	}
	if o.EmptyFile {
		return fmt.Errorf("shrink: emptyfile is reserved for Phase 2 and must be false: %w", ErrInvalidManifest)
	}
	if _, err := ParseTargetFreeSpace(o.TargetFreeSpace); err != nil {
		return fmt.Errorf("shrink: %w", err)
	}
	return nil
}
