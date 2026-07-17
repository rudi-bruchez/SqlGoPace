package ddl_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestParseRebuildIndexIntent(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want ddl.Intent
	}{
		{"unset", "", ""},
		{"compression", "    intent: compression\n", ddl.IntentCompression},
		{"fragmentation", "    intent: fragmentation\n", ddl.IntentFragmentation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n" + tt.yaml
			m, err := ddl.ParseManifest(strings.NewReader(src))
			if err != nil {
				t.Fatalf("ParseManifest() error = %v", err)
			}
			got := m.Operations[0].(ddl.RebuildIndex).Intent
			if got != tt.want {
				t.Errorf("Intent = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRebuildIndexRejectsUnknownIntent(t *testing.T) {
	src := "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n    intent: banana\n"
	_, err := ddl.ParseManifest(strings.NewReader(src))
	if err == nil {
		t.Fatal("ParseManifest() error = nil, want an invalid-intent error")
	}
	if !errors.Is(err, ddl.ErrInvalidManifest) {
		t.Errorf("error is not ErrInvalidManifest: %v", err)
	}
	if !strings.Contains(err.Error(), "banana") {
		t.Errorf("error does not name the offending value: %v", err)
	}
}

func TestParseManifestIntentDefault(t *testing.T) {
	src := "intent: compression\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
	m, err := ddl.ParseManifest(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}
	if m.Intent != ddl.IntentCompression {
		t.Errorf("Manifest.Intent = %q, want %q", m.Intent, ddl.IntentCompression)
	}
	// The default is NOT pushed into the operation: the op keeps its own empty intent.
	if got := m.Operations[0].(ddl.RebuildIndex).Intent; got != "" {
		t.Errorf("operation Intent = %q, want empty (default resolves at use, not at load)", got)
	}
}

func TestManifestRejectsUnknownIntent(t *testing.T) {
	_, err := ddl.ParseManifest(strings.NewReader("intent: banana\noperations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"))
	if err == nil {
		t.Fatal("ParseManifest() error = nil, want an invalid manifest-intent error")
	}
	if !strings.Contains(err.Error(), "banana") {
		t.Errorf("error does not name the offending value: %v", err)
	}
}
