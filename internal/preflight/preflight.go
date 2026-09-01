// Package preflight runs health and precondition checks before any DDL is
// executed. A failing report routes the manifest to the failed directory without
// a single lock having been taken. The check logic is pure; the server facts are
// gathered through the narrow Prober interface (satisfied by *mssql.Conn).
package preflight

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// Severity is the outcome level of a single check.
type Severity int

const (
	// Pass means the check is satisfied.
	Pass Severity = iota
	// Warn means a non-blocking concern was found.
	Warn
	// Fail means the manifest must not run.
	Fail
)

// String returns the uppercase label of the severity.
func (s Severity) String() string {
	switch s {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	default:
		return "UNKNOWN"
	}
}

// Check is the result of one preflight check.
type Check struct {
	Name     string
	Severity Severity
	Detail   string
}

// Report aggregates all checks for one manifest.
type Report struct {
	Checks []Check
}

func (r *Report) add(c Check) { r.Checks = append(r.Checks, c) }

// HasFailure reports whether any check failed.
func (r Report) HasFailure() bool {
	for _, c := range r.Checks {
		if c.Severity == Fail {
			return true
		}
	}
	return false
}

// HasWarning reports whether any check raised a warning.
func (r Report) HasWarning() bool {
	for _, c := range r.Checks {
		if c.Severity == Warn {
			return true
		}
	}
	return false
}

// OK reports whether the manifest may proceed (no failures).
func (r Report) OK() bool { return !r.HasFailure() }

// CheckServer verifies the engine edition is one SqlGoPace supports.
func CheckServer(info mssql.ServerInfo) Check {
	if !info.Supported() {
		return Check{"server", Fail, fmt.Sprintf("unsupported engine edition %d", info.EngineEdition)}
	}
	return Check{"server", Pass, fmt.Sprintf("tier=%s major=%d adr=%t recovery=%s",
		info.Tier(), info.MajorVersion, info.ADREnabled, info.RecoveryModel)}
}

// CheckLog verifies the transaction log is healthy before starting. The absolute
// used bytes is the primary signal; the percent (of the current file) is
// secondary. A non-NOTHING log_reuse_wait is surfaced as a warning.
func CheckLog(ls mssql.LogSpace, reuseWait string, maxBytes int64, maxPercent int) Check {
	switch used := ls.UsedBytes(); {
	case used >= maxBytes:
		return Check{"transaction log", Fail, fmt.Sprintf("log already uses %d bytes (cap %d)", used, maxBytes)}
	case int(ls.UsedPercent) >= maxPercent:
		return Check{"transaction log", Fail, fmt.Sprintf("log already at %.0f%% (cap %d%%)", ls.UsedPercent, maxPercent)}
	case reuseWait != "" && reuseWait != "NOTHING":
		return Check{"transaction log", Warn, fmt.Sprintf("log_reuse_wait = %s", reuseWait)}
	default:
		return Check{"transaction log", Pass, fmt.Sprintf("%.0f%% used, reuse_wait=%s", ls.UsedPercent, reuseWait)}
	}
}

// CheckBlocking warns when sessions are already blocked before we start, so we
// know the server is not in a clean state.
func CheckBlocking(sessions []mssql.Session) Check {
	blocked := 0
	for _, s := range sessions {
		if s.BlockingSPID > 0 {
			blocked++
		}
	}
	if blocked > 0 {
		return Check{"blocking", Warn, fmt.Sprintf("%d session(s) already blocked before start", blocked)}
	}
	return Check{"blocking", Pass, "no pre-existing blocking"}
}

// CheckDataFreeSpace verifies the database's data files have room to build a second copy
// of the object. A rebuild materializes the new index before dropping the old, so it needs
// roughly the object's own size free; running out mid-rebuild wastes the entire operation,
// which is exactly what preflight exists to prevent. needMB of 0 means the size could not
// be read, and an unknown size never fails a run.
// Free space inside the files is not the whole answer: a file that can still autogrow has
// headroom that counts too, and ignoring it fails runs that would have succeeded. Relying
// on growth is a warning rather than a pass, because the growth itself is a blocking
// zero-fill unless instant file initialization applies.
//
// DataSpace carries what is known about the data files. GrowthKnown is false when the
// autogrowth read failed, in which case a shortfall cannot be judged either way and must
// not fail the run.
type DataSpace struct {
	FreeMB      int
	Growth      []mssql.FileGrowth
	GrowthKnown bool
}

