package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestSuspensionTracker(t *testing.T) {
	tr := newSuspensionTracker()
	t0 := time.Unix(0, 0)
	// not blocked → blocked by 104 for two polls (10s..30s) → free → blocked by 88 (40s..50s).
	tr.observe(false, 0, "", "", t0)
	tr.observe(true, 104, "SVC_OBS", "APPSRV01", t0.Add(10*time.Second))
	tr.observe(true, 104, "SVC_OBS", "APPSRV01", t0.Add(20*time.Second)) // accrues 10s to 104
	tr.observe(false, 0, "", "", t0.Add(30*time.Second))                 // accrues another 10s to 104
	tr.observe(true, 88, "SVC_X", "APPSRV02", t0.Add(40*time.Second))
	tr.observe(false, 0, "", "", t0.Add(50*time.Second)) // accrues 10s to 88

	snap := tr.snapshot()
	if snap.Episodes != 2 {
		t.Errorf("Episodes = %d, want 2", snap.Episodes)
	}
	if snap.TotalMS != (30 * time.Second).Milliseconds() {
		t.Errorf("TotalMS = %d, want 30000 (20s by 104 + 10s by 88)", snap.TotalMS)
	}
	if len(snap.Blockers) != 2 {
		t.Fatalf("Blockers = %d, want 2 (104 then 88, first-seen order)", len(snap.Blockers))
	}
	if b := snap.Blockers[0]; b.SPID != 104 || b.Count != 1 || b.TotalMS != 20000 || b.Login != "SVC_OBS" {
		t.Errorf("Blockers[0] = %+v, want SPID 104 count 1 20000ms SVC_OBS", b)
	}
	if b := snap.Blockers[1]; b.SPID != 88 || b.Count != 1 || b.TotalMS != 10000 {
		t.Errorf("Blockers[1] = %+v, want SPID 88 count 1 10000ms", b)
	}
}

func TestSuspensionTrackerCountsRepeatBlocker(t *testing.T) {
	// The same session blocking us in two separate episodes counts twice.
	tr := newSuspensionTracker()
	t0 := time.Unix(0, 0)
	tr.observe(true, 104, "SVC_OBS", "APPSRV01", t0)
	tr.observe(false, 0, "", "", t0.Add(10*time.Second))
	tr.observe(true, 104, "SVC_OBS", "APPSRV01", t0.Add(20*time.Second))
	tr.observe(false, 0, "", "", t0.Add(30*time.Second))

	snap := tr.snapshot()
	if snap.Episodes != 2 {
		t.Errorf("Episodes = %d, want 2", snap.Episodes)
	}
	if len(snap.Blockers) != 1 || snap.Blockers[0].Count != 2 {
		t.Errorf("Blockers = %+v, want a single SPID 104 with Count 2", snap.Blockers)
	}
}

func TestSuspensionTrackerCapturesHost(t *testing.T) {
	tr := newSuspensionTracker()
	t0 := time.Unix(0, 0)
	tr.observe(true, 104, "SVC_OBS", "APPSRV01", t0)
	tr.observe(false, 0, "", "", t0.Add(10*time.Second))

	snap := tr.snapshot()
	if len(snap.Blockers) != 1 {
		t.Fatalf("Blockers = %d, want 1", len(snap.Blockers))
	}
	if b := snap.Blockers[0]; b.Host != "APPSRV01" {
		t.Errorf("Blockers[0].Host = %q, want APPSRV01", b.Host)
	}
}

