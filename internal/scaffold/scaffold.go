// Package scaffold writes the files and directories a fresh installation needs.
//
// The templates are embedded so that a released binary is self-sufficient: the
// compatibility matrix is read from disk at run time, and without this a user who
// downloaded only the executable would have to fetch three YAML files by hand
// before the tool could do anything at all.
//
// The embedded copies under assets/ are byte-identical to the files at the
// repository root; scaffold_test.go fails if they drift apart.
package scaffold

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed assets
var assets embed.FS

// File is one scaffolded file: the path it is written to, relative to the target
// directory, the embedded asset it comes from, and the mode it is created with.
type File struct {
	Path  string
	asset string
	mode  fs.FileMode
}

// Files are written in this order, which is also the order they are reported in.
var Files = []File{
	{Path: "config.yaml", asset: "assets/config.yaml", mode: 0o644},
	{Path: "ddl_compatibility.yaml", asset: "assets/ddl_compatibility.yaml", mode: 0o644},
	{Path: "maintenance_profile.yaml", asset: "assets/maintenance_profile.yaml", mode: 0o644},
	// 0600, alone among these: getting-started.md says to `cp .env.example .env`
	// and fill in DB_PASSWORD, and cp carries the source mode, so a 0644 template
	// hands the operator a world-readable credentials file.
	{Path: ".env.example", asset: "assets/env.example", mode: 0o600},
	// The dot prefix is what keeps the example out of the run: Queue.Discover
	// skips manifests whose name starts with one. Renaming it arms it.
	{Path: filepath.Join("01.to_run", ".010_example_rebuild.yaml"), asset: "assets/example_manifest.yaml", mode: 0o644},
}

// Dirs are the queue lifecycle directories, matching the defaults in config.yaml.
// The engine creates them at run time too; creating them here means a fresh
// installation can be inspected before anything connects to a server.
var Dirs = []string{"01.to_run", "02.processing", "03.done", "04.failed"}

// Result records what happened to one path.
type Result struct {
	Path    string
	Created bool // false: it already existed and was left untouched
}

// Write creates Dirs and Files under dir. An existing file is left alone and
// reported as not created, unless force is set. Directories that already exist
// are never an error.
func Write(dir string, force bool) ([]Result, error) {
	var out []Result

	for _, d := range Dirs {
		p := filepath.Join(dir, d)
		_, err := os.Stat(p)
		existed := err == nil
		if err := os.MkdirAll(p, 0o755); err != nil {
			return out, fmt.Errorf("create directory %s: %w", p, err)
		}
		out = append(out, Result{Path: d, Created: !existed})
	}

	for _, f := range Files {
		created, err := writeFile(dir, f, force)
		if err != nil {
			return out, err
		}
		out = append(out, Result{Path: f.Path, Created: created})
	}
	return out, nil
}

// writeFile writes one asset, reporting whether it created it. It refuses to
// clobber an existing file unless force is set: a config.yaml that has been
// edited is the one thing in the directory that cannot be regenerated.
func writeFile(dir string, f File, force bool) (bool, error) {
	p := filepath.Join(dir, f.Path)
	if !force {
		if _, err := os.Stat(p); err == nil {
			return false, nil
		}
	}
	body, err := fs.ReadFile(assets, f.asset)
	if err != nil {
		return false, fmt.Errorf("read embedded %s: %w", f.asset, err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, fmt.Errorf("create directory for %s: %w", p, err)
	}
	if err := os.WriteFile(p, body, f.mode); err != nil {
		return false, fmt.Errorf("write %s: %w", p, err)
	}
	return true, nil
}
