package ddl

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// IgnoredSessionFor builds a single-criterion ignore rule, used by the TUI when the
// operator picks a blocked session to ignore. "session_id" matches the SPID exactly;
// "app_name" / "host_name" / "login_name" become an anchored literal regexp on the
// observed value, so "ignore this app/login/host" matches exactly that value (the
// operator can later loosen it by hand). It returns ok=false for an unknown criterion,
// a non-positive SPID, or an empty value on a string criterion.
func IgnoredSessionFor(criterion, value string, spid int) (IgnoredSession, bool) {
	switch criterion {
	case "session_id":
		if spid <= 0 {
			return IgnoredSession{}, false
		}
		s := spid
		return IgnoredSession{SessionID: &s}, true
	case "app_name":
		return literalRule(func(re string) IgnoredSession { return IgnoredSession{AppName: re} }, value)
	case "host_name":
		return literalRule(func(re string) IgnoredSession { return IgnoredSession{HostName: re} }, value)
	case "login_name":
		return literalRule(func(re string) IgnoredSession { return IgnoredSession{LoginName: re} }, value)
	default:
		return IgnoredSession{}, false
	}
}

// literalRule builds a rule from an anchored literal regexp of value, or ok=false when
// value is empty.
func literalRule(build func(re string) IgnoredSession, value string) (IgnoredSession, bool) {
	if value == "" {
		return IgnoredSession{}, false
	}
	return build("^" + regexp.QuoteMeta(value) + "$"), true
}

// AppendIgnoredSession adds an ignore rule to the manifest at path and writes it back
// atomically (temp file + rename), so a concurrent reader (the live reload) never sees
// a torn file. An exact duplicate rule is a no-op. The manifest is re-rendered via
// MarshalManifest, so any comments in the original file are not preserved.
func AppendIgnoredSession(path string, s IgnoredSession) error {
	m, err := LoadManifestFile(path)
	if err != nil {
		return err
	}
	for _, e := range m.IgnoreBlockedSessions {
		if sameIgnoredSession(e, s) {
			return nil // already present
		}
	}
	m.IgnoreBlockedSessions = append(m.IgnoreBlockedSessions, s)
	data, err := MarshalManifest(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data)
}

// sameIgnoredSession reports whether two rules are field-for-field equal (the *int
// SessionID is compared by value, not pointer).
func sameIgnoredSession(a, b IgnoredSession) bool {
	if (a.SessionID == nil) != (b.SessionID == nil) {
		return false
	}
	if a.SessionID != nil && *a.SessionID != *b.SessionID {
		return false
	}
	return a.AppName == b.AppName && a.HostName == b.HostName &&
		a.LoginName == b.LoginName && a.Statement == b.Statement
}

// atomicWriteFile writes data to a temp file in the same directory and renames it over
// path, so readers see either the old or the new complete file. os.Rename replaces the
// destination on both POSIX and Windows.
func atomicWriteFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sqlgopace-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp manifest: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("write temp manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close temp manifest: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("replace manifest: %w", err)
	}
	return nil
}
