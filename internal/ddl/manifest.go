package ddl

import (
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
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

	// MaxBlockMinutes is a reaction-policy safety cap (not a T-SQL option): after this
	// many minutes of continuous blocking, the operation yields even if the blocked
	// session is allowed by ignore_blocked_sessions / ignore_blocking. It backstops a
	// too-broad ignore rule that would otherwise block real work indefinitely. Unset
	// or zero means no cap.
	MaxBlockMinutes *int `yaml:"max_block_minutes"`
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

// MarshalYAML renders the literal back to a bare scalar (not the {raw,string}
// struct), so a manifest carrying a Literal — an add_column default or a batch
// set value — round-trips through MarshalManifest. A string keeps its !!str tag so
// it re-parses as a string; other scalars are emitted untagged for type inference.
func (l Literal) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.ScalarNode, Value: l.Raw}
	if l.String {
		node.Tag = "!!str"
	}
	return node, nil
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

// Intent records why a rebuild was scheduled. A rebuild both applies a compression
// target (a state, idempotent) and defragments (an act, never idempotent); only the
// manifest knows which motivated it, and the engine cannot skip correctly without
// being told. Empty means "unknown" and always runs.
type Intent string

const (
	IntentCompression   Intent = "compression"
	IntentFragmentation Intent = "fragmentation"
)

// IgnoredSession matches sessions that are allowed to remain blocked by our DDL
// operation (the manifest's ignore_blocked_sessions list). All string fields are
// regular expressions, evaluated app-side because SQL Server has no regex before
// 2025. A session matches an entry when every field it sets matches (AND); the list
// is OR'd, so a session is ignorable if it matches any entry. An entry must set at
// least one field — an empty entry would match everything (equivalent to the blanket
// ignore_blocking) and is rejected.
type IgnoredSession struct {
	SessionID *int   `yaml:"session_id"` // nil = unset; when set, exact match on the blocked SPID
	AppName   string `yaml:"app_name"`   // regexp on program_name
	HostName  string `yaml:"host_name"`  // regexp on host_name
	LoginName string `yaml:"login_name"` // regexp on login_name
	Statement string `yaml:"statement"`  // regexp on the blocked session's SQL text
}

// regexpFields returns the rule's regexp criteria paired with their names, in a
// stable order, so validation, rendering, and matching agree on the field set.
func (s IgnoredSession) regexpFields() []struct{ name, expr string } {
	return []struct{ name, expr string }{
		{"app_name", s.AppName},
		{"host_name", s.HostName},
		{"login_name", s.LoginName},
		{"statement", s.Statement},
	}
}

// String renders the rule for --explain: each field it sets as "field<op>value"
// (session_id with "=", the regexp fields with "~"), joined by " AND ".
func (s IgnoredSession) String() string {
	var parts []string
	if s.SessionID != nil {
		parts = append(parts, fmt.Sprintf("session_id=%d", *s.SessionID))
	}
	for _, f := range s.regexpFields() {
		if f.expr != "" {
			parts = append(parts, f.name+"~"+f.expr)
		}
	}
	return strings.Join(parts, " AND ")
}

// validate reports whether the entry is usable: at least one field set, a positive
// session_id when present, and every non-empty regexp compiles (fail-fast at load).
func (s IgnoredSession) validate() error {
	if s.SessionID == nil && s.AppName == "" && s.HostName == "" && s.LoginName == "" && s.Statement == "" {
		return fmt.Errorf("an entry must set at least one of session_id/app_name/host_name/login_name/statement: %w", ErrInvalidManifest)
	}
	if s.SessionID != nil && *s.SessionID <= 0 {
		return fmt.Errorf("session_id must be positive, got %d: %w", *s.SessionID, ErrInvalidManifest)
	}
	for _, f := range s.regexpFields() {
		if f.expr == "" {
			continue
		}
		if _, err := regexp.Compile(f.expr); err != nil {
			return fmt.Errorf("%s is not a valid regexp %q: %w", f.name, f.expr, ErrInvalidManifest)
		}
	}
	return nil
}

