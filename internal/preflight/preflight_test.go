package preflight_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
)

func TestCheckServer(t *testing.T) {
	if got := preflight.CheckServer(mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16}).Severity; got != preflight.Pass {
		t.Errorf("CheckServer(enterprise) = %v, want Pass", got)
	}
	if got := preflight.CheckServer(mssql.ServerInfo{EngineEdition: 6}).Severity; got != preflight.Fail {
		t.Errorf("CheckServer(synapse) = %v, want Fail", got)
	}
}

func TestCheckLog(t *testing.T) {
	tests := []struct {
		name       string
		ls         mssql.LogSpace
		reuseWait  string
		maxBytes   int64
		maxPercent int
		want       preflight.Severity
	}{
		{"healthy", mssql.LogSpace{TotalBytes: 100, UsedPercent: 50}, "NOTHING", 1000, 80, preflight.Pass},
		{"over byte cap", mssql.LogSpace{TotalBytes: 2000, UsedPercent: 100}, "NOTHING", 1000, 80, preflight.Fail},
		{"over percent cap", mssql.LogSpace{TotalBytes: 100, UsedPercent: 90}, "NOTHING", 1000, 80, preflight.Fail},
		{"reuse wait warns", mssql.LogSpace{TotalBytes: 100, UsedPercent: 10}, "AVAILABILITY_REPLICA", 1000, 80, preflight.Warn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preflight.CheckLog(tt.ls, tt.reuseWait, tt.maxBytes, tt.maxPercent).Severity
			if got != tt.want {
				t.Errorf("CheckLog() = %v, want %v", got, tt.want)
			}
		})
	}
}

// An offline rebuild materializes the new index before dropping the old one, so it needs
// roughly the object's own size free in the data files. Running out mid-rebuild wastes the
// whole operation, which is what preflight exists to prevent.
func TestCheckDataFreeSpace(t *testing.T) {
	tests := []struct {
		name   string
		needMB int
		freeMB int
		want   preflight.Severity
	}{
		{"room to spare", 100, 500, preflight.Pass},
		{"exactly enough", 100, 100, preflight.Pass},
		{"short", 500, 100, preflight.Fail},
		{"unknown size does not fail", 0, 100, preflight.Pass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preflight.CheckDataFreeSpace("dbo.MEASUREMENT.PK_MEASUREMENT", tt.needMB,
				preflight.DataSpace{FreeMB: tt.freeMB, GrowthKnown: true}).Severity
			if got != tt.want {
				t.Errorf("CheckDataFreeSpace(need=%d, free=%d) = %v, want %v", tt.needMB, tt.freeMB, got, tt.want)
			}
		})
	}
}

// Free space inside the files is not the whole story: a file that can still grow has
// headroom the check must count, or it fails runs that would have succeeded. Relying on
// growth is a Warn rather than a Pass, because the growth itself is a blocking zero-fill.
func TestCheckDataFreeSpaceCountsGrowthHeadroom(t *testing.T) {
	capped := func(sizeMB, maxMB int) mssql.FileGrowth {
		return mssql.FileGrowth{Name: "data", TypeDesc: "ROWS", SizeMB: sizeMB, Growth: 65536, MaxSizeMB: maxMB}
	}
	unlimited := mssql.FileGrowth{Name: "data", TypeDesc: "ROWS", SizeMB: 1000, Growth: 65536, MaxSizeMB: -1}
	noGrowth := mssql.FileGrowth{Name: "data", TypeDesc: "ROWS", SizeMB: 1000, Growth: 0, MaxSizeMB: 0}

	tests := []struct {
		name   string
		needMB int
		freeMB int
		growth []mssql.FileGrowth
		want   preflight.Severity
	}{
		{"short but growth covers it", 500, 100, []mssql.FileGrowth{capped(1000, 2000)}, preflight.Warn},
		{"short and growth is unlimited", 500, 100, []mssql.FileGrowth{unlimited}, preflight.Warn},
		{"short and growth is disabled", 500, 100, []mssql.FileGrowth{noGrowth}, preflight.Fail},
		{"short and the cap is too low", 5000, 100, []mssql.FileGrowth{capped(1000, 1200)}, preflight.Fail},
		{"enough free space ignores growth", 50, 100, []mssql.FileGrowth{noGrowth}, preflight.Pass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preflight.CheckDataFreeSpace("dbo.MEASUREMENT.PK_MEASUREMENT", tt.needMB,
				preflight.DataSpace{FreeMB: tt.freeMB, Growth: tt.growth, GrowthKnown: true}).Severity
			if got != tt.want {
				t.Errorf("CheckDataFreeSpace(need=%d, free=%d, growth) = %v, want %v", tt.needMB, tt.freeMB, got, tt.want)
			}
		})
	}
}

