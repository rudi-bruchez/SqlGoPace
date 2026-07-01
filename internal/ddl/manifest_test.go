package ddl_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

const rebuildAndAddYAML = `
description: "Rebuild and add a column"
database: MYDB
operations:
  - operation: rebuild_index
    schema: dbo
    table: DISPATCH
    index: IX_DISPATCH
    data_compression: PAGE
    options:
      maxdop: 4
  - operation: add_column
    schema: dbo
    table: DISPATCH
    column: PROCESSED
    type: BIT
    nullable: false
    default: 0
`

func TestParseManifest(t *testing.T) {
	m, err := ddl.ParseManifest(strings.NewReader(rebuildAndAddYAML))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v, want nil", err)
	}

	if got, want := m.Description, "Rebuild and add a column"; got != want {
		t.Errorf("Description = %q, want %q", got, want)
	}
	if got, want := m.Database, "MYDB"; got != want {
		t.Errorf("Database = %q, want %q", got, want)
	}
	if got, want := len(m.Operations), 2; got != want {
		t.Fatalf("len(Operations) = %d, want %d", got, want)
	}

	rebuild, ok := m.Operations[0].(ddl.RebuildIndex)
	if !ok {
		t.Fatalf("Operations[0] type = %T, want ddl.RebuildIndex", m.Operations[0])
	}
	maxdop := 4
	want := ddl.RebuildIndex{
		Schema:          "dbo",
		Table:           "DISPATCH",
		Index:           "IX_DISPATCH",
		DataCompression: "PAGE",
		Options:         ddl.OptionOverrides{MaxDOP: &maxdop},
	}
	if diff := cmp.Diff(want, rebuild); diff != "" {
		t.Errorf("Operations[0] mismatch (-want +got):\n%s", diff)
	}
	if got, want := rebuild.CommandType(), "rebuild_index"; got != want {
		t.Errorf("CommandType() = %q, want %q", got, want)
	}
	if got, want := rebuild.Target(), (ddl.ObjectRef{Schema: "dbo", Table: "DISPATCH", Name: "IX_DISPATCH"}); got != want {
		t.Errorf("Target() = %+v, want %+v", got, want)
	}

	add, ok := m.Operations[1].(ddl.AddColumn)
	if !ok {
		t.Fatalf("Operations[1] type = %T, want ddl.AddColumn", m.Operations[1])
	}
	if add.Default == nil {
		t.Fatalf("AddColumn.Default = nil, want literal 0")
	}
	if got, want := add.Default.Raw, "0"; got != want {
		t.Errorf("AddColumn.Default.Raw = %q, want %q", got, want)
	}
	if add.Default.String {
		t.Errorf("AddColumn.Default.String = true, want false for numeric literal")
	}
}

func TestParseOnFailure(t *testing.T) {
	for _, tt := range []struct {
		name     string
		yaml     string
		want     ddl.OnFailure
		wantCont bool
	}{
		{name: "absent defaults to stop", yaml: "", want: "", wantCont: false},
		{name: "explicit stop", yaml: "on_failure: stop\n", want: ddl.OnFailureStop, wantCont: false},
		{name: "continue", yaml: "on_failure: continue\n", want: ddl.OnFailureContinue, wantCont: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			y := tt.yaml + "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
			m, err := ddl.ParseManifest(strings.NewReader(y))
			if err != nil {
				t.Fatalf("ParseManifest() error = %v", err)
			}
			if m.OnFailure != tt.want {
				t.Errorf("OnFailure = %q, want %q", m.OnFailure, tt.want)
			}
			if m.Continue() != tt.wantCont {
				t.Errorf("Continue() = %v, want %v", m.Continue(), tt.wantCont)
			}
		})
	}
}

func TestParseSkipIfSatisfied(t *testing.T) {
	op := "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
	for _, tt := range []struct {
		name string
		yaml string
		want bool
	}{
		{name: "absent defaults false", yaml: "", want: false},
		{name: "explicit true", yaml: "skip_if_satisfied: true\n", want: true},
		{name: "explicit false", yaml: "skip_if_satisfied: false\n", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ddl.ParseManifest(strings.NewReader(tt.yaml + op))
			if err != nil {
				t.Fatalf("ParseManifest() error = %v", err)
			}
			if m.SkipIfSatisfied != tt.want {
				t.Errorf("SkipIfSatisfied = %v, want %v", m.SkipIfSatisfied, tt.want)
			}
		})
	}
}

func TestParseIgnoreBlockedSessions(t *testing.T) {
	y := `
ignore_blocked_sessions:
  - app_name: "^SQLAgent"
    login_name: "svc_reporting"
  - session_id: 57
  - statement: "dbo\\.Report"
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`
	m, err := ddl.ParseManifest(strings.NewReader(y))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}
	id57 := 57
	want := []ddl.IgnoredSession{
		{AppName: "^SQLAgent", LoginName: "svc_reporting"},
		{SessionID: &id57},
		{Statement: `dbo\.Report`},
	}
	if diff := cmp.Diff(want, m.IgnoreBlockedSessions); diff != "" {
		t.Errorf("IgnoreBlockedSessions mismatch (-want +got):\n%s", diff)
	}
}

