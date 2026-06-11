// Package run is the orchestration engine: it discovers manifests, moves them
// through the lifecycle directories, runs each operation against SQL Server, and
// drives the reaction state machine (pause/cancel/kill) under monitoring. The
// DB-touching work is reached through narrow interfaces so the engine is tested
// deterministically against fakes.
package run

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Dirs are the four lifecycle directories a manifest moves through.
type Dirs struct {
	ToRun      string
	Processing string
	Done       string
	Failed     string
}

// Queue manages the manifest lifecycle on disk.
type Queue struct {
	dirs Dirs
}

// NewQueue returns a Queue over the given directories.
func NewQueue(d Dirs) *Queue { return &Queue{dirs: d} }

// EnsureDirs creates the lifecycle directories if they do not exist.
func (q *Queue) EnsureDirs() error {
	for _, dir := range []string{q.dirs.ToRun, q.dirs.Processing, q.dirs.Done, q.dirs.Failed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return nil
}

// Discover returns the manifest file names waiting in to_run, sorted so that the
// numeric prefix convention (010_, 020_) defines execution order.
func (q *Queue) Discover() ([]string, error) {
	entries, err := os.ReadDir(q.dirs.ToRun)
	if err != nil {
		return nil, fmt.Errorf("read to_run: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !isManifest(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// isManifest reports whether a file name is a manifest to process. Files whose
// name starts with a dot are skipped, so a manifest can be disabled by prefixing
// it with "." and editor/OS dotfiles (.gitkeep, .DS_Store) are never picked up.
func isManifest(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

// Claim moves a manifest from to_run to processing and returns its new path.
func (q *Queue) Claim(name string) (string, error) {
	return q.move(name, q.dirs.ToRun, q.dirs.Processing)
}

// Complete moves a manifest from processing to done.
func (q *Queue) Complete(name string) error {
	_, err := q.move(name, q.dirs.Processing, q.dirs.Done)
	return err
}

// Fail moves a manifest from processing to failed.
func (q *Queue) Fail(name string) error {
	_, err := q.move(name, q.dirs.Processing, q.dirs.Failed)
	return err
}

// Requeue moves a manifest from processing back to to_run, e.g. during recovery.
func (q *Queue) Requeue(name string) error {
	_, err := q.move(name, q.dirs.Processing, q.dirs.ToRun)
	return err
}

func (q *Queue) move(name, from, to string) (string, error) {
	dst := filepath.Join(to, name)
	if err := os.Rename(filepath.Join(from, name), dst); err != nil {
		return "", fmt.Errorf("move %s from %s to %s: %w", name, filepath.Base(from), filepath.Base(to), err)
	}
	return dst, nil
}
