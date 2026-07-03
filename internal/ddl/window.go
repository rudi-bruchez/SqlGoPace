package ddl

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window restricts a manifest's operations to a recurring time window evaluated
// against the SQL Server's local wall clock (SYSDATETIME). Absent = no constraint.
type Window struct {
	Start string   `yaml:"start"` // "HH:MM", 24h, server local
	End   string   `yaml:"end"`   // "HH:MM", 24h, server local
	Days  []string `yaml:"days"`  // optional; Mon..Sun (case-insensitive); empty = every day
}

// weekdayNames maps lowercase 3-letter names to time.Weekday.
var weekdayNames = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

// parseHHMM parses "HH:MM" into minutes since midnight (0–1439).
func parseHHMM(s string) (int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("time %q is not HH:MM: %w", s, ErrInvalidManifest)
	}
	h, herr := strconv.Atoi(parts[0])
	m, merr := strconv.Atoi(parts[1])
	if herr != nil || merr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q is not a valid 24h HH:MM: %w", s, ErrInvalidManifest)
	}
	return h*60 + m, nil
}

// parseWeekday parses a case-insensitive 3-letter weekday name.
func parseWeekday(s string) (time.Weekday, error) {
	d, ok := weekdayNames[strings.ToLower(strings.TrimSpace(s))]
	if !ok {
		return 0, fmt.Errorf("unknown day %q (want Mon..Sun): %w", s, ErrInvalidManifest)
	}
	return d, nil
}

// Validate checks the window's times and day names, and rejects a zero-length
// (start == end) window as ambiguous.
func (w *Window) Validate() error {
	start, err := parseHHMM(w.Start)
	if err != nil {
		return err
	}
	end, err := parseHHMM(w.End)
	if err != nil {
		return err
	}
	if start == end {
		return fmt.Errorf("window start and end are equal (%q): %w", w.Start, ErrInvalidManifest)
	}
	for _, d := range w.Days {
		if _, err := parseWeekday(d); err != nil {
			return err
		}
	}
	return nil
}

// Contains reports whether server wall-clock time t falls inside the window.
// It is defensive: an unvalidated window (start==end or unparseable) returns false.
func (w Window) Contains(t time.Time) bool {
	start, serr := parseHHMM(w.Start)
	end, eerr := parseHHMM(w.End)
	if serr != nil || eerr != nil || start == end {
		return false
	}
	now := t.Hour()*60 + t.Minute()
	today := t.Weekday()
	yesterday := time.Weekday((int(today) + 6) % 7)

	dayAllowed := func(d time.Weekday) bool {
		if len(w.Days) == 0 {
			return true
		}
		for _, name := range w.Days {
			if wd, err := parseWeekday(name); err == nil && wd == d {
				return true
			}
		}
		return false
	}

	if start < end { // same-day window [start, end)
		return now >= start && now < end && dayAllowed(today)
	}
	// overnight window: [start, 24:00) opens today, [00:00, end) is yesterday's tail
	switch {
	case now >= start:
		return dayAllowed(today)
	case now < end:
		return dayAllowed(yesterday)
	default:
		return false
	}
}