func TestQueuedDatabases(t *testing.T) {
	dir := t.TempDir()
	write := func(name, database string) {
		body := "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
		if database != "" {
			body = "database: " + database + "\n" + body
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("010_a.yaml", "")     // no database → the connected database
	write("020_b.yaml", "DB2")  // explicit
	write("030_c.yaml", "db2")  // same database, different case
	write("040_d.yaml", "CONN") // equals the connected database
	_ = os.WriteFile(filepath.Join(dir, ".hidden.yaml"), []byte("ignored"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)

	got, err := queuedDatabases(dir, "CONN")
	if err != nil {
		t.Fatalf("queuedDatabases() error = %v", err)
	}
	if diff := cmp.Diff([]string{"CONN", "DB2"}, got); diff != "" {
		t.Errorf("queuedDatabases mismatch (-want +got):\n%s", diff)
	}

	// A missing directory is not an error (nothing queued yet).
	if dbs, err := queuedDatabases(filepath.Join(dir, "nope"), "CONN"); err != nil || dbs != nil {
		t.Errorf("queuedDatabases(missing) = (%v, %v), want (nil, nil)", dbs, err)
	}
}

const (
	exampleManifest = "../../01.to_run/.010_example_rebuild.yaml"
	matrixFlag      = "--matrix=../../ddl_compatibility.yaml"
)

func TestProgressMsgForwardVsRollback(t *testing.T) {
	fwd := progressMsg(mssql.Progress{PercentComplete: 30, EstimatedCompletionMS: 5000, Command: "ALTER INDEX"})
	if fwd.Percent != 30 || fwd.RollbackPercent != 0 || fwd.ETASeconds != 5 {
		t.Errorf("forward progress = %+v, want Percent=30 RollbackPercent=0 ETASeconds=5", fwd)
	}

	rb := progressMsg(mssql.Progress{PercentComplete: 60, Command: "KILLED/ROLLBACK"})
	if rb.RollbackPercent != 60 || rb.Percent != 0 {
		t.Errorf("rollback progress = %+v, want RollbackPercent=60 Percent=0", rb)
	}
}

func TestSpaceMsgSumsDataFilesAcrossFilegroups(t *testing.T) {
	files := []mssql.FileSpace{
		{Name: "PRODDB_Data1", SizeMB: 500_000, UsedMB: 450_000, FreeMB: 50_000},
		{Name: "PRODDB_Data2", SizeMB: 300_000, UsedMB: 270_000, FreeMB: 30_000},
	}
	logSpace := mssql.LogSpace{TotalBytes: 64 * 1024 * 1024 * 1024, UsedPercent: 37}
	msg := spaceMsg(files, logSpace, "LOG_BACKUP", false)

	if msg.DataMB != 800_000 || msg.DataFreeMB != 80_000 {
		t.Errorf("spaceMsg data = MB:%d FreeMB:%d, want 800000/80000", msg.DataMB, msg.DataFreeMB)
	}
	if msg.LogBytes != logSpace.TotalBytes || msg.LogUsedPercent != 37 || msg.ReuseWait != "LOG_BACKUP" {
		t.Errorf("spaceMsg log fields = %+v, want the passed-through log space and reuse wait", msg)
	}
	if msg.LogAlert {
		t.Errorf("spaceMsg LogAlert = true, want false (caller passed false)")
	}
}

func TestSpaceMsgCarriesTheCallerComputedAlertFlag(t *testing.T) {
	// The TUI is dumb about thresholds: spaceMsg just carries whatever the caller decided.
	msg := spaceMsg(nil, mssql.LogSpace{UsedPercent: 95}, "ACTIVE_TRANSACTION", true)
	if !msg.LogAlert {
		t.Errorf("spaceMsg LogAlert = false, want true (caller passed true)")
	}
}

func TestLogAlertMsgNamesPercentAndReuseWait(t *testing.T) {
	a := logAlertMsg(93.4, "LOG_BACKUP")
	if !strings.Contains(a.Title, "93") || !strings.Contains(a.Title, "LOG_BACKUP") {
		t.Errorf("logAlertMsg title = %q, want it to name the percent and the reuse wait", a.Title)
	}
}

func TestRunDryRunEnterprise2022(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--dry-run", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, exampleManifest}

	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(dry-run) error = %v, want nil", err)
	}

	got := out.String()
	wants := []string{
		"ALTER INDEX [IX_DISPATCH] ON [dbo].[DISPATCH] REBUILD WITH (ONLINE = ON",
		"MAXDOP = 4",
		"DATA_COMPRESSION = PAGE",
		"IF COL_LENGTH(N'[dbo].[DISPATCH]', N'PROCESSED') IS NULL",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("dry-run output missing %q\n--- output ---\n%s", w, got)
		}
	}
}

func TestRunDryRunStandardOmitsOnline(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--dry-run", "--assume-version=16", "--assume-edition=standard", matrixFlag, exampleManifest}

	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(dry-run standard) error = %v, want nil", err)
	}
	if strings.Contains(out.String(), "ONLINE = ON") {
		t.Errorf("Standard edition output should not inject ONLINE:\n%s", out.String())
	}
}

func TestRunDryRunWithConfigPolicy(t *testing.T) {
	var out bytes.Buffer
	// Offline target (assume flags) but policy comes from --config, which forces
	// ONLINE off; the matrix path is taken from the config file.
	args := []string{
		"--dry-run", "--assume-version=16", "--assume-edition=enterprise",
		"--config=testdata/config_force_online_off.yaml", exampleManifest,
	}

	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(config policy) error = %v, want nil", err)
	}
	if strings.Contains(out.String(), "ONLINE = ON") {
		t.Errorf("config forced ONLINE off, but output still injects it:\n%s", out.String())
	}
}

