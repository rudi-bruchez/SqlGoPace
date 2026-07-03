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
