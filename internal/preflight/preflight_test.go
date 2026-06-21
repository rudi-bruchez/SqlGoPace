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
	dmlPermission  bool
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
func (f fakeProber) HasDMLPermission(context.Context, string, string, string) (bool, error) {
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
		rep, err := preflight.Run(context.Background(), healthyProber(), info, manifest, th)
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
		rep, err := preflight.Run(context.Background(), p, info, manifest, th)
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
		rep, err := preflight.Run(context.Background(), p, info, shrinkManifest, th)
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
		rep, err := preflight.Run(context.Background(), p, info, shrinkManifest, th)
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
		rep, err := preflight.Run(context.Background(), p, info, checkManifest, th)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.HasFailure() {
			t.Errorf("Run() should fail a check_db when the login lacks db_owner/sysadmin")
		}
	})

	t.Run("ordinary DDL is not gated on elevated rights", func(t *testing.T) {
		p := healthyProber()
		p.elevatedAccess = false // not probed for a plain rebuild manifest
		rep, err := preflight.Run(context.Background(), p, info, manifest, th)
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
		rep, err := preflight.Run(context.Background(), p, info, manifest, th)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !rep.HasFailure() {
			t.Errorf("Run() should fail when the log is already over the cap")
		}
	})
}