func TestRunExplain(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--dry-run", "--explain", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, exampleManifest}

	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(explain) error = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "online = ON") {
		t.Errorf("explain output missing option decision trail:\n%s", out.String())
	}
}

func TestRunExplainIgnoreBlockedSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "010_ig.yaml")
	body := "ignore_blocked_sessions:\n  - app_name: \"^SQLAgent\"\n    login_name: \"svc\"\n  - session_id: 142\n" +
		"operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	args := []string{"--dry-run", "--explain", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, path}
	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(explain) error = %v", err)
	}
	s := out.String()
	for _, want := range []string{"ignore_blocked_sessions", "app_name~^SQLAgent AND login_name~svc", "session_id=142"} {
		if !strings.Contains(s, want) {
			t.Errorf("explain output missing %q:\n%s", want, s)
		}
	}
}

func TestDryRunAnnotatesWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "win.yaml")
	const m = `
description: windowed
window:
  start: "01:00"
  end: "05:00"
  days: [Sat, Sun]
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`
	if err := os.WriteFile(path, []byte(m), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out bytes.Buffer
	args := []string{"--dry-run", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, path}
	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(dry-run) error = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "window 01:00–05:00") {
		t.Errorf("dry-run output missing window annotation:\n%s", out.String())
	}
}

// TestDryRunCancelOnlyLineCauses pins the three causes CANCEL-ONLY.md §1 requires, in
// order: (1) the resumable decision's own Reason when one was emitted (here, a
// per-operation override); (2) "resumable not supported by <tier> major <n>" when the
// matrix has a resumable entry for the command but no decision was emitted (Standard,
// no override — pickBool marks it not relevant); (3) "<command> has no RESUMABLE form"
// when the matrix has no resumable entry for the command at all (rebuild_heap, on any
// target).
func TestDryRunCancelOnlyLineCauses(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		yaml  string
		wants []string
	}{
		{
			name: "per-operation override",
			args: []string{"--assume-version=16", "--assume-edition=enterprise"},
			yaml: "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n" +
				"    options:\n      resumable: false\n",
			wants: []string{
				"reaction = cancel only (per-operation override): a cancel under pressure rolls back all work",
			},
		},
		{
			name: "resumable not supported by target",
			args: []string{"--assume-version=16", "--assume-edition=standard"},
			yaml: "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n",
			wants: []string{
				"reaction = cancel only (resumable not supported by standard major 16): a cancel under pressure rolls back all work",
			},
		},
		{
			name: "command has no RESUMABLE form",
			args: []string{"--assume-version=16", "--assume-edition=enterprise"},
			yaml: "operations:\n  - operation: rebuild_heap\n    schema: dbo\n    table: T\n",
			wants: []string{
				"reaction = cancel only (rebuild_heap has no RESUMABLE form): a cancel under pressure rolls back all work",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "m.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			args := append([]string{"--dry-run", matrixFlag}, tt.args...)
			args = append(args, path)
			if err := cli(&out, io.Discard, args); err != nil {
				t.Fatalf("run(dry-run) error = %v, want nil", err)
			}
			got := out.String()
			for _, w := range tt.wants {
				if !strings.Contains(got, w) {
					t.Errorf("dry-run output missing %q\n--- output ---\n%s", w, got)
				}
			}
			// The hazard line is a warning, not an explanation: it must appear even
			// without --explain (already the case above), and must not depend on it.
		})
	}
}