// Autogrowth settings are advisory: a bad one is a reason to tell the operator, never a
// reason to refuse to run. Percentage growth is Microsoft's own named anti-pattern because
// the increment scales with the file, and growth is a blocking zero-fill without instant
// file initialization. Growth disabled matters most when a shrink is about to remove the
// headroom that cannot then be reclaimed.
func TestCheckFileGrowth(t *testing.T) {
	fixed := mssql.FileGrowth{Name: "data", TypeDesc: "ROWS", SizeMB: 100_000, Growth: 65536, MaxSizeMB: -1} // 512 MB
	percent := mssql.FileGrowth{Name: "data", TypeDesc: "ROWS", SizeMB: 100_000, IsPercent: true, Growth: 10, MaxSizeMB: -1}
	fixedSize := mssql.FileGrowth{Name: "data", TypeDesc: "ROWS", SizeMB: 100_000, Growth: 0, MaxSizeMB: 0}

	tests := []struct {
		name      string
		files     []mssql.FileGrowth
		shrunk    map[string]bool
		want      preflight.Severity
	}{
		{"fixed increment is fine", []mssql.FileGrowth{fixed}, nil, preflight.Pass},
		{"percentage growth warns", []mssql.FileGrowth{percent}, nil, preflight.Warn},
		{"growth disabled alone is fine", []mssql.FileGrowth{fixedSize}, nil, preflight.Pass},
		{"growth disabled warns when its type is shrunk", []mssql.FileGrowth{fixedSize}, map[string]bool{"ROWS": true}, preflight.Warn},
		{"growth disabled is ignored when another type is shrunk", []mssql.FileGrowth{fixedSize}, map[string]bool{"LOG": true}, preflight.Pass},
		{"one bad file among good ones warns", []mssql.FileGrowth{fixed, percent}, nil, preflight.Warn},
		{"no files is not a finding", nil, nil, preflight.Pass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preflight.CheckFileGrowth(tt.files, tt.shrunk).Severity
			if got != tt.want {
				t.Errorf("CheckFileGrowth(shrunk=%v) = %v, want %v", tt.shrunk, got, tt.want)
			}
		})
	}
}

// The warning has to carry the number that makes it actionable: what one growth event
// would actually cost at the file's current size.
func TestCheckFileGrowthReportsEventSize(t *testing.T) {
	percent := mssql.FileGrowth{Name: "PRODDB", TypeDesc: "ROWS", SizeMB: 14_500_000, IsPercent: true, Growth: 10, MaxSizeMB: -1}

	got := preflight.CheckFileGrowth([]mssql.FileGrowth{percent}, nil)

	if !strings.Contains(got.Detail, "1450000") {
		t.Errorf("CheckFileGrowth detail = %q, want it to name the 1450000 MB growth event", got.Detail)
	}
}

func TestCheckBlocking(t *testing.T) {
	none := []mssql.Session{{SPID: 51, BlockingSPID: 0}}
	if got := preflight.CheckBlocking(none).Severity; got != preflight.Pass {
		t.Errorf("CheckBlocking(none) = %v, want Pass", got)
	}
	blocked := []mssql.Session{{SPID: 51, BlockingSPID: 0}, {SPID: 52, BlockingSPID: 51}}
	if got := preflight.CheckBlocking(blocked).Severity; got != preflight.Warn {
		t.Errorf("CheckBlocking(blocked) = %v, want Warn", got)
	}
}

