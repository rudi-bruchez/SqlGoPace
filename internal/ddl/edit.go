package ddl

import (
	"fmt"
	"regexp"

	"github.com/rudi-bruchez/SqlGoPace/internal/fsutil"
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
	if err := fsutil.AtomicWrite(path, data); err != nil {
		return fmt.Errorf("write manifest %q: %w", path, err)
	}
	return nil
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

// KilledSessionFor builds a single-criterion kill rule from an operator's choice, the
// inverse of IgnoredSessionFor: same criteria (session_id exact, app/host/login anchored
// literal regexp), with after=0 (kill on sight, since the operator explicitly authorized
// it). It returns ok=false for an unknown criterion, a non-positive SPID, or an empty value.
func KilledSessionFor(criterion, value string, spid int) (KilledSession, bool) {
	ig, ok := IgnoredSessionFor(criterion, value, spid)
	if !ok {
		return KilledSession{}, false
	}
	return KilledSession{IgnoredSession: ig}, true
}

// AppendKilledSession adds a kill rule to the manifest at path and writes it back
// atomically, mirroring AppendIgnoredSession, so the live reload never sees a torn file.
// An exact duplicate is a no-op. Comments in the original file are not preserved.
func AppendKilledSession(path string, s KilledSession) error {
	m, err := LoadManifestFile(path)
	if err != nil {
		return err
	}
	for _, e := range m.KillBlockedSessions {
		if sameKilledSession(e, s) {
			return nil // already present
		}
	}
	m.KillBlockedSessions = append(m.KillBlockedSessions, s)
	data, err := MarshalManifest(m)
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(path, data); err != nil {
		return fmt.Errorf("write manifest %q: %w", path, err)
	}
	return nil
}

// sameKilledSession reports whether two kill rules are field-for-field equal, including
// the delay.
func sameKilledSession(a, b KilledSession) bool {
	return sameIgnoredSession(a.IgnoredSession, b.IgnoredSession) && a.AfterSeconds == b.AfterSeconds
}

// RemoveKilledSession drops every kill rule field-equal to s from the manifest at path and
// writes it back atomically, the inverse of AppendKilledSession. Removing a rule that is not
// present is a no-op — the file is rewritten only when something actually changed, so an
// unrelated concurrent reader never sees a needless torn write.
func RemoveKilledSession(path string, s KilledSession) error {
	m, err := LoadManifestFile(path)
	if err != nil {
		return err
	}
	kept := make([]KilledSession, 0, len(m.KillBlockedSessions))
	removed := false
	for _, e := range m.KillBlockedSessions {
		if sameKilledSession(e, s) {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		return nil
	}
	m.KillBlockedSessions = kept
	data, err := MarshalManifest(m)
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(path, data); err != nil {
		return fmt.Errorf("write manifest %q: %w", path, err)
	}
	return nil
}