// TestDryRunCancelOnlyLineAbsent covers the negative side of §1: the hazard line must
// not appear for a resumable rebuild, a shrink, a batch DML, a reorganize, or an
// add_column — none of these roll back all their work on cancel.
func TestDryRunCancelOnlyLineAbsent(t *testing.T) {
	const yaml = `
operations:
  - operation: rebuild_index
    schema: dbo
    table: T1
    index: IX1
  - operation: shrink
    type: data
    targetfreespace: 10%
  - operation: batch_delete
    schema: dbo
    table: AuditLog
    where:
      - { column: CreatedAt, op: '<', value: '2024-01-01' }
  - operation: reorganize_index
    schema: dbo
    table: T2
    index: IX2
  - operation: add_column
    schema: dbo
    table: T3
    column: C
    type: BIT
`
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// Enterprise 2022: rebuild_index resolves Resumable=true here (auto, supported).
	args := []string{"--dry-run", "--assume-version=16", "--assume-edition=enterprise", matrixFlag, path}
	if err := cli(&out, io.Discard, args); err != nil {
		t.Fatalf("run(dry-run) error = %v, want nil", err)
	}
	if strings.Contains(out.String(), "reaction = cancel only") {
		t.Errorf("dry-run output has the cancel-only hazard line where none is expected:\n%s", out.String())
	}
}

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer
	if err := cli(&out, io.Discard, []string{"--version"}); err != nil {
		t.Fatalf("run(--version) error = %v, want nil", err)
	}
	if !strings.HasPrefix(out.String(), "sqlgopace ") {
		t.Errorf("run(--version) = %q, want it to start with 'sqlgopace '", out.String())
	}
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing version on non-azure", []string{"--dry-run", "--assume-edition=enterprise", matrixFlag, exampleManifest}},
		{"no manifests", []string{"--dry-run", "--assume-version=16", matrixFlag}},
		{"bad edition", []string{"--dry-run", "--assume-version=16", "--assume-edition=bogus", matrixFlag, exampleManifest}},
		{"missing matrix file", []string{"--dry-run", "--assume-version=16", "--matrix=does-not-exist.yaml", exampleManifest}},
		{"auto with dry-run is rejected", []string{"--auto", "--dry-run", matrixFlag}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := cli(io.Discard, io.Discard, tt.args); err == nil {
				t.Errorf("run(%v) error = nil, want non-nil", tt.args)
			}
		})
	}
}

// TestAmplifierDwellWarning covers review finding 6: a dwell longer than the blocking
// timeout suppresses the yield reaction for that victim for the whole dwell, and nothing
// on the config surface said so. It is a warning, not a rejection — a long dwell is a
// legitimate choice.
func TestAmplifierDwellWarning(t *testing.T) {
	cfgWith := func(enabled bool, afterSeconds, timeoutMinutes int) *config.Config {
		c := &config.Config{}
		c.KillAmplifyingMaintenance.Enabled = enabled
		c.KillAmplifyingMaintenance.AfterSeconds = afterSeconds
		c.Monitoring.BlockingTimeoutMinutes = timeoutMinutes
		return c
	}
	tests := []struct {
		name string
		cfg  *config.Config
		warn bool
	}{
		{"disabled", cfgWith(false, 600, 1), false},
		{"dwell under the timeout", cfgWith(true, 60, 5), false},
		{"dwell equal to the timeout", cfgWith(true, 300, 5), false},
		{"dwell over the timeout", cfgWith(true, 600, 1), true},
		{"default dwell over a one-minute timeout", cfgWith(true, 0, 1), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := amplifierDwellWarning(tc.cfg)
			if (got != "") != tc.warn {
				t.Fatalf("amplifierDwellWarning() = %q, want warning = %t", got, tc.warn)
			}
			if !tc.warn {
				return
			}
			for _, want := range []string{"after_seconds", "blocking_timeout_minutes", "max_block_minutes", "10m0s", "1m0s"} {
				if !strings.Contains(got, want) {
					t.Errorf("warning = %q, missing %q", got, want)
				}
			}
		})
	}
}

// killSpy records which session the console's kill key ended.
type killSpy struct {
	spid   int
	killed []int
}

func (k *killSpy) SPID() int                             { return k.spid }
func (k *killSpy) ExecDDL(context.Context, string) error { return nil }
func (k *killSpy) Kill(_ context.Context, spid int) error {
	k.killed = append(k.killed, spid)
	return nil
}

// The console's k key ends our own DDL session. SQL Server reuses session ids, so
// killing an id captured when the run started can name somebody else's session once
// the execution connection has been re-pinned. Read it at the moment of the kill.
func TestKillDDLUsesTheCurrentSessionID(t *testing.T) {
	spy := &killSpy{spid: 57}
	spy.spid = 88 // the execution connection was re-pinned onto session 88

	if err := killDDL(context.Background(), spy); err != nil {
		t.Fatalf("killDDL() error = %v", err)
	}

	if want := []int{88}; !cmp.Equal(spy.killed, want) {
		t.Errorf("killed %v, want %v — the kill must name the live session", spy.killed, want)
	}
}