// KilledSession is a rule for a session the engine may KILL when it blocks this run's DDL.
// It embeds IgnoredSession's match fields (session_id + app/host/login/statement regexps) and
// adds AfterSeconds: the session is killed only once it has blocked our operation continuously
// for at least that long (0 = on sight). It is the inverse of IgnoredSession — ignore holds
// the lock through a session, kill terminates it — so it reuses IgnoredSession's matching,
// validation, and rendering directly through the embedded value.
type KilledSession struct {
	IgnoredSession `yaml:",inline"`
	// AfterSeconds is omitempty so a zero delay (kill on sight) is not rendered back.
	AfterSeconds int `yaml:"after_seconds,omitempty"`
}

// String renders the rule for --explain, appending the delay when set.
func (s KilledSession) String() string {
	if s.AfterSeconds > 0 {
		return fmt.Sprintf("%s after %ds", s.IgnoredSession.String(), s.AfterSeconds)
	}
	return s.IgnoredSession.String()
}

// validate reuses the ignore-rule checks (≥1 match field, positive session_id, valid
// regexps) and additionally rejects a negative delay.
func (s KilledSession) validate() error {
	if err := s.IgnoredSession.validate(); err != nil {
		return err
	}
	if s.AfterSeconds < 0 {
		return fmt.Errorf("after_seconds must be >= 0, got %d: %w", s.AfterSeconds, ErrInvalidManifest)
	}
	return nil
}

// Manifest is one task: an ordered list of operations run sequentially.
type Manifest struct {
	Description string
	Database    string
	OnFailure   OnFailure // empty defaults to stop (fail-fast)
	// Intent is the default recorded on every rebuild_index that sets none of its own
	// (see Intent). It is resolved where it is used, not pushed into operations at load.
	Intent Intent
	// IgnoreBlockedSessions lists sessions allowed to remain blocked by this run's
	// operations (see IgnoredSession). It is the single durable source of exclusions:
	// read at start, re-read live during the run, and copied into the recovery manifest.
	IgnoreBlockedSessions []IgnoredSession
	// KillBlockingSessions lists sessions the engine may KILL when they block this run's
	// operations (see KilledSession). Like IgnoreBlockedSessions it is read at start,
	// re-read live during the run, and copied into the recovery manifest; the TUI appends
	// to it on operator request. Kills only occur when armed by config (kill_blockers.enabled).
	KillBlockingSessions []KilledSession
	// AbortBlockingResumable lets the engine clear a stale/foreign paused resumable that
	// blocks a fresh REBUILD of the target index (SQL Server Msg 10637), with ALTER INDEX
	// … ABORT, before running the operation. Off by default (the run fails with an
	// actionable message instead), because aborting discards the paused operation's
	// server-side progress — a deliberate choice on a shared/production database.
	AbortBlockingResumable bool
	// Window, when set, restricts this manifest's operations to a recurring
	// server-time window: outside it the manifest is deferred; a window closing
	// mid-run stops cleanly at the next operation boundary (see run engine).
	Window     *Window
	Operations []Operation
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
	if err := validateIntent(m.Intent); err != nil {
		return err
	}
	for i, s := range m.IgnoreBlockedSessions {
		if err := s.validate(); err != nil {
			return fmt.Errorf("ignore_blocked_sessions[%d]: %w", i, err)
		}
	}
	for i, s := range m.KillBlockingSessions {
		if err := s.validate(); err != nil {
			return fmt.Errorf("kill_blocking_sessions[%d]: %w", i, err)
		}
	}
	if m.Window != nil {
		if err := m.Window.Validate(); err != nil {
			return fmt.Errorf("window: %w", err)
		}
	}
	if len(m.Operations) == 0 {
		return fmt.Errorf("no operations: %w", ErrInvalidManifest)
	}
	for i, op := range m.Operations {
		if err := op.Validate(); err != nil {
			return fmt.Errorf("operation %d: %w", i, err)
		}
		if err := validateMaxDOP(overridesOf(op).MaxDOP); err != nil {
			return fmt.Errorf("operation %d: %w", i, err)
		}
	}
	return nil
}

// MaxDOPCeiling is the largest degree of parallelism SQL Server accepts. Outside
// 0..MaxDOPCeiling both forms the generator emits are refused: the index option with
// Msg 304 ("out of range for index/statistics option 'MAXDOP'"), and the batched-DML
// query hint with Msg 304 above the ceiling and Msg 102 below zero, the negative sign
// not even being grammatical there. Zero is legal and means "all available processors".
// Measured against SQL Server 2022 CU26.
const MaxDOPCeiling = 32767