func TestCheckElevatedRights(t *testing.T) {
	if got := preflight.CheckElevatedRights(true).Severity; got != preflight.Pass {
		t.Errorf("CheckElevatedRights(true) = %v, want Pass", got)
	}
	if got := preflight.CheckElevatedRights(false).Severity; got != preflight.Fail {
		t.Errorf("CheckElevatedRights(false) = %v, want Fail", got)
	}
}

func TestCheckKillPermission(t *testing.T) {
	if got := preflight.CheckKillPermission(true).Severity; got != preflight.Pass {
		t.Errorf("CheckKillPermission(true) = %v, want Pass", got)
	}
	if got := preflight.CheckKillPermission(false).Severity; got != preflight.Warn {
		t.Errorf("CheckKillPermission(false) = %v, want Warn (advisory, never Fail)", got)
	}
}

func TestRunWarnsWhenKillArmedWithoutPermission(t *testing.T) {
	info := mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16}
	manifest := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"},
	}}
	th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80}

	p := healthyProber()
	p.alterAnyConn = false // login cannot KILL
	rep, err := preflight.Run(context.Background(), p, info, manifest, th, true)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.HasFailure() {
		t.Errorf("kill-permission advisory must not fail the run:\n%v", rep.Checks)
	}
	if !rep.HasWarning() {
		t.Errorf("Run() should warn when kill is armed but ALTER ANY CONNECTION is missing")
	}
}