func CheckDataFreeSpace(target string, needMB int, sp DataSpace) Check {
	const name = "data free space"
	freeMB := sp.FreeMB
	switch {
	case needMB <= 0:
		return Check{name, Pass, fmt.Sprintf("%s: size unknown, not checked (%d MB free in data files)", target, freeMB)}
	case freeMB >= needMB:
		return Check{name, Pass, fmt.Sprintf("%s: %d MB free, ~%d MB needed", target, freeMB, needMB)}
	case !sp.GrowthKnown:
		return Check{name, Warn, fmt.Sprintf(
			"%s needs ~%d MB, data files have %d MB free, and their autogrowth could not be read — cannot tell whether it fits",
			target, needMB, freeMB)}
	}

	headroomMB := 0
	for _, f := range sp.Growth {
		// A file with no cap can grow until the disk fills, which the catalog cannot see.
		// We cannot prove the run will fail, so we must not fail it.
		if f.Unlimited() {
			return Check{name, Warn, fmt.Sprintf(
				"%s needs ~%d MB, data files have %d MB free; %q grows until the disk fills, so expect an autogrowth of ~%d MB",
				target, needMB, freeMB, f.Name, f.NextGrowthMB())}
		}
		if mb, ok := f.HeadroomMB(); ok {
			headroomMB += mb
		}
	}
	if headroomMB >= needMB-freeMB {
		return Check{name, Warn, fmt.Sprintf(
			"%s needs ~%d MB, data files have %d MB free; autogrowth can add %d MB more, so the rebuild will grow the files",
			target, needMB, freeMB, headroomMB)}
	}
	return Check{name, Fail, fmt.Sprintf(
		"%s needs ~%d MB free, data files have %d MB and can grow by only %d MB more",
		target, needMB, freeMB, headroomMB)}
}

// growthOf reads one file type's autogrowth, reporting ok=false rather than an error: every
// consumer of it is advisory, so a failed read degrades the advice instead of the run.
func growthOf(ctx context.Context, p Prober, fileType string) ([]mssql.FileGrowth, bool) {
	g, err := p.FileGrowths(ctx, fileType)
	if err != nil {
		return nil, false
	}
	return g, true
}

// rebuiltObject reports the object an operation rebuilds in place, and whether it is such
// an operation. Only a rebuild needs room for a second copy of something that already
// exists: create_index cannot be sized in advance (the index is not there yet) and is
// deliberately not checked, and the remaining operations are metadata-only.
//
// partition is carried through because `REBUILD PARTITION = n` rebuilds one partition and
// needs room for that partition alone. Sizing the whole index there would fail a
// partitioned rebuild of a large table that has ample room for the partition in hand.
func rebuiltObject(op ddl.Operation) (schema, table, index string, partition *int, ok bool) {
	switch o := op.(type) {
	case ddl.RebuildIndex:
		return o.Schema, o.Table, o.Index, o.Partition, true
	case ddl.RebuildHeap:
		return o.Schema, o.Table, "", nil, true // empty index = the heap itself
	default:
		return "", "", "", nil, false
	}
}

// rebuiltObjectLabel names the object for the check detail, distinguishing a heap (which
// has no index name) from a named index, and naming the partition when only one is rebuilt.
func rebuiltObjectLabel(schema, table, index string, partition *int) string {
	name := fmt.Sprintf("%s.%s.%s", schema, table, index)
	if index == "" {
		name = fmt.Sprintf("%s.%s (heap)", schema, table)
	}
	if partition != nil {
		name += fmt.Sprintf(" partition %d", *partition)
	}
	return name
}

// shrunkFileTypes reports which file types the manifest shrinks, keyed by the same
// type_desc the catalog uses. Disabled autogrowth only matters for a file the run is about
// to take space away from, and the two directions are not interchangeable: a log shrink on
// a log that cannot grow is how a database ends up refusing writes with error 9002, and no
// amount of data-file headroom helps. shrink_tempdb is excluded — tempdb is recreated from
// model at every restart, so its file settings do not persist.
func shrunkFileTypes(m *ddl.Manifest) map[string]bool {
	out := make(map[string]bool, 2)
	for _, op := range m.Operations {
		sh, ok := op.(ddl.Shrink)
		if !ok {
			continue
		}
		if sh.CommandType() == "shrink_log" {
			out[mssql.FileTypeLog] = true
		} else {
			out[mssql.FileTypeRows] = true
		}
	}
	return out
}

