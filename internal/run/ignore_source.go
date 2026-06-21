package run

import (
	"os"
	"sync"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// IgnoreSource provides the current set of ignored-session rules. It is re-read live
// during a run so a rule added to the manifest mid-run (by hand or by the TUI) takes
// effect on the next blocking poll — before the operation would abort — without a
// restart.
type IgnoreSource interface {
	Current() IgnoredSessions
}

// staticIgnore is an IgnoreSource over a fixed compiled matcher: no live reload, used
// in tests and when reload is disabled.
type staticIgnore struct{ rules IgnoredSessions }

func (s staticIgnore) Current() IgnoredSessions { return s.rules }

// manifestSource re-reads a manifest's ignore_blocked_sessions from disk on demand,
// so an exclusion added during the run is honored before the next reaction. It is
// mtime-gated — one os.Stat per call, re-parse only when the file changed — and keeps
// the last good matcher when the file is unreadable or being edited mid-write.
type manifestSource struct {
	path string
	mu   sync.Mutex
	mod  time.Time
	cur  IgnoredSessions
}

// newManifestSource starts from the rules already compiled at run start, recording the
// file's current mtime so the first change (not the initial state) triggers a re-read.
func newManifestSource(path string, initial IgnoredSessions) *manifestSource {
	s := &manifestSource{path: path, cur: initial}
	if info, err := os.Stat(path); err == nil {
		s.mod = info.ModTime()
	}
	return s
}

func (s *manifestSource) Current() IgnoredSessions {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Stat(s.path)
	if err != nil {
		return s.cur // file gone/unreadable: keep the last good matcher
	}
	mod := info.ModTime()
	if !mod.After(s.mod) {
		return s.cur // unchanged since last read
	}
	s.mod = mod
	m, err := ddl.LoadManifestFile(s.path)
	if err != nil {
		return s.cur // mid-edit / invalid: keep the last good matcher
	}
	rules, err := CompileIgnoredSessions(m.IgnoreBlockedSessions)
	if err != nil {
		return s.cur
	}
	s.cur = rules
	return s.cur
}

// currentRules returns src.Current(), treating a nil source as "no rules".
func currentRules(src IgnoreSource) IgnoredSessions {
	if src == nil {
		return nil
	}
	return src.Current()
}