func TestCheckOperation(t *testing.T) {
	tests := []struct {
		name         string
		op           ddl.Operation
		tableExists  bool
		targetExists bool
		want         preflight.Severity
	}{
		{"rebuild ok", ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}, true, true, preflight.Pass},
		{"rebuild missing index", ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}, true, false, preflight.Fail},
		{"rebuild missing table", ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}, false, false, preflight.Fail},
		{"create index already exists", ddl.CreateIndex{Schema: "dbo", Table: "T", Index: "IX", Columns: []string{"C"}}, true, true, preflight.Warn},
		{"create index ok", ddl.CreateIndex{Schema: "dbo", Table: "T", Index: "IX", Columns: []string{"C"}}, true, false, preflight.Pass},
		{"add column already exists", ddl.AddColumn{Schema: "dbo", Table: "T", Column: "C", DataType: "BIT"}, true, true, preflight.Warn},
		{"alter column missing", ddl.AlterColumn{Schema: "dbo", Table: "T", Column: "C", DataType: "BIT"}, true, false, preflight.Fail},
		{"drop constraint missing", ddl.DropConstraint{Schema: "dbo", Table: "T", Constraint: "PK"}, true, false, preflight.Fail},
		// Database- and file-scoped operations have no table precondition: they must
		// pass even when no table by that (empty) name exists.
		{"shrink data passes without a table", ddl.Shrink{Type: "data", Files: "all"}, false, false, preflight.Pass},
		{"shrink log passes without a table", ddl.Shrink{Type: "log", Files: "MyDb_Log"}, false, false, preflight.Pass},
		{"check_db passes without a table", ddl.CheckDB{Database: "MyDb"}, false, false, preflight.Pass},
		{"shrink_tempdb passes without a table", ddl.ShrinkTempdb{TargetSizeMB: 20480}, false, false, preflight.Pass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preflight.CheckOperation(tt.op, tt.tableExists, tt.targetExists).Severity
			if got != tt.want {
				t.Errorf("CheckOperation() = %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeProber is a scripted Prober for testing Run without a database.
type fakeProber struct {
	logSpace       mssql.LogSpace
	reuseWait      string
	sessions       []mssql.Session
	tableExists    bool
	indexExists    bool
	elevatedAccess bool
	alterAnyConn   bool
	sysadmin       bool
	dmlPermission  bool
	dmlDenied      map[string]bool // perms this login lacks, overriding dmlPermission
	dataFreeMB     int
	growth         []mssql.FileGrowth
	growthByType   map[string][]mssql.FileGrowth
	growthErr      error
	indexSizeErr   error
	indexSizeMB    int
	indexSizeByPartition map[int]int
}

func (f fakeProber) FileSpace(context.Context, string) ([]mssql.FileSpace, error) {
	return []mssql.FileSpace{{Name: "data", TypeDesc: "ROWS", FreeMB: f.dataFreeMB}}, nil
}
func (f fakeProber) FileGrowths(_ context.Context, fileType string) ([]mssql.FileGrowth, error) {
	if f.growthErr != nil {
		return nil, f.growthErr
	}
	if f.growthByType != nil {
		return f.growthByType[fileType], nil
	}
	if fileType == mssql.FileTypeRows {
		return f.growth, nil
	}
	return nil, nil
}

func (f fakeProber) IndexSizeMB(_ context.Context, _, _, _ string, partition *int) (int, error) {
	if f.indexSizeErr != nil {
		return 0, f.indexSizeErr
	}
	if partition != nil {
		return f.indexSizeByPartition[*partition], nil
	}
	return f.indexSizeMB, nil
}

func (f fakeProber) LogSpace(context.Context) (mssql.LogSpace, error) { return f.logSpace, nil }
func (f fakeProber) LogReuseWait(context.Context) (string, error)     { return f.reuseWait, nil }
func (f fakeProber) ActiveSessions(context.Context) ([]mssql.Session, error) {
	return f.sessions, nil
}
func (f fakeProber) TableExists(context.Context, string, string) (bool, error) {
	return f.tableExists, nil
}
func (f fakeProber) IndexExists(context.Context, string, string, string) (bool, error) {
	return f.indexExists, nil
}
func (f fakeProber) ColumnExists(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (f fakeProber) ConstraintExists(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (f fakeProber) HasElevatedDBAccess(context.Context) (bool, error) {
	return f.elevatedAccess, nil
}
func (f fakeProber) IsSysadmin(context.Context) (bool, error) {
	return f.sysadmin, nil
}
func (f fakeProber) HasAlterAnyConnection(context.Context) (bool, error) {
	return f.alterAnyConn, nil
}
func (f fakeProber) HasDMLPermission(_ context.Context, _, _, perm string) (bool, error) {
	if f.dmlDenied != nil && f.dmlDenied[perm] {
		return false, nil
	}
	return f.dmlPermission, nil
}

func healthyProber() fakeProber {
	return fakeProber{
		logSpace:       mssql.LogSpace{TotalBytes: 100, UsedPercent: 10},
		reuseWait:      "NOTHING",
		tableExists:    true,
		indexExists:    true,
		elevatedAccess: true,
		sysadmin:       true,
	}
}

func TestRun(t *testing.T) {
	info := mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16}
	manifest := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"},
	}}
	th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80}

	t.Run("healthy passes", func(t *testing.T) {
		rep, err := preflight.Run(context.Background(), healthyProber(), info, manifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.OK() {
			t.Errorf("Run() report not OK:\n%v", rep.Checks)
		}
	})

	t.Run("missing index fails", func(t *testing.T) {
		p := healthyProber()
		p.indexExists = false
		rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.HasFailure() {
			t.Errorf("Run() should fail when the target index is missing")
		}
	})

	t.Run("file-scoped shrink passes without a matching table", func(t *testing.T) {
		p := healthyProber()
		p.tableExists = false // no table named "" exists; shrink must not require one
		shrinkManifest := &ddl.Manifest{Operations: []ddl.Operation{
			ddl.Shrink{Type: "data", Files: "all"},
		}}
		rep, err := preflight.Run(context.Background(), p, info, shrinkManifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.OK() {
			t.Errorf("Run() report not OK for a shrink manifest:\n%v", rep.Checks)
		}
	})

	t.Run("shrink without db_owner fails on permissions", func(t *testing.T) {
		p := healthyProber()
		p.elevatedAccess = false
		shrinkManifest := &ddl.Manifest{Operations: []ddl.Operation{
			ddl.Shrink{Type: "data", Files: "all"},
		}}
		rep, err := preflight.Run(context.Background(), p, info, shrinkManifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.HasFailure() {
			t.Errorf("Run() should fail a shrink when the login lacks db_owner/sysadmin")
		}
	})

	t.Run("check_db without db_owner fails on permissions", func(t *testing.T) {
		p := healthyProber()
		p.elevatedAccess = false
		checkManifest := &ddl.Manifest{Operations: []ddl.Operation{
			ddl.CheckDB{Database: "MyDb"},
		}}
		rep, err := preflight.Run(context.Background(), p, info, checkManifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.HasFailure() {
			t.Errorf("Run() should fail a check_db when the login lacks db_owner/sysadmin")
		}
	})

	t.Run("file-scoped shrink_tempdb passes without a matching table", func(t *testing.T) {
		p := healthyProber()
		p.tableExists = false // no table named "" exists; shrink_tempdb must not require one
		shrinkTempdbManifest := &ddl.Manifest{Operations: []ddl.Operation{
			ddl.ShrinkTempdb{TargetSizeMB: 20480},
		}}
		rep, err := preflight.Run(context.Background(), p, info, shrinkTempdbManifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.OK() {
			t.Errorf("Run() report not OK for a shrink_tempdb manifest:\n%v", rep.Checks)
		}
	})

	t.Run("shrink_tempdb without db_owner fails on permissions", func(t *testing.T) {
		p := healthyProber()
		p.elevatedAccess = false
		shrinkTempdbManifest := &ddl.Manifest{Operations: []ddl.Operation{
			ddl.ShrinkTempdb{TargetSizeMB: 20480},
		}}
		rep, err := preflight.Run(context.Background(), p, info, shrinkTempdbManifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.HasFailure() {
			t.Errorf("Run() should fail a shrink_tempdb when the login lacks db_owner/sysadmin")
		}
	})

	t.Run("ordinary DDL is not gated on elevated rights", func(t *testing.T) {
		p := healthyProber()
		p.elevatedAccess = false // not probed for a plain rebuild manifest
		rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.OK() {
			t.Errorf("Run() should not require db_owner for a rebuild manifest:\n%v", rep.Checks)
		}
	})

	t.Run("log over cap fails", func(t *testing.T) {
		p := healthyProber()
		p.logSpace = mssql.LogSpace{TotalBytes: 5000, UsedPercent: 100}
		rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.HasFailure() {
			t.Errorf("Run() should fail when the log is already over the cap")
		}
	})
}

// TestRunBatchDMLRequiresSelect pins a gap measured against SQL Server 2022 CU26: a
// login with db_datawriter but not db_datareader passed the UPDATE check and then died
// mid-run with "The SELECT permission was denied on the object". Every batch is an
// UPDATE/DELETE TOP (n), the key_range walk reads the key with its own SELECT MAX, and
// a predicate reads the columns it filters on, so SELECT is needed unconditionally —
// with a where clause and without one. An opaque execution-time permission error, after
// the engine has claimed the manifest and written a sidecar, is exactly what preflight
// exists to pre-empt.
func TestRunBatchDMLRequiresSelect(t *testing.T) {
	info := mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16, RCSIEnabled: true}
	th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80}
	manifest := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.BatchDML{
			Verb: "update", Schema: "dbo", Table: "T",
			Set:   map[string]ddl.Literal{"flag": {Raw: "1"}},
			Batch: ddl.BatchSpec{Strategy: "predicate"},
		},
	}}

	t.Run("update granted but select denied fails", func(t *testing.T) {
		p := healthyProber()
		p.dmlPermission = true
		p.dmlDenied = map[string]bool{"SELECT": true}

		rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.HasFailure() {
			t.Errorf("Run() passed with SELECT denied; the run would fail mid-batch instead:\n%v", rep.Checks)
		}
	})

	t.Run("both granted passes", func(t *testing.T) {
		p := healthyProber()
		p.dmlPermission = true

		rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if rep.HasFailure() {
			t.Errorf("Run() failed with both permissions granted:\n%v", rep.Checks)
		}
	})
}

// TestRunShrinkTempdbRequiresSysadmin pins the second permission gap of the same
// family as the batched-DML one: the elevated-rights probe asks whether the login is
// db_owner in the connected database, but DBCC SHRINKFILE for shrink_tempdb runs in
// tempdb. A db_owner of a user database passed preflight and then failed mid-operation
// with Msg 7983, "User 'guest' does not have permission to run DBCC shrinkfile for
// database 'tempdb'". Measured against SQL Server 2022 CU26. tempdb is recreated from
// model at every restart, so a membership granted there does not survive one, which
// leaves sysadmin.
func TestRunShrinkTempdbRequiresSysadmin(t *testing.T) {
	info := mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16}
	th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80}
	manifest := &ddl.Manifest{Operations: []ddl.Operation{ddl.ShrinkTempdb{TargetSizeMB: 100}}}

	t.Run("db_owner without sysadmin fails", func(t *testing.T) {
		p := healthyProber()
		p.elevatedAccess = true // db_owner in the connected database
		p.sysadmin = false

		rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.HasFailure() {
			t.Errorf("Run() passed for a non-sysadmin; the run would fail on the first DBCC:\n%v", rep.Checks)
		}
	})

	t.Run("sysadmin passes", func(t *testing.T) {
		rep, err := preflight.Run(context.Background(), healthyProber(), info, manifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if rep.HasFailure() {
			t.Errorf("Run() failed for a sysadmin:\n%v", rep.Checks)
		}
	})
}

// The data-free-space check is config-gated: on, an index rebuild that cannot fit a second
// copy of itself in the data files fails preflight rather than running out of room hours in;
// off, the check is absent from the report entirely.
func TestRunDataFreeSpace(t *testing.T) {
	info := mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16}
	manifest := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "PK_MEASUREMENT"},
	}}
	shortOfRoom := func() fakeProber {
		p := healthyProber()
		p.indexSizeMB = 5000
		p.dataFreeMB = 100
		return p
	}

	t.Run("enabled and short of room fails", func(t *testing.T) {
		th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80, RequireDataFreeSpace: true}

		rep, err := preflight.Run(context.Background(), shortOfRoom(), info, manifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.HasFailure() {
			t.Errorf("Run() passed a rebuild needing 5000 MB with 100 MB free:\n%v", rep.Checks)
		}
	})

	t.Run("disabled omits the check", func(t *testing.T) {
		th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80}

		rep, err := preflight.Run(context.Background(), shortOfRoom(), info, manifest, th, false)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		for _, c := range rep.Checks {
			if c.Name == "data free space" {
				t.Errorf("Run() emitted %q with require_data_free_space off", c.Name)
			}
		}
	})
}