// requiresElevatedRights reports whether an operation needs db_owner or sysadmin:
// DBCC SHRINKFILE (shrink, shrink_tempdb) and DBCC CHECKDB (check_db) all do. Lesser
// DDL rights (db_ddladmin, ALTER on a table) are not enough for these.
func requiresElevatedRights(op ddl.Operation) bool {
	switch op.(type) {
	case ddl.CheckDB, ddl.Shrink, ddl.ShrinkTempdb:
		return true
	default:
		return false
	}
}

// needsElevatedRights reports whether any operation in the manifest requires
// db_owner / sysadmin, so the permission probe runs at most once per manifest.
func needsElevatedRights(m *ddl.Manifest) bool {
	return slices.ContainsFunc(m.Operations, requiresElevatedRights)
}

// CheckElevatedRights verifies the connected login can run the manifest's
// elevated-privilege operations (shrink / check_db). The grant requirement is
// surfaced explicitly so a missing membership is actionable, not an opaque
// execution-time DBCC error.
func CheckElevatedRights(hasAccess bool) Check {
	if !hasAccess {
		return Check{"permissions", Fail,
			"shrink/check_db require db_owner (in this database) or sysadmin; the connected login has neither"}
	}
	return Check{"permissions", Pass, "db_owner or sysadmin"}
}

// CheckTempdbShrinkRights verifies the login can shrink tempdb. db_owner in the
// connected database is not the right question and not enough: DBCC SHRINKFILE runs
// in tempdb, which is recreated from model at every restart, so a membership granted
// there does not survive one. In practice that leaves sysadmin.
func CheckTempdbShrinkRights(isSysadmin bool) Check {
	if !isSysadmin {
		return Check{"tempdb shrink permission", Fail,
			"shrink_tempdb runs DBCC SHRINKFILE in tempdb, which requires sysadmin; db_owner in the connected database is not enough"}
	}
	return Check{"tempdb shrink permission", Pass, "sysadmin"}
}

// CheckKillPermission warns (never fails) when the blocker-kill policy is armed but the
// connected login cannot KILL another session. A missing grant would make every auto-kill a
// silent no-op, so it is surfaced — but it does not block the run, which is otherwise valid.
func CheckKillPermission(hasPerm bool) Check {
	if !hasPerm {
		return Check{"kill permission", Warn,
			"blocker-killing is armed (kill_blockers or allow_abort_blockers) but the login lacks ALTER ANY CONNECTION (or sysadmin/processadmin); blocker kills will be no-ops"}
	}
	return Check{"kill permission", Pass, "ALTER ANY CONNECTION (or sysadmin/processadmin)"}
}

// CheckOperation verifies an operation's preconditions: the table must exist, and
// the target object must exist (for rebuild/alter/drop) or not exist (for
// create/add — already present means the idempotent guard will skip it).
func CheckOperation(op ddl.Operation, tableExists, targetExists bool) Check {
	ref := op.Target()

	// Database- and file-scoped operations (check_db, shrink, shrink_tempdb) have no
	// schema.table precondition; their target (database/file) is validated by the
	// engine at run time (DBCC resolves the file list / database itself).
	switch op.(type) {
	case ddl.CheckDB, ddl.Shrink, ddl.ShrinkTempdb:
		return Check{fmt.Sprintf("%s %s", op.CommandType(), ref), Pass, "no table precondition (database/file-scoped)"}
	}

	name := fmt.Sprintf("%s %s.%s", op.CommandType(), ref.Schema, ref.Table)

	if !tableExists {
		return Check{name, Fail, fmt.Sprintf("table [%s].[%s] does not exist", ref.Schema, ref.Table)}
	}

	switch op.(type) {
	case ddl.RebuildIndex, ddl.DropIndex, ddl.AlterColumn, ddl.DropColumn, ddl.DropConstraint:
		if !targetExists {
			return Check{name, Fail, fmt.Sprintf("%q does not exist", ref.Name)}
		}
	case ddl.CreateIndex, ddl.AddColumn, ddl.AddConstraint:
		if targetExists {
			return Check{name, Warn, fmt.Sprintf("%q already exists; operation will be skipped (idempotent)", ref.Name)}
		}
	}
	return Check{name, Pass, "preconditions satisfied"}
}