// validateMaxDOP rejects a degree of parallelism the server would refuse, so an
// unrunnable manifest is caught at load rather than at the statement that fails.
func validateMaxDOP(md *int) error {
	if md == nil || (*md >= 0 && *md <= MaxDOPCeiling) {
		return nil
	}
	return fmt.Errorf("maxdop must be in 0..%d, got %d: %w", MaxDOPCeiling, *md, ErrInvalidManifest)
}

// UnmarshalYAML decodes the manifest, dispatching each operation to its concrete
// type based on the "operation" discriminator field.
func (m *Manifest) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Description            string           `yaml:"description"`
		Database               string           `yaml:"database"`
		OnFailure              string           `yaml:"on_failure"`
		Intent                 string           `yaml:"intent"`
		IgnoreBlockedSessions  []IgnoredSession `yaml:"ignore_blocked_sessions"`
		KillBlockingSessions   []KilledSession  `yaml:"kill_blocking_sessions"`
		AbortBlockingResumable bool             `yaml:"abort_blocking_resumable"`
		Window                 *Window          `yaml:"window"`
		Operations             []yaml.Node      `yaml:"operations"`
	}
	if err := checkKnownFields(value, reflect.TypeOf(raw)); err != nil {
		return err
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}

	m.Description = raw.Description
	m.Database = raw.Database
	m.OnFailure = OnFailure(strings.TrimSpace(raw.OnFailure))
	m.Intent = Intent(strings.TrimSpace(raw.Intent))
	m.IgnoreBlockedSessions = raw.IgnoreBlockedSessions
	m.KillBlockingSessions = raw.KillBlockingSessions
	m.AbortBlockingResumable = raw.AbortBlockingResumable
	m.Window = raw.Window
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
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	// Checked before decoding: a non-mapping (a bare scalar in the list) would otherwise
	// fail inside yaml.v3 with a message naming the anonymous discriminator struct.
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: operation must be a mapping: %w", node.Line, ErrInvalidManifest)
	}
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
	case "shrink_tempdb":
		return decodeInto[ShrinkTempdb](node)
	case "batch_update":
		return decodeBatchDML(node, "update")
	case "batch_delete":
		return decodeBatchDML(node, "delete")
	default:
		return nil, fmt.Errorf("%q: %w", disc.Operation, ErrUnknownOperation)
	}
}

// decodeBatchDML decodes a batch_update/batch_delete node and stamps the verb,
// which is carried by the discriminator rather than a YAML field.
func decodeBatchDML(node *yaml.Node, verb string) (Operation, error) {
	var op BatchDML
	if err := decodeOperationNode(node, &op); err != nil {
		return nil, err
	}
	op.Verb = verb
	return op, nil
}

// decodeInto decodes a YAML node into the concrete operation type T.
func decodeInto[T Operation](node *yaml.Node) (Operation, error) {
	var op T
	if err := decodeOperationNode(node, &op); err != nil {
		return nil, err
	}
	return op, nil
}

// decodeOperationNode decodes an operation node, rejecting keys that map to no field of the
// concrete type. The "operation" discriminator is exempt: every operation node carries it,
// no operation type declares it.
func decodeOperationNode(node *yaml.Node, out any) error {
	if err := checkKnownFields(withoutKey(node, "operation"), reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	return node.Decode(out)
}

// withoutKey returns a shallow copy of a mapping node with one key/value pair removed. The
// children are shared, so every node keeps its source position; the original is left
// untouched because decodeOperation reads the discriminator from it first.
func withoutKey(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return node
	}
	out := *node
	out.Content = make([]*yaml.Node, 0, len(node.Content))
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			continue
		}
		out.Content = append(out.Content, node.Content[i], node.Content[i+1])
	}
	return &out
}

var (
	yamlNodeType        = reflect.TypeOf(yaml.Node{})
	yamlUnmarshalerType = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()
)