const maintenanceYAML = `
description: "Maintenance operations"
operations:
  - operation: reorganize_index
    schema: dbo
    table: T
    index: IX
    partition: 2
    lob_compaction: true
  - operation: rebuild_heap
    schema: dbo
    table: H
    data_compression: PAGE
  - operation: update_statistics
    schema: dbo
    table: T
    statistic: ST
    sample_percent: 30
  - operation: check_db
    database: MYDB
    physical_only: true
`

func TestParseMaintenanceManifest(t *testing.T) {
	m, err := ddl.ParseManifest(strings.NewReader(maintenanceYAML))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v, want nil", err)
	}
	if got, want := len(m.Operations), 4; got != want {
		t.Fatalf("len(Operations) = %d, want %d", got, want)
	}

	partition := 2
	sample := 30
	wantOps := []ddl.Operation{
		ddl.ReorganizeIndex{Schema: "dbo", Table: "T", Index: "IX", Partition: &partition, LOBCompaction: true},
		ddl.RebuildHeap{Schema: "dbo", Table: "H", DataCompression: "PAGE"},
		ddl.UpdateStatistics{Schema: "dbo", Table: "T", Statistic: "ST", SamplePercent: &sample},
		ddl.CheckDB{Database: "MYDB", PhysicalOnly: true},
	}
	if diff := cmp.Diff(wantOps, m.Operations); diff != "" {
		t.Errorf("Operations mismatch (-want +got):\n%s", diff)
	}

	// check_db is database-scoped: the database lives in Target().Database, never Table.
	cdb := m.Operations[3].(ddl.CheckDB)
	ref := cdb.Target()
	if ref.Database != "MYDB" || ref.Table != "" || ref.Schema != "" {
		t.Errorf("CheckDB.Target() = %+v, want only Database set", ref)
	}
	if got, want := ref.String(), "MYDB"; got != want {
		t.Errorf("CheckDB Target().String() = %q, want %q", got, want)
	}
	// rebuild_heap is table-level: no named object, so String() drops the trailing dot.
	if got, want := m.Operations[1].Target().String(), "dbo.H"; got != want {
		t.Errorf("RebuildHeap Target().String() = %q, want %q", got, want)
	}
}

func TestParseManifestErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr error
	}{
		{
			name:    "unknown operation",
			yaml:    "operations:\n  - operation: drop_database\n    schema: dbo\n",
			wantErr: ddl.ErrUnknownOperation,
		},
		{
			name:    "missing operation field",
			yaml:    "operations:\n  - schema: dbo\n    table: t\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "missing required field",
			yaml:    "operations:\n  - operation: rebuild_index\n    schema: dbo\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "no operations",
			yaml:    "description: empty\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "check_db missing database",
			yaml:    "operations:\n  - operation: check_db\n    physical_only: true\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "reorganize_index missing index",
			yaml:    "operations:\n  - operation: reorganize_index\n    schema: dbo\n    table: T\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "update_statistics conflicting sampling",
			yaml:    "operations:\n  - operation: update_statistics\n    schema: dbo\n    table: T\n    full_scan: true\n    resample: true\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "update_statistics sample percent out of range",
			yaml:    "operations:\n  - operation: update_statistics\n    schema: dbo\n    table: T\n    sample_percent: 200\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "shrink invalid type",
			yaml:    "operations:\n  - operation: shrink\n    type: index\n    targetfreespace: 10%\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "shrink emptyfile reserved",
			yaml:    "operations:\n  - operation: shrink\n    type: data\n    emptyfile: true\n    targetfreespace: 10%\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "shrink unparseable targetfreespace",
			yaml:    "operations:\n  - operation: shrink\n    type: data\n    targetfreespace: lots\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "shrink missing targetfreespace",
			yaml:    "operations:\n  - operation: shrink\n    type: log\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "invalid on_failure",
			yaml:    "on_failure: kontinue\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "ignore_blocked_sessions empty entry",
			yaml:    "ignore_blocked_sessions:\n  - {}\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "ignore_blocked_sessions bad regexp",
			yaml:    "ignore_blocked_sessions:\n  - app_name: \"(\"\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n",
			wantErr: ddl.ErrInvalidManifest,
		},
		{
			name:    "ignore_blocked_sessions non-positive session_id",
			yaml:    "ignore_blocked_sessions:\n  - session_id: 0\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n",
			wantErr: ddl.ErrInvalidManifest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ddl.ParseManifest(strings.NewReader(tt.yaml))
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ParseManifest() error = %v, want errors.Is %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadShippedExampleManifest(t *testing.T) {
	// Shipped example is dot-prefixed so the runner treats it as disabled, but it
	// must still parse as a valid manifest.
	path := filepath.FromSlash("../../01.to_run/.010_example_rebuild.yaml")
	m, err := ddl.LoadManifestFile(path)
	if err != nil {
		t.Fatalf("LoadManifestFile(%q) error = %v, want nil", path, err)
	}
	if got, want := len(m.Operations), 2; got != want {
		t.Errorf("len(Operations) = %d, want %d", got, want)
	}
}