// Growth advice is always available, independent of require_data_free_space: percentage
// growth hurts any long operation, not just a rebuild that is short of room.
func TestRunWarnsOnPercentGrowth(t *testing.T) {
	info := mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16}
	th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80}
	manifest := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "PK_MEASUREMENT"},
	}}

	p := healthyProber()
	p.growth = []mssql.FileGrowth{
		{Name: "PRODDB", TypeDesc: "ROWS", SizeMB: 100_000, IsPercent: true, Growth: 10, MaxSizeMB: -1},
	}

	rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.HasFailure() {
		t.Errorf("growth advice must never fail a run:\n%v", rep.Checks)
	}
	var found bool
	for _, c := range rep.Checks {
		if c.Name == "file growth" {
			found = true
			if c.Severity != preflight.Warn {
				t.Errorf("file growth check = %v, want Warn for percentage growth", c.Severity)
			}
		}
	}
	if !found {
		t.Errorf("Run() emitted no file growth check:\n%v", rep.Checks)
	}
}

// REBUILD PARTITION = n rebuilds one partition, so it needs room for that partition, not for
// the whole index. Sizing the whole index would fail a partitioned rebuild of a large table
// that has ample room for the partition actually being rebuilt.
func TestRunSizesRebuildByPartition(t *testing.T) {
	info := mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16}
	th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80, RequireDataFreeSpace: true}
	partition := 37

	p := healthyProber()
	p.dataFreeMB = 500
	p.indexSizeMB = 5000                            // the whole index does not fit
	p.indexSizeByPartition = map[int]int{37: 100}   // the one partition does
	p.growth = []mssql.FileGrowth{{Name: "data", TypeDesc: "ROWS", SizeMB: 1000, Growth: 0, MaxSizeMB: 0}}

	manifest := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "PK_MEASUREMENT", Partition: &partition},
	}}

	rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.HasFailure() {
		t.Errorf("a partitioned rebuild was sized as the whole index and failed:\n%v", rep.Checks)
	}
}

