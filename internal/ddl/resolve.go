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

	// IgnoreBlocking carries the per-operation reaction-policy override: when true,
	// the engine does not yield the operation when it blocks other sessions. It is
	// not a T-SQL option; it flows to the runner's Capabilities.
	IgnoreBlocking bool

	// MaxBlockMinutes is the per-operation safety cap (0 = none): after this many
	// minutes of continuous blocking, the operation yields even if the blocker is
	// ignored. Like IgnoreBlocking it flows to the runner's Capabilities.
	MaxBlockMinutes int
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

	// Shrink does not share the index/table option model: DBCC SHRINKFILE has no
	// ONLINE clause, so the "WALP requires ONLINE" dependency must never fire, and
	// online/resumable/sort_in_tempdb are not options here. Resolve WALP alone.
	if cmd == "shrink_data" || cmd == "shrink_log" {
		return resolveShrink(op, t, m, p)
	}

	// Batched DML shares none of the index/table WITH options: only MAXDOP (a query
	// hint) and the reaction-policy overrides apply. Resolve them directly.
	if cmd == "batch_update" || cmd == "batch_delete" {
		return resolveBatchDML(op, t, m, p)
	}

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

	// RESUMABLE is not supported with ALTER INDEX ALL: SQL Server requires a single
	// named index for a resumable rebuild. Drop it before the ONLINE dependency so
	// we do not force ONLINE on for an option we are about to remove.
	if resumable && IsAllIndexRebuild(op) {
		resumable, resReason, resRel = false, "omitted: RESUMABLE is not supported with ALTER INDEX ALL", true
	}

	// RESUMABLE is likewise not permitted when rebuilding a single partition: that
	// syntax accepts only SORT_IN_TEMPDB / MAXDOP / DATA_COMPRESSION / XML_COMPRESSION
	// plus ONLINE / WAIT_AT_LOW_PRIORITY. Drop it before the ONLINE dependency so we
	// do not force ONLINE on for an option we are about to remove.
	if resumable && isSinglePartitionRebuild(op) {
		resumable, resReason, resRel = false, "omitted: RESUMABLE is not supported when rebuilding a single partition", true
	}

	// Index-type gating: columnstore / XML / spatial indexes rebuild offline and
	// reject the ONLINE family. Applies only to expanded ALL rebuilds, where the
	// index kind is known; KindUnknown (single named index) is treated as rowstore.
	if ri, ok := op.(RebuildIndex); ok && !ri.Kind.supportsOnlineRebuild() {
		if online {
			online, onlineReason, onlineRel = false, "omitted: "+ri.Kind.String()+" index rebuilds offline", true
		}
		if resumable {
			resumable, resReason, resRel = false, "omitted: "+ri.Kind.String()+" index does not support resumable", true
		}
		if walp {
			walp, walpReason, walpRel = false, "omitted: "+ri.Kind.String()+" index does not support wait_at_low_priority", true
		}
	}

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
	res.IgnoreBlocking = ov.IgnoreBlocking != nil && *ov.IgnoreBlocking
	if ov.MaxBlockMinutes != nil && *ov.MaxBlockMinutes > 0 {
		res.MaxBlockMinutes = *ov.MaxBlockMinutes
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
	if res.IgnoreBlocking {
		add(true, "ignore_blocking", "ON", "set by override: hold the lock through blocking (reaction policy, not a WITH option)")
	}
	if res.MaxBlockMinutes > 0 {
		add(true, "max_block_minutes", strconv.Itoa(res.MaxBlockMinutes), "set by override: yield after this long even if the blocker is ignored (safety cap)")
	}

	return res, decisions
}

// resolveShrink resolves options for a DBCC SHRINKFILE operation. Only
// WAIT_AT_LOW_PRIORITY is injectable (2022+, data files only — gated by the
// matrix). There is no ONLINE clause for DBCC, so no "WALP requires ONLINE"
// dependency is applied and no MAX_DURATION is emitted (the engine fixes the
// shrink WALP timeout at ~1 minute). ABORT_AFTER_WAIT defaults to SELF and only
// becomes BLOCKERS when the global policy opts in.
func resolveShrink(op Operation, t Target, m *Matrix, p Policy) (ResolvedOptions, []Decision) {
	cmd := op.CommandType()
	ov := overridesOf(op)

	app := m.Applicable(t.MajorVersion, t.Tier, cmd, "wait_at_low_priority")
	walp, reason, relevant, _ := pickBool(app, true, ov.WaitAtLowPriority, p.WaitAtLowPriority)

	res := ResolvedOptions{WaitAtLowPriority: walp}
	if walp {
		res.AbortAfterWait = "SELF"
		if p.AllowAbortBlockers {
			res.AbortAfterWait = "BLOCKERS"
		}
		// No MAX_DURATION for shrink WALP: the engine uses a fixed ~1-minute wait.
	}

	var decisions []Decision
	if relevant {
		decisions = append(decisions, Decision{
			Option: "wait_at_low_priority", Value: onOff(walp), Reason: reason,
		})
	}
	return res, decisions
}

// resolveBatchDML resolves options for a batch_update/batch_delete. Only MAXDOP (a
// query hint, gated by the matrix) is a T-SQL option; ignore_blocking and
// max_block_minutes are reaction-policy overrides that flow to the runner. There is
// no ONLINE/RESUMABLE/WALP family for DML, so none is considered.
func resolveBatchDML(op Operation, t Target, m *Matrix, p Policy) (ResolvedOptions, []Decision) {
	ov := overridesOf(op)
	var res ResolvedOptions
	var decisions []Decision

	if m.Applicable(t.MajorVersion, t.Tier, op.CommandType(), "maxdop") {
		if md := firstInt(ov.MaxDOP, p.MaxDOP); md != nil {
			v := *md
			res.MaxDOP = &v
			decisions = append(decisions, Decision{Option: "maxdop", Value: strconv.Itoa(v), Reason: "set by override"})
		}
	}

	res.IgnoreBlocking = ov.IgnoreBlocking != nil && *ov.IgnoreBlocking
	if ov.MaxBlockMinutes != nil && *ov.MaxBlockMinutes > 0 {
		res.MaxBlockMinutes = *ov.MaxBlockMinutes
	}
	if res.IgnoreBlocking {
		decisions = append(decisions, Decision{Option: "ignore_blocking", Value: "ON",
			Reason: "set by override: hold the lock through blocking (reaction policy, not a WITH option)"})
	}
	if res.MaxBlockMinutes > 0 {
		decisions = append(decisions, Decision{Option: "max_block_minutes", Value: strconv.Itoa(res.MaxBlockMinutes),
			Reason: "set by override: yield after this long even if the blocker is ignored (safety cap)"})
	}
	return res, decisions
}

// pickBool resolves one boolean option. defaultOn selects the auto behavior:
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
	case RebuildHeap:
		return o.Options
	case CheckDB:
		return o.Options
	case CreateIndex:
		return o.Options
	case AlterColumn:
		return o.Options
	case AddConstraint:
		return o.Options
	case Shrink:
		return o.Options
	case BatchDML:
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