// checkKnownFields reports the first YAML key in node that maps to no field of t, naming the
// key and its line.
//
// config.yaml, maintenance_profile.yaml and ddl_compatibility.yaml all decode with
// KnownFields(true); manifests did not, so a misspelled key was silently dropped — a
// `data_compresion: PAGE` typo produced an uncompressed rebuild reported as success. Setting
// KnownFields on the manifest decoder does not fix that: yaml.v3 does not carry the flag into
// a type's UnmarshalYAML, and both Manifest and every operation type decode through one.
//
// So the check walks the parsed document instead. Decoding still runs against the original
// node, which keeps yaml.v3's own errors pointing at the operator's line numbers and leaves
// anchors, aliases and merge keys resolvable — none of which survive re-encoding a subtree.
func checkKnownFields(node *yaml.Node, t reflect.Type) error {
	if node == nil || t == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode {
		return checkKnownFields(node.Alias, t)
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// A type that decodes itself owns its shape, and a yaml.Node holds an arbitrary document.
	if t == yamlNodeType || reflect.PointerTo(t).Implements(yamlUnmarshalerType) {
		return nil
	}
	switch {
	case t.Kind() == reflect.Slice && node.Kind == yaml.SequenceNode:
		for _, el := range node.Content {
			if err := checkKnownFields(el, t.Elem()); err != nil {
				return err
			}
		}
	case t.Kind() == reflect.Struct && node.Kind == yaml.MappingNode:
		fields := yamlFields(t)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			// A merge key contributes the merged mapping's keys to this same struct.
			if key.Value == "<<" {
				if err := checkMerged(val, t); err != nil {
					return err
				}
				continue
			}
			f, ok := fields[key.Value]
			if !ok {
				return fmt.Errorf("line %d: unknown field %q: %w", key.Line, key.Value, ErrInvalidManifest)
			}
			if err := checkKnownFields(val, f.Type); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkMerged checks a merge key's value — one mapping, or a sequence of them — against t.
func checkMerged(val *yaml.Node, t reflect.Type) error {
	if val.Kind == yaml.SequenceNode {
		for _, el := range val.Content {
			if err := checkKnownFields(el, t); err != nil {
				return err
			}
		}
		return nil
	}
	return checkKnownFields(val, t)
}

// yamlFields maps a struct's YAML keys to their fields, following yaml.v3's rules: an
// untagged field is keyed by its lowercased name, `yaml:"-"` is not a key at all, and an
// inline struct contributes its own fields to the parent.
func yamlFields(t reflect.Type) map[string]reflect.StructField {
	out := make(map[string]reflect.StructField, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if strings.Contains(opts, "inline") {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range yamlFields(ft) {
					out[k] = v
				}
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f
	}
	return out
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

// validateIntent accepts an empty intent (unset) or one of the two constants.
func validateIntent(i Intent) error {
	switch i {
	case "", IntentCompression, IntentFragmentation:
		return nil
	default:
		return fmt.Errorf("intent must be %q or %q, got %q: %w",
			IntentCompression, IntentFragmentation, i, ErrInvalidManifest)
	}
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
	Intent          Intent          `yaml:"intent,omitempty"`
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
	if err := requireFields("rebuild_index", map[string]string{
		"schema": o.Schema, "table": o.Table, "index": o.Index,
	}); err != nil {
		return err
	}
	return validateIntent(o.Intent)
}

// CreateIndex is CREATE [UNIQUE] INDEX.
type CreateIndex struct {
	Schema          string          `yaml:"schema"`
	Table           string          `yaml:"table"`
	Index           string          `yaml:"index"`
	Columns         []string        `yaml:"columns"`
	Unique          bool            `yaml:"unique,omitempty"`
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
	Nullable bool            `yaml:"nullable,omitempty"`
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
	Nullable bool     `yaml:"nullable,omitempty"`
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
	Partition     *int   `yaml:"partition"`                // nil = whole index
	LOBCompaction bool   `yaml:"lob_compaction,omitempty"` // WITH (LOB_COMPACTION = ON)
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
	Statistic     string `yaml:"statistic"`           // optional; empty = all statistics on the table
	FullScan      bool   `yaml:"full_scan,omitempty"` // WITH FULLSCAN
	SamplePercent *int   `yaml:"sample_percent"`      // WITH SAMPLE n PERCENT (1..100)
	Resample      bool   `yaml:"resample,omitempty"`  // WITH RESAMPLE
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
	PhysicalOnly bool            `yaml:"physical_only,omitempty"`
	DataPurity   bool            `yaml:"data_purity,omitempty"`
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
	Type               string          `yaml:"type"`                           // "data" | "log"
	Files              string          `yaml:"files"`                          // "all" | logical file name; defaults to "all"
	EmptyFile          bool            `yaml:"emptyfile,omitempty"`            // reserved for Phase 2; must be false in v1
	TargetFreeSpace    string          `yaml:"targetfreespace"`                // raw "10%" | "100MB"; parsed by ParseTargetFreeSpace
	Options            OptionOverrides `yaml:"options"`                        // only WaitAtLowPriority is relevant
	IdentifyTailObject bool            `yaml:"identify_tail_object,omitempty"` // run the tail-object walk at shrink start
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

// ShrinkTempdb shrinks every tempdb data file to a common absolute size. It is a
// dedicated operation (not a Shrink variant): tempdb is always database_id 2, has
// no schema.table target, and the runtime driver adds tempdb-specific escalation
// (see docs/specs/TEMPDB-SHRINK.md). Like check_db it is database-scoped.
type ShrinkTempdb struct {
	TargetSizeMB int             `yaml:"targetsizemb"`          // common absolute target per data file, MB
	FlushCaches  bool            `yaml:"flushcaches,omitempty"` // opt-in cache-flush escalation on persistent stall
	Options      OptionOverrides `yaml:"options"`               // only WaitAtLowPriority is relevant
}

func (o ShrinkTempdb) CommandType() string { return "shrink_tempdb" }
func (o ShrinkTempdb) Target() ObjectRef   { return ObjectRef{Database: "tempdb"} }

func (o ShrinkTempdb) Validate() error {
	if o.TargetSizeMB <= 0 {
		return fmt.Errorf("shrink_tempdb: targetsizemb must be > 0, got %d: %w", o.TargetSizeMB, ErrInvalidManifest)
	}
	return nil
}

// Condition is one simple predicate in a batch_update/batch_delete where list: a
// column compared to a scalar literal, or a NULL test. Conditions are AND-ed. The
// operator must be in the fixed allowlist (see batch.go); arbitrary SQL goes through
// where_raw instead.
type Condition struct {
	Column string   `yaml:"column"`
	Op     string   `yaml:"op"`    // one of the batchOps allowlist
	Value  *Literal `yaml:"value"` // required, except for IS NULL / IS NOT NULL
}

// validate reports whether the condition is usable: a column, an allowed operator,
// and a value present iff the operator needs one.
func (c Condition) validate() error {
	if strings.TrimSpace(c.Column) == "" {
		return fmt.Errorf("condition: column is required: %w", ErrInvalidManifest)
	}
	norm, ok := normalizeBatchOp(c.Op)
	if !ok {
		return fmt.Errorf("condition: unsupported op %q: %w", c.Op, ErrInvalidManifest)
	}
	if batchOpIsNullTest(norm) {
		if c.Value != nil {
			return fmt.Errorf("condition: %s takes no value: %w", norm, ErrInvalidManifest)
		}
	} else if c.Value == nil {
		return fmt.Errorf("condition: op %s requires a value: %w", norm, ErrInvalidManifest)
	}
	return nil
}

// BatchSpec configures how a batch_update/batch_delete is chunked.
type BatchSpec struct {
	Strategy    string `yaml:"strategy"`     // "" or "predicate" (key_range is planned for a later iteration)
	Key         string `yaml:"key"`          // key column for key_range; unused by the predicate strategy
	InitialRows *int   `yaml:"initial_rows"` // optional starting batch size; auto-sized when nil
}

// IsKeyRange reports whether the spec selects the key_range walk strategy.
func (b BatchSpec) IsKeyRange() bool {
	return strings.EqualFold(strings.TrimSpace(b.Strategy), "key_range")
}

// validate reports whether the batch spec is usable.
func (b BatchSpec) validate() error {
	switch strings.ToLower(strings.TrimSpace(b.Strategy)) {
	case "", "predicate", "key_range":
	default:
		return fmt.Errorf("batch.strategy must be \"predicate\" or \"key_range\", got %q: %w", b.Strategy, ErrInvalidManifest)
	}
	if b.InitialRows != nil && *b.InitialRows <= 0 {
		return fmt.Errorf("batch.initial_rows must be positive, got %d: %w", *b.InitialRows, ErrInvalidManifest)
	}
	return nil
}

// BatchDML is a chunked UPDATE or DELETE: a large DML statement run as a loop of
// small, individually-committed batches so it never escalates to a table lock or
// blows up the transaction log. Like Shrink it does not fit the "one operation =
// one statement" model: a dedicated runtime driver builds the per-batch SQL and
// runs its own loop (see docs/specs/BATCH-DML.md). It is table-scoped (no named object,
// like rebuild_heap). Verb is carried by the discriminator, not a YAML field.
type BatchDML struct {
	Verb             string             `yaml:"-"` // "update" | "delete"
	Schema           string             `yaml:"schema"`
	Table            string             `yaml:"table"`
	Set              map[string]Literal `yaml:"set"`       // column -> scalar literal (update only)
	SetRaw           string             `yaml:"set_raw"`   // guarded raw SET list (update only)
	Where            []Condition        `yaml:"where"`     // simple conditions, AND-ed
	WhereRaw         string             `yaml:"where_raw"` // guarded raw predicate
	Batch            BatchSpec          `yaml:"batch"`
	ConfirmFullTable bool               `yaml:"confirm_full_table,omitempty"`
	Options          OptionOverrides    `yaml:"options"` // only MAXDOP applies to DML
}

func (o BatchDML) CommandType() string {
	if o.Verb == "delete" {
		return "batch_delete"
	}
	return "batch_update"
}

func (o BatchDML) Target() ObjectRef { return ObjectRef{Schema: o.Schema, Table: o.Table} }

func (o BatchDML) Validate() error {
	cmd := o.CommandType()
	if err := requireFields(cmd, map[string]string{"schema": o.Schema, "table": o.Table}); err != nil {
		return err
	}

	hasSet := len(o.Set) > 0
	hasSetRaw := strings.TrimSpace(o.SetRaw) != ""
	switch o.Verb {
	case "update":
		if hasSet == hasSetRaw { // neither set, or both set
			return fmt.Errorf("%s: set exactly one of set or set_raw: %w", cmd, ErrInvalidManifest)
		}
	case "delete":
		if hasSet || hasSetRaw {
			return fmt.Errorf("%s: delete takes no set/set_raw: %w", cmd, ErrInvalidManifest)
		}
	default:
		return fmt.Errorf("batch_dml: unknown verb %q: %w", o.Verb, ErrInvalidManifest)
	}
	for col := range o.Set {
		if strings.TrimSpace(col) == "" {
			return fmt.Errorf("%s: set has an empty column name: %w", cmd, ErrInvalidManifest)
		}
	}

	hasWhere := len(o.Where) > 0
	hasWhereRaw := strings.TrimSpace(o.WhereRaw) != ""
	if hasWhere && hasWhereRaw {
		return fmt.Errorf("%s: set at most one of where or where_raw: %w", cmd, ErrInvalidManifest)
	}
	for i, c := range o.Where {
		if err := c.validate(); err != nil {
			return fmt.Errorf("%s: where[%d]: %w", cmd, i, err)
		}
	}

	// Whole-table guard: no predicate at all is destructive and must be confirmed.
	if !hasWhere && !hasWhereRaw && !o.ConfirmFullTable {
		return fmt.Errorf("%s: no where/where_raw targets the whole table; set confirm_full_table: true to allow it (or use TRUNCATE for an unconditional delete): %w", cmd, ErrInvalidManifest)
	}
	// A raw whole-table UPDATE cannot self-limit under the predicate strategy, so it
	// would loop forever — require a self-limiting where_raw.
	if o.Verb == "update" && hasSetRaw && !hasWhere && !hasWhereRaw && !o.Batch.IsKeyRange() {
		return fmt.Errorf("%s: set_raw without where/where_raw cannot self-limit (it would loop forever); add a self-limiting where_raw: %w", cmd, ErrInvalidManifest)
	}

	// The key_range walk persists a watermark and re-applies the boundary batch on a
	// resume, so it is restricted to an idempotent literal UPDATE.
	if o.Batch.IsKeyRange() {
		switch {
		case o.Verb != "update":
			return fmt.Errorf("%s: key_range is only for batch_update (delete is already self-limiting under predicate): %w", cmd, ErrInvalidManifest)
		case !hasSet:
			return fmt.Errorf("%s: key_range requires a literal set: (a non-idempotent set_raw cannot be safely re-applied on resume): %w", cmd, ErrInvalidManifest)
		}
	}

	return o.Batch.validate()
}
