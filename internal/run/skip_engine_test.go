package run_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// fakeCompression is a CompressionReader returning a fixed per-partition compression.
type fakeCompression struct {
	parts []mssql.PartitionCompression
	err   error
	calls int
}

func (f *fakeCompression) IndexCompression(context.Context, string, string, string) ([]mssql.PartitionCompression, error) {
	f.calls++
	return f.parts, f.err
}

const skipCompressManifest = `
description: skip test
intent: compression
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
    data_compression: PAGE
`

func TestCompressionIntentSkipsSatisfiedRebuild(t *testing.T) {
	runner := &fakeOpRunner{}
	comp := &fakeCompression{parts: []mssql.PartitionCompression{{Partition: 1, Desc: "PAGE"}}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithCompressionReader(comp))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(skipCompressManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Done != 1 {
		t.Fatalf("Summary = %+v, want Done:1", sum)
	}
	if runner.calls != 0 {
		t.Errorf("runner ran %d times, want 0 (the rebuild was already at target)", runner.calls)
	}
	if comp.calls != 1 {
		t.Errorf("IndexCompression called %d times, want 1", comp.calls)
	}

	data, err := os.ReadFile(filepath.Join(dirs.Done, "010_a.yaml.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if out := string(data); !strings.Contains(out, "skipped: already PAGE") {
		t.Errorf("log missing skip line\n--- log ---\n%s", out)
	}
}

func TestCompressionIntentRunsWhenNotSatisfied(t *testing.T) {
	runner := &fakeOpRunner{}
	comp := &fakeCompression{parts: []mssql.PartitionCompression{{Partition: 1, Desc: "NONE"}}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithCompressionReader(comp))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(skipCompressManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("runner ran %d times, want 1 (current NONE != target PAGE)", runner.calls)
	}
}

func TestSkipUnsetIntentRunsEvenWhenSatisfied(t *testing.T) {
	// No intent → unknown → runs, even at target. Skipping is opt-in via intent: compression.
	runner := &fakeOpRunner{}
	comp := &fakeCompression{parts: []mssql.PartitionCompression{{Partition: 1, Desc: "PAGE"}}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithCompressionReader(comp))
	manifest := strings.Replace(skipCompressManifest, "intent: compression\n", "", 1)
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("runner ran %d times, want 1 (unset intent runs)", runner.calls)
	}
}

func TestFragmentationIntentRunsEvenWhenSatisfied(t *testing.T) {
	// The regression this feature exists to prevent: a rebuild whose compression already
	// holds but whose intent is fragmentation MUST run — the defrag still needs doing.
	runner := &fakeOpRunner{}
	comp := &fakeCompression{parts: []mssql.PartitionCompression{{Partition: 1, Desc: "PAGE"}}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithCompressionReader(comp))
	manifest := strings.Replace(skipCompressManifest, "intent: compression\n", "intent: fragmentation\n", 1)
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("runner ran %d times, want 1 (fragmentation intent must run despite matching compression)", runner.calls)
	}
}

func TestOperationIntentBeatsManifestDefault(t *testing.T) {
	// Manifest default is compression, but the operation overrides to fragmentation → runs.
	runner := &fakeOpRunner{}
	comp := &fakeCompression{parts: []mssql.PartitionCompression{{Partition: 1, Desc: "PAGE"}}}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithCompressionReader(comp))
	manifest := strings.Replace(skipCompressManifest,
		"    data_compression: PAGE\n",
		"    data_compression: PAGE\n    intent: fragmentation\n", 1)
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("runner ran %d times, want 1 (operation intent overrides the manifest default)", runner.calls)
	}
}