// A log shrink removes headroom from the LOG file, so that is the file whose autogrowth
// matters. Advising on the data files there is both irrelevant and a miss: a log file that
// cannot grow, after a shrink, is how a database ends up refusing writes with error 9002.
func TestRunChecksLogGrowthForALogShrink(t *testing.T) {
	info := mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16}
	th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80}

	p := healthyProber()
	p.elevatedAccess = true
	p.growthByType = map[string][]mssql.FileGrowth{
		mssql.FileTypeRows: {{Name: "PRODDB", TypeDesc: "ROWS", SizeMB: 1000, Growth: 65536, MaxSizeMB: -1}},
		mssql.FileTypeLog:  {{Name: "PRODDB_log", TypeDesc: "LOG", SizeMB: 1000, Growth: 0, MaxSizeMB: 0}},
	}
	manifest := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.Shrink{Type: "log", Files: "PRODDB_log", TargetFreeSpace: "10%"},
	}}

	rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, c := range rep.Checks {
		if c.Name == "file growth" {
			if c.Severity != preflight.Warn || !strings.Contains(c.Detail, "PRODDB_log") {
				t.Errorf("file growth = %v %q, want Warn naming the log file", c.Severity, c.Detail)
			}
			return
		}
	}
	t.Errorf("Run() emitted no file growth check:\n%v", rep.Checks)
}