// The console header names the session the operator will check in SSMS before pressing
// k. 0.33.0 made the kill read the live session id, because a repaired connection is a
// new session and SQL Server reuses the old id — but the header was still sent once, at
// startup. The two must not disagree about which session is ours.
func TestSPIDAnnouncerFollowsTheExecutionSession(t *testing.T) {
	var a spidAnnouncer

	msg, ok := a.observe(57)
	if !ok || msg.SPID != 57 {
		t.Fatalf("observe(57) = (%+v, %v), want the first session announced", msg, ok)
	}

	if _, ok := a.observe(57); ok {
		t.Error("observe(57) again announced a second time; an unchanged session has nothing to say")
	}

	msg, ok = a.observe(88)
	if !ok || msg.SPID != 88 {
		t.Errorf("observe(88) = (%+v, %v), want the re-pinned session announced", msg, ok)
	}
}

// TestDryRunHeapScopeLines: connected, the dry run lists what else the heap rebuild
// rewrites and warns about a disabled index; offline it says it cannot list them.
func TestDryRunHeapScopeLines(t *testing.T) {
	sizes := []mssql.StructureSize{
		{IndexID: 0, TypeDesc: "HEAP", UsedKB: 5 * 1024 * 1024},
		{IndexID: 2, Name: "IX_A", TypeDesc: "NONCLUSTERED", UsedKB: 2 * 1024 * 1024},
		{IndexID: 3, Name: "IX_OLD", TypeDesc: "NONCLUSTERED", Disabled: true},
	}
	got := strings.Join(heapScopeLines(sizes, false), "\n")
	for _, want := range []string{
		"also rebuilds 2 nonclustered index(es): IX_A 2.0 GB, IX_OLD 0 KB",
		"7.0 GB rewritten",
		"re-enables disabled index IX_OLD without its compression",
		"allow_reenable_disabled_indexes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run heap lines missing %q:\n%s", want, got)
		}
	}
	if allowed := strings.Join(heapScopeLines(sizes, true), "\n"); !strings.Contains(allowed, "allowed by allow_reenable_disabled_indexes") {
		t.Errorf("opted-in wording missing:\n%s", allowed)
	}
	if offline := strings.Join(heapScopeLines(nil, false), "\n"); !strings.Contains(offline, "not listed offline") {
		t.Errorf("offline wording missing:\n%s", offline)
	}
}

// TestDryRunHeapScopeLinesNoNonclusteredIndex: a heap alone (no nonclustered index)
// gets no extra line — there is nothing else the rebuild touches to report.
func TestDryRunHeapScopeLinesNoNonclusteredIndex(t *testing.T) {
	sizes := []mssql.StructureSize{
		{IndexID: 0, TypeDesc: "HEAP", UsedKB: 5 * 1024 * 1024},
	}
	if lines := heapScopeLines(sizes, false); len(lines) != 0 {
		t.Errorf("heapScopeLines() = %v, want no lines for a heap with no nonclustered index", lines)
	}
}

// fakeDrySizeReader answers TableStructureSizes with a fixed result, for heapScopes tests.
type fakeDrySizeReader struct {
	sizes []mssql.StructureSize
	err   error
}

func (f fakeDrySizeReader) TableStructureSizes(context.Context, string, string, *int) ([]mssql.StructureSize, error) {
	return f.sizes, f.err
}

// TestHeapScopesDistinguishesOfflineFromUnreadable pins H1 (both 2026-09-16 harm reviews): a
// failed or empty read while connected must not say "not listed offline" — that claims an
// offline run while online. The offline wording is reserved for the genuinely offline case, a
// nil size reader.
func TestHeapScopesDistinguishesOfflineFromUnreadable(t *testing.T) {
	planned := []ddl.PlannedOperation{{Operation: ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT"}}}

	offline := heapScopes(context.Background(), nil, planned)
	if got := strings.Join(offline[0], "\n"); !strings.Contains(got, "not listed offline") {
		t.Errorf("nil reader (offline) = %q, want the offline wording", got)
	}

	failed := heapScopes(context.Background(), fakeDrySizeReader{err: errors.New("permission denied")}, planned)
	got := strings.Join(failed[0], "\n")
	if strings.Contains(got, "not listed offline") {
		t.Errorf("failed read while connected = %q, must not claim an offline run", got)
	}
	if !strings.Contains(got, "could not be read") || !strings.Contains(got, "permission denied") {
		t.Errorf("failed read while connected = %q, want it to say the read could not be read and why", got)
	}

	zeroRows := heapScopes(context.Background(), fakeDrySizeReader{}, planned)
	got = strings.Join(zeroRows[0], "\n")
	if strings.Contains(got, "not listed offline") {
		t.Errorf("zero-row read while connected = %q, must not claim an offline run", got)
	}
	if !strings.Contains(got, "could not be read") {
		t.Errorf("zero-row read while connected = %q, want it to say the sizes could not be read", got)
	}
}
