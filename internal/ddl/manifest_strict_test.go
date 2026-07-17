package ddl_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// A misspelled key used to be dropped in silence. For data_compression that is the worst
// possible failure: the rebuild runs with no compression at all and reports success, so a
// compression campaign looks like it worked. Every other config surface in the project
// (config.yaml, the profile, the matrix) already decodes with KnownFields(true); manifests
// did not.
func TestParseManifestRejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		wantIn   string // the error must name the offending key, so the operator can find it
		wantLine int    // …and point at its line in the FILE, not in any internal buffer
	}{
		{
			name: "misspelled data_compression on an operation",
			manifest: `
description: typo
operations:
  - operation: rebuild_index
    schema: dbo
    table: T1
    index: IX1
    data_compresion: PAGE
`,
			wantIn:   "data_compresion",
			wantLine: 8,
		},
		{
			name: "unknown top-level manifest key",
			manifest: `
description: typo
databse: MYDB
operations:
  - operation: rebuild_index
    schema: dbo
    table: T1
    index: IX1
`,
			wantIn:   "databse",
			wantLine: 3,
		},
		{
			name: "unknown key nested under options",
			manifest: `
description: typo
operations:
  - operation: rebuild_index
    schema: dbo
    table: T1
    index: IX1
    options:
      maxdopp: 4
`,
			wantIn:   "maxdopp",
			wantLine: 9,
		},
		{
			name: "unknown key on a shrink operation",
			manifest: `
description: typo
operations:
  - operation: shrink
    type: data
    files: all
    targetfreespac: 10%
`,
			wantIn:   "targetfreespac",
			wantLine: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ddl.ParseManifest(strings.NewReader(tt.manifest))
			if err == nil {
				t.Fatalf("ParseManifest() error = nil, want an error naming %q", tt.wantIn)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error does not name the offending key %q, so the operator cannot find it:\n%v", tt.wantIn, err)
			}
			if !errors.Is(err, ddl.ErrInvalidManifest) {
				t.Errorf("error is not an ErrInvalidManifest: %v", err)
			}
			// yaml.v3 names the Go type it was decoding into — for a manifest that is an
			// anonymous struct dump. An operator cannot act on it.
			if strings.Contains(err.Error(), "not found in type") || strings.Contains(err.Error(), "yaml:\\\"") {
				t.Errorf("error leaks the Go type instead of naming the field:\n%v", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("line %d", tt.wantLine)) {
				t.Errorf("error does not point at source line %d (where the typo is):\n%v", tt.wantLine, err)
			}
		})
	}
}

// The discriminator is present on every operation node but is not a field of any operation
// type, so a strict decode must not trip over it.
func TestParseManifestAcceptsValidManifests(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
	}{
		{
			name: "rebuild_index with options and a manifest-level ignore rule",
			manifest: `
description: valid
database: PRODDB
on_failure: continue
ignore_blocked_sessions:
  - login_name: svc_reporting
operations:
  - operation: rebuild_index
    schema: dbo
    table: T1
    index: IX1
    data_compression: PAGE
    options:
      maxdop: 2
`,
		},
		{
			name: "shrink",
			manifest: `
description: valid
operations:
  - operation: shrink
    type: data
    files: all
    targetfreespace: 10%
`,
		},
		{
			name: "window",
			manifest: `
description: valid
window:
  start: "01:00"
  end: "05:00"
  days: [Sat, Sun]
operations:
  - operation: rebuild_index
    schema: dbo
    table: T1
    index: IX1
`,
		},
		{
			name: "batch_update carries its verb in the discriminator",
			manifest: `
description: valid
operations:
  - operation: batch_update
    schema: dbo
    table: T1
    set:
      status: 1
    where_raw: "status IS NULL"
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ddl.ParseManifest(strings.NewReader(tt.manifest)); err != nil {
				t.Fatalf("ParseManifest() error = %v, want nil — this manifest is valid", err)
			}
		})
	}
}
