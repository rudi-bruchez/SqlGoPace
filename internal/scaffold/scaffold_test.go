package scaffold

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// sources maps each embedded asset to the repository file it is a copy of. The
// embed directive cannot reach outside the package directory, so the templates
// exist twice; this table is what keeps the two copies honest.
var sources = map[string]string{
	"assets/config.yaml":              "../../config.yaml",
	"assets/ddl_compatibility.yaml":   "../../ddl_compatibility.yaml",
	"assets/maintenance_profile.yaml": "../../maintenance_profile.yaml",
	"assets/env.example":              "../../.env.example",
	"assets/example_manifest.yaml":    "../../01.to_run/.010_example_rebuild.yaml",
}

// TestAssetsMatchRepositoryFiles fails when a template is edited at the
// repository root without refreshing the embedded copy that `sqlgopace init`
// hands to users, which would otherwise drift silently for months.
func TestAssetsMatchRepositoryFiles(t *testing.T) {
	for asset, src := range sources {
		embedded, err := fs.ReadFile(assets, asset)
		if err != nil {
			t.Fatalf("read embedded %s: %v", asset, err)
		}
		onDisk, err := os.ReadFile(filepath.FromSlash(src))
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if string(embedded) != string(onDisk) {
			t.Errorf("%s differs from %s; copy the repository file over the embedded one", asset, src)
		}
	}
}

// TestFilesAllHaveAnAsset guards the table against a typo'd asset path, which
// would only surface when a user ran init.
func TestFilesAllHaveAnAsset(t *testing.T) {
	for _, f := range Files {
		if _, err := fs.ReadFile(assets, f.asset); err != nil {
			t.Errorf("%s: %v", f.Path, err)
		}
	}
}

func TestWriteCreatesEverything(t *testing.T) {
	dir := t.TempDir()

	got, err := Write(dir, false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want := len(Dirs) + len(Files); len(got) != want {
		t.Fatalf("got %d results, want %d", len(got), want)
	}
	for _, r := range got {
		if !r.Created {
			t.Errorf("%s reported as already existing in an empty directory", r.Path)
		}
		if _, err := os.Stat(filepath.Join(dir, r.Path)); err != nil {
			t.Errorf("%s: %v", r.Path, err)
		}
	}
}

// TestWriteDoesNotClobber is the property that matters most: re-running init in
// a configured directory must not overwrite an edited config.yaml.
func TestWriteDoesNotClobber(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, false); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	cfg := filepath.Join(dir, "config.yaml")
	const edited = "# mine\n"
	if err := os.WriteFile(cfg, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Write(dir, false)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	for _, r := range got {
		if r.Created {
			t.Errorf("%s reported as created on the second run", r.Path)
		}
	}
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != edited {
		t.Error("config.yaml was overwritten by a second init without --force")
	}
}

func TestWriteForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, false); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Write(dir, true); err != nil {
		t.Fatalf("forced Write: %v", err)
	}
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "# mine\n" {
		t.Error("--force did not overwrite config.yaml")
	}
}

// TestExampleManifestIsDisabled pins the dot prefix: without it, the first run
// after an init would execute the example against a real server.
func TestExampleManifestIsDisabled(t *testing.T) {
	for _, f := range Files {
		if filepath.Dir(f.Path) != "01.to_run" {
			continue
		}
		if base := filepath.Base(f.Path); base[0] != '.' {
			t.Errorf("%s would be picked up by Discover; it must start with a dot", f.Path)
		}
	}
}

// TestEnvExampleIsPrivate pins the mode of the one scaffolded file that becomes a
// secret. getting-started.md tells the operator to `cp .env.example .env` and fill
// in DB_PASSWORD; under a default umask, copying a 0644 template produces a
// world-readable credentials file.
func TestEnvExampleIsPrivate(t *testing.T) {
	for _, f := range Files {
		if f.Path != ".env.example" {
			continue
		}
		if f.mode != 0o600 {
			t.Errorf(".env.example is scaffolded %#o, want 0600", f.mode)
		}
		return
	}
	t.Fatal(".env.example is no longer scaffolded; delete this test or follow the template")
}

// TestWriteAppliesTheDeclaredModes checks that the modes in Files reach the disk.
// Skipped on Windows, which ignores every bit but read-only.
func TestWriteAppliesTheDeclaredModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores file modes apart from the read-only bit")
	}
	dir := t.TempDir()
	if _, err := Write(dir, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, f := range Files {
		info, err := os.Stat(filepath.Join(dir, f.Path))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != f.mode {
			t.Errorf("%s written %#o, want %#o", f.Path, got, f.mode)
		}
	}
}
