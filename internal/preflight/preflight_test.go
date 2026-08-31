package preflight_test

import (
	"context"
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
	dmlPermission  bool
	dmlDenied      map[string]bool // perms this login lacks, overriding dmlPermission
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