// The growth read is advisory. A login without the permission it needs must not have its
// manifest aborted by it — the documented requirement is VIEW SERVER STATE, which does not
// imply the VIEW DEFINITION that sys.dm_db_partition_stats also wants.
func TestRunSurvivesAGrowthReadFailure(t *testing.T) {
	info := mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16}
	th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80, RequireDataFreeSpace: true}

	p := healthyProber()
	p.growthErr = errors.New("VIEW DEFINITION permission was denied")
	manifest := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "PK_MEASUREMENT"},
	}}

	rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
	if err != nil {
		t.Fatalf("Run() returned an error for an advisory read: %v", err)
	}
	if rep.HasFailure() {
		t.Errorf("an unreadable growth setting failed the run:\n%v", rep.Checks)
	}
	if !rep.HasWarning() {
		t.Errorf("Run() should warn when autogrowth could not be read:\n%v", rep.Checks)
	}
}

// The size read wants VIEW DEFINITION, which the documented VIEW SERVER STATE does not
// imply. A login that lacks it must get "size unknown", not a failed manifest — the check's
// own contract is that an unreadable size never fails a run.
func TestRunSurvivesAnObjectSizeReadFailure(t *testing.T) {
	info := mssql.ServerInfo{EngineEdition: 3, MajorVersion: 16}
	th := preflight.Thresholds{LogMaxBytes: 1000, LogMaxPercent: 80, RequireDataFreeSpace: true}

	p := healthyProber()
	p.indexSizeErr = errors.New("The SELECT permission was denied on the object 'dm_db_partition_stats'")
	manifest := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "PK_MEASUREMENT"},
	}}

	rep, err := preflight.Run(context.Background(), p, info, manifest, th, false)
	if err != nil {
		t.Fatalf("Run() returned an error for an unreadable object size: %v", err)
	}
	if rep.HasFailure() {
		t.Errorf("an unreadable object size failed the run:\n%v", rep.Checks)
	}
}
