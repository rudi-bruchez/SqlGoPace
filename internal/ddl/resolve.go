package ddl

import "strconv"

// Target carries the resolution-relevant facts about the destination server.
type Target struct {
	MajorVersion int
	Tier         Tier
}

// Policy holds the global option settings sourced from config.yaml.
// A nil *bool means "auto" (decide from the compatibility matrix).
type Policy struct {
	Online            *bool
	Resumable         *bool
	WaitAtLowPriority *bool
	SortInTempDB      *bool
	MaxDOP            *int

	// AllowAbortBlockers permits ABORT_AFTER_WAIT = BLOCKERS (dangerous; kills
	// blocking user queries). Off by default → ABORT_AFTER_WAIT = SELF.
	AllowAbortBlockers bool
	// WaitMaxDurationMinutes is MAX_DURATION for WAIT_AT_LOW_PRIORITY (default 1).
	WaitMaxDurationMinutes int
}

// ResolvedOptions is the concrete set of options to inject into the T-SQL.
type ResolvedOptions struct {
	Online            bool
	Resumable         bool
	WaitAtLowPriority bool
	SortInTempDB      bool
	MaxDOP            *int

	// AbortAfterWait and MaxDurationMinutes are only meaningful when
	// WaitAtLowPriority is true.
	AbortAfterWait     string // "SELF" | "BLOCKERS"
	MaxDurationMinutes int
}

// Decision records why one option was set the way it was, for --explain output.
type Decision struct {
	Option string
	Value  string // "ON" | "OFF" | a MAXDOP number
	Reason string
}

// Resolve decides which options to inject for op on target, applying the
// precedence per-op override > config override > auto (matrix), the safety vs
// tuning defaults, and the RESUMABLE/WALP ⇒ ONLINE dependencies. It returns the
// resolved options and an explanation trail.
func Resolve(op Operation, t Target, m *Matrix, p Policy) (ResolvedOptions, []Decision) {
	cmd := op.CommandType()
	ov := overridesOf(op)

	app := func(option string) bool {
		return m.Applicable(t.MajorVersion, t.Tier, cmd, option)
	}

	// Safety options default ON when supported; tuning options are opt-in.
	onlineApp := app("online")
	online, onlineReason, onlineRel, onlineOverridden := pickBool(onlineApp, true, ov.Online, p.Online)
	resumable, resReason, resRel, _ := pickBool(app("resumable"), true, ov.Resumable, p.Resumable)
	walp, walpReason, walpRel, _ := pickBool(app("wait_at_low_priority"), true, ov.WaitAtLowPriority, p.WaitAtLowPriority)
	sort, sortReason, sortRel, _ := pickBool(app("sort_in_tempdb"), false, ov.SortInTempDB, p.SortInTempDB)

	// RESUMABLE requires ONLINE. Honor an explicit "online off"; otherwise turn
	// ONLINE on to satisfy the dependency when the target supports it.
	if resumable && !online {
		if !onlineOverridden && onlineApp {
			online, onlineReason, onlineRel = true, "forced on: required by resumable", true
		} else {
			resumable, resReason = false, "omitted: requires online, which is off"
		}
	}
	// WAIT_AT_LOW_PRIORITY also requires ONLINE.
	if walp && !online {
		if !onlineOverridden && onlineApp {
			online, onlineReason, onlineRel = true, "forced on: required by wait_at_low_priority", true
		} else {
			walp, walpReason = false, "omitted: requires online, which is off"
		}
	}

	res := ResolvedOptions{
		Online:            online,
		Resumable:         resumable,
		WaitAtLowPriority: walp,
		SortInTempDB:      sort,
	}
	if walp {
		res.AbortAfterWait = "SELF"
		if p.AllowAbortBlockers {
			res.AbortAfterWait = "BLOCKERS"
		}
		res.MaxDurationMinutes = p.WaitMaxDurationMinutes
		if res.MaxDurationMinutes <= 0 {
			res.MaxDurationMinutes = 1
		}
	}
	if app("maxdop") {
		if md := firstInt(ov.MaxDOP, p.MaxDOP); md != nil {
			v := *md
			res.MaxDOP = &v
		}
	}

	// Build the decision trail in a stable order.
	var decisions []Decision
	add := func(rel bool, option, value, reason string) {
		if rel {
			decisions = append(decisions, Decision{Option: option, Value: value, Reason: reason})
		}
	}
	add(onlineRel, "online", onOff(online), onlineReason)
	add(resRel, "resumable", onOff(resumable), resReason)
	add(walpRel, "wait_at_low_priority", onOff(walp), walpReason)
	add(sortRel, "sort_in_tempdb", onOff(sort), sortReason)
	if res.MaxDOP != nil {
		add(true, "maxdop", strconv.Itoa(*res.MaxDOP), "set by override")
	}

	return res, decisions
}

// pickBool resolves one boolean option. defaultOn selects the auto behaviour:
// safety options default to applicability, tuning options default to off.
// A forced-on but unsupported option is omitted. It returns the value, a reason,
// whether the option is worth explaining, and whether it came from an override.
func pickBool(applicable, defaultOn bool, perOp, global *bool) (value bool, reason string, relevant, overridden bool) {
	switch {
	case perOp != nil:
		if *perOp && !applicable {
			return false, "forced on per-operation but unsupported by target — omitted", true, true
		}
		return *perOp, "per-operation override", true, true
	case global != nil:
		if *global && !applicable {
			return false, "forced on by config but unsupported by target — omitted", true, true
		}
		return *global, "config override", true, true
	case defaultOn && applicable:
		return true, "supported by target (auto)", true, false
	default:
		return false, "", false, false
	}
}

// overridesOf extracts per-operation option overrides; operations without
// options return the zero value.
func overridesOf(op Operation) OptionOverrides {
	switch o := op.(type) {
	case RebuildIndex:
		return o.Options
	case CreateIndex:
		return o.Options
	case AlterColumn:
		return o.Options
	case AddConstraint:
		return o.Options
	default:
		return OptionOverrides{}
	}
}

func firstInt(values ...*int) *int {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}
