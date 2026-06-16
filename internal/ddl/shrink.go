package ddl

import (
	"fmt"
	"strconv"
	"strings"
)

// TargetSpec is a parsed `targetfreespace` value: the desired free space left in
// the file after shrinking, expressed as a percentage of used space (Percent) or
// an absolute number of megabytes (AbsoluteMB). Exactly one is non-nil.
type TargetSpec struct {
	Percent    *int
	AbsoluteMB *int
}

// ParseTargetFreeSpace parses a raw `targetfreespace` string into a TargetSpec.
// Accepted forms: "10%" (percent of used space) and "100MB" / "100 MB" (absolute
// megabytes, case-insensitive). The value must be a positive integer. The empty
// string, non-positive values, and unknown units are rejected.
func ParseTargetFreeSpace(s string) (TargetSpec, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return TargetSpec{}, fmt.Errorf("targetfreespace is required: %w", ErrInvalidManifest)
	}

	switch {
	case strings.HasSuffix(t, "%"):
		n, err := positiveInt(strings.TrimSpace(strings.TrimSuffix(t, "%")))
		if err != nil {
			return TargetSpec{}, fmt.Errorf("targetfreespace %q: %w", s, err)
		}
		return TargetSpec{Percent: &n}, nil
	case hasUnitSuffix(t, "mb"):
		n, err := positiveInt(strings.TrimSpace(t[:len(t)-2]))
		if err != nil {
			return TargetSpec{}, fmt.Errorf("targetfreespace %q: %w", s, err)
		}
		return TargetSpec{AbsoluteMB: &n}, nil
	default:
		return TargetSpec{}, fmt.Errorf("targetfreespace %q: unknown unit, use \"%%\" or \"MB\": %w", s, ErrInvalidManifest)
	}
}

// hasUnitSuffix reports whether t ends with the given unit, case-insensitively.
func hasUnitSuffix(t, unit string) bool {
	return len(t) > len(unit) && strings.EqualFold(t[len(t)-len(unit):], unit)
}

// positiveInt parses a strictly positive integer, rejecting empty/negative/zero.
func positiveInt(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("not an integer: %w", ErrInvalidManifest)
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be > 0: %w", ErrInvalidManifest)
	}
	return n, nil
}

// FinalTargetMB computes the target file size in megabytes from the used space and
// a TargetSpec. Percent ⇒ ceil(used × (1 + N/100)); AbsoluteMB ⇒ used + N. The
// result is clamped so it is never below the used-space floor: a file cannot be
// shrunk below the space it actually holds.
func FinalTargetMB(usedMB int, spec TargetSpec) int {
	var final int
	switch {
	case spec.Percent != nil:
		// ceil(used × (100 + N) / 100) using integer arithmetic.
		final = (usedMB*(100+*spec.Percent) + 99) / 100
	case spec.AbsoluteMB != nil:
		final = usedMB + *spec.AbsoluteMB
	default:
		final = usedMB
	}
	if final < usedMB {
		final = usedMB
	}
	return final
}