// Prober is the narrow set of server facts preflight needs. *mssql.Conn satisfies it.
type Prober interface {
	LogSpace(ctx context.Context) (mssql.LogSpace, error)
	LogReuseWait(ctx context.Context) (string, error)
	ActiveSessions(ctx context.Context) ([]mssql.Session, error)
	TableExists(ctx context.Context, schema, table string) (bool, error)
	IndexExists(ctx context.Context, schema, table, index string) (bool, error)
	ColumnExists(ctx context.Context, schema, table, column string) (bool, error)
	ConstraintExists(ctx context.Context, schema, table, constraint string) (bool, error)
	HasElevatedDBAccess(ctx context.Context) (bool, error)
	IsSysadmin(ctx context.Context) (bool, error)
	HasAlterAnyConnection(ctx context.Context) (bool, error)
	HasDMLPermission(ctx context.Context, schema, table, perm string) (bool, error)
	FileSpace(ctx context.Context, fileType string) ([]mssql.FileSpace, error)
	FileGrowths(ctx context.Context, fileType string) ([]mssql.FileGrowth, error)
	IndexSizeMB(ctx context.Context, schema, table, index string, partition *int) (int, error)
}

var _ Prober = (*mssql.Conn)(nil)

// Thresholds are the limits and toggles sourced from config.
type Thresholds struct {
	LogMaxBytes   int64
	LogMaxPercent int
	// RequireDataFreeSpace enables the data-free-space check: every index rebuild is
	// verified to have roughly its own size free in the database's data files before
	// the manifest runs. Off leaves the check out of the report entirely.
	RequireDataFreeSpace bool
}

// Run gathers server facts and builds the preflight report for a manifest. killArmed
// requests the ALTER ANY CONNECTION advisory (the blocker-kill policy or ABORT_AFTER_WAIT =
// BLOCKERS is enabled); it only ever warns, never fails.
func Run(ctx context.Context, p Prober, info mssql.ServerInfo, m *ddl.Manifest, th Thresholds, killArmed bool) (Report, error) {
	var rep Report
	rep.add(CheckServer(info))

	ls, err := p.LogSpace(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("preflight log space: %w", err)
	}
	reuseWait, err := p.LogReuseWait(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("preflight log reuse wait: %w", err)
	}
	rep.add(CheckLog(ls, reuseWait, th.LogMaxBytes, th.LogMaxPercent))

	sessions, err := p.ActiveSessions(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("preflight active sessions: %w", err)
	}
	rep.add(CheckBlocking(sessions))

	// Operations that issue DBCC (shrink, check_db) need db_owner or sysadmin.
	// Probe once if any is present, so a missing grant fails preflight with a clear
	// message rather than an opaque DBCC error at execution time.
	if needsElevatedRights(m) {
		access, err := p.HasElevatedDBAccess(ctx)
		if err != nil {
			return Report{}, fmt.Errorf("preflight elevated access: %w", err)
		}
		rep.add(CheckElevatedRights(access))
	}

	// shrink_tempdb is the one elevated operation the check above answers the wrong
	// question for: it probes db_owner in the connected database, while the DBCC runs
	// in tempdb. A db_owner of a user database passed, then failed mid-operation with
	// "User 'guest' does not have permission to run DBCC shrinkfile for database
	// 'tempdb'" (Msg 7983). Measured against SQL Server 2022 CU26.
	if slices.ContainsFunc(m.Operations, func(op ddl.Operation) bool {
		_, ok := op.(ddl.ShrinkTempdb)
		return ok
	}) {
		sa, err := p.IsSysadmin(ctx)
		if err != nil {
			return Report{}, fmt.Errorf("preflight sysadmin: %w", err)
		}
		rep.add(CheckTempdbShrinkRights(sa))
	}

	// When the blocker-kill policy is armed, advise (warn only) if the login cannot KILL.
	if killArmed {
		hasPerm, err := p.HasAlterAnyConnection(ctx)
		if err != nil {
			return Report{}, fmt.Errorf("preflight kill permission: %w", err)
		}
		rep.add(CheckKillPermission(hasPerm))
	}

	// A rebuild materializes a second copy of the object before dropping the original, so
	// the data files need roughly the object's own size free. The file read is done once
	// per manifest; the peak requirement is the largest single rebuild, not their sum,
	// because each one releases its temporary copy before the next begins.
	// Autogrowth is read unconditionally: it is one small catalog query, it advises on
	// every run rather than only when a rebuild is short of room, and the free-space check
	// below needs it to count headroom rather than fail conservatively.
	// The growth read is advisory and must never abort a manifest: sys.database_files is
	// readable with less than the documented VIEW SERVER STATE on some logins, and a check
	// that only ever warns cannot be allowed to fail a run through its own error path.
	dataGrowth, growthKnown := growthOf(ctx, p, mssql.FileTypeRows)
	logGrowth, logKnown := growthOf(ctx, p, mssql.FileTypeLog)
	if growthKnown && logKnown {
		rep.add(CheckFileGrowth(append(dataGrowth, logGrowth...), shrunkFileTypes(m)))
	} else {
		rep.add(Check{"file growth", Warn, "autogrowth settings could not be read; growth is not being advised on"})
	}

	dataFreeMB := 0
	if th.RequireDataFreeSpace {
		files, err := p.FileSpace(ctx, mssql.FileTypeRows)
		if err != nil {
			return Report{}, fmt.Errorf("preflight data file space: %w", err)
		}
		for _, f := range files {
			dataFreeMB += f.FreeMB
		}
	}

	for _, op := range m.Operations {
		if th.RequireDataFreeSpace {
			if schema, table, index, partition, ok := rebuiltObject(op); ok {
				// A size we cannot read is reported as unknown (0), never as a failed run:
				// sys.dm_db_partition_stats also wants VIEW DEFINITION, which the documented
				// VIEW SERVER STATE does not imply, so a legitimate login can be refused it.
				sizeMB, err := p.IndexSizeMB(ctx, schema, table, index, partition)
				if err != nil {
					sizeMB = 0
				}
				rep.add(CheckDataFreeSpace(rebuiltObjectLabel(schema, table, index, partition), sizeMB,
					DataSpace{FreeMB: dataFreeMB, Growth: dataGrowth, GrowthKnown: growthKnown}))
			}
		}

		tableExists, targetExists, err := objectExistence(ctx, p, op)
		if err != nil {
			return Report{}, err
		}
		rep.add(CheckOperation(op, tableExists, targetExists))

		// Batched DML needs UPDATE/DELETE permission on the table (a clear preflight
		// failure beats an opaque execution-time permission error), plus an RCSI
		// advisory on how lock escalation / the tempdb version store will behave.
		if b, ok := op.(ddl.BatchDML); ok {
			if tableExists {
				// SELECT is needed too, and unconditionally: every batch is an
				// UPDATE/DELETE TOP (n), the key_range walk reads the key with its own
				// SELECT MAX, and a predicate reads the columns it filters on. A login
				// with db_datawriter but not db_datareader passes the UPDATE check and
				// then fails mid-run with "The SELECT permission was denied", which is
				// the opaque error this check exists to pre-empt. Measured against
				// SQL Server 2022 CU26, with and without a where clause.
				for _, perm := range []string{dmlPermissionFor(b), "SELECT"} {
					has, err := p.HasDMLPermission(ctx, b.Schema, b.Table, perm)
					if err != nil {
						return Report{}, fmt.Errorf("preflight dml permission %s.%s: %w", b.Schema, b.Table, err)
					}
					rep.add(CheckBatchDMLPermission(b, perm, has))
				}
			}
			rep.add(CheckBatchDMLIsolation(info, b))
		}
	}
	return rep, nil
}

// dmlPermissionFor returns the object permission a batch operation needs.
func dmlPermissionFor(op ddl.BatchDML) string {
	if op.Verb == "delete" {
		return "DELETE"
	}
	return "UPDATE"
}

// CheckBatchDMLPermission verifies the connected login can run the batch operation's
// UPDATE/DELETE on the target table.
func CheckBatchDMLPermission(op ddl.BatchDML, perm string, hasAccess bool) Check {
	name := fmt.Sprintf("%s %s.%s permission", op.CommandType(), op.Schema, op.Table)
	if !hasAccess {
		return Check{name, Fail,
			fmt.Sprintf("the connected login lacks %s permission on [%s].[%s]", perm, op.Schema, op.Table)}
	}
	return Check{name, Pass, perm + " granted"}
}

// CheckBatchDMLIsolation is an advisory (never blocking) on how the database's
// snapshot-isolation state shapes a batched DML: with RCSI off, lock escalation to a
// table X lock blocks readers (so batches are capped small); with it on, readers are
// unaffected but the tempdb version store grows with each changed row.
func CheckBatchDMLIsolation(info mssql.ServerInfo, op ddl.BatchDML) Check {
	name := fmt.Sprintf("%s isolation advisory", op.CommandType())
	if info.RCSIEnabled || info.SnapshotIsolation {
		return Check{name, Warn,
			"RCSI/snapshot isolation is on: readers are not blocked by the batch, but each changed row adds a tempdb version-store entry — watch tempdb on a long run"}
	}
	return Check{name, Warn,
		"RCSI is off: a batch large enough to escalate to a table lock blocks readers; batch size is capped below the escalation threshold — keep batches small or enable RCSI"}
}

// objectExistence resolves whether the operation's table and target object exist.
// The target lookup is skipped when the table is absent (the check already fails).
func objectExistence(ctx context.Context, p Prober, op ddl.Operation) (table, target bool, err error) {
	// Database- and file-scoped operations (check_db, shrink, shrink_tempdb) have no
	// schema.table to verify; skip the lookup so CheckOperation can pass them through.
	switch op.(type) {
	case ddl.CheckDB, ddl.Shrink, ddl.ShrinkTempdb:
		return true, true, nil
	}

	ref := op.Target()
	table, err = p.TableExists(ctx, ref.Schema, ref.Table)
	if err != nil {
		return false, false, fmt.Errorf("check table %s.%s: %w", ref.Schema, ref.Table, err)
	}
	if !table {
		return false, false, nil
	}

	// ALTER INDEX ALL ... REBUILD has no single named index to verify; the table's
	// existence (checked above) is the precondition.
	if ddl.IsAllIndexRebuild(op) {
		return table, true, nil
	}

	switch op.(type) {
	case ddl.RebuildIndex, ddl.CreateIndex, ddl.DropIndex:
		target, err = p.IndexExists(ctx, ref.Schema, ref.Table, ref.Name)
	case ddl.AddColumn, ddl.AlterColumn, ddl.DropColumn:
		target, err = p.ColumnExists(ctx, ref.Schema, ref.Table, ref.Name)
	case ddl.AddConstraint, ddl.DropConstraint:
		target, err = p.ConstraintExists(ctx, ref.Schema, ref.Table, ref.Name)
	}
	if err != nil {
		return false, false, fmt.Errorf("check target %q: %w", ref.Name, err)
	}
	return table, target, nil
}

// CheckFileGrowth advises on autogrowth settings that will hurt later. It never fails a
// run: a bad growth setting is a reason to tell the operator, not a reason to refuse.
//
// Percentage growth is called out because the increment scales with the file, so it grows
// as the file does — Microsoft's guidance is to set a fixed number of megabytes instead.
// Growth is also a blocking operation that zero-fills the new space unless instant file
// initialization applies. For data files that needs the SE_MANAGE_VOLUME_NAME privilege; for
// log files it applies only from SQL Server 2022, and then only to growth events of 64 MB or
// less — which a percentage increment on a large log will exceed immediately.
//
// Disabled growth is only reported for a file type this manifest shrinks, where it is the
// dangerous combination: the shrink removes headroom the file will not be able to reclaim.
func CheckFileGrowth(files []mssql.FileGrowth, shrunk map[string]bool) Check {
	var notes []string
	for _, f := range files {
		switch {
		case f.IsPercent:
			notes = append(notes, fmt.Sprintf("%s grows by %d%% (~%d MB at its current size; prefer a fixed increment)",
				f.Name, f.Growth, f.NextGrowthMB()))
		case shrunk[f.TypeDesc] && f.GrowthDisabled():
			notes = append(notes, fmt.Sprintf("%s has autogrowth disabled; a shrink removes space it cannot reclaim", f.Name))
		}
	}
	if len(notes) > 0 {
		return Check{"file growth", Warn, strings.Join(notes, "; ")}
	}
	return Check{"file growth", Pass, fmt.Sprintf("%d file(s) with usable autogrowth settings", len(files))}
}
