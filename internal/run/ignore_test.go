package run

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// sid returns a pointer to i, for IgnoredSession.SessionID.
func sid(i int) *int { return &i }

func TestIgnoredSessionsIgnores(t *testing.T) {
	sess := mssql.Session{
		SPID:        53,
		Login:       "svc_reporting",
		Host:        "BATCH01",
		Program:     "SQLAgent - TSQL JobStep",
		ActiveQuery: "SELECT * FROM dbo.Report",
		ParentQuery: "EXEC dbo.RunReport",
	}
	tests := []struct {
		name  string
		rules []ddl.IgnoredSession
		want  bool
	}{
		{"empty list ignores nothing", nil, false},
		{"session_id match", []ddl.IgnoredSession{{SessionID: sid(53)}}, true},
		{"session_id mismatch", []ddl.IgnoredSession{{SessionID: sid(99)}}, false},
		{"app_name regexp match", []ddl.IgnoredSession{{AppName: "^SQLAgent"}}, true},
		{"app_name regexp mismatch", []ddl.IgnoredSession{{AppName: "^Tableau"}}, false},
		{"host_name match", []ddl.IgnoredSession{{HostName: "BATCH0[0-9]"}}, true},
		{"login_name match", []ddl.IgnoredSession{{LoginName: "svc_(reporting|etl)"}}, true},
		{"statement matches active query", []ddl.IgnoredSession{{Statement: `dbo\.Report`}}, true},
		{"statement falls back to parent batch", []ddl.IgnoredSession{{Statement: "RunReport"}}, true},
		{"statement mismatch", []ddl.IgnoredSession{{Statement: "DELETE"}}, false},
		{"AND within entry: all fields match", []ddl.IgnoredSession{{AppName: "^SQLAgent", LoginName: "svc_reporting"}}, true},
		{"AND within entry: one field fails", []ddl.IgnoredSession{{AppName: "^SQLAgent", LoginName: "other"}}, false},
		{"OR across entries: second matches", []ddl.IgnoredSession{{AppName: "nope"}, {HostName: "BATCH01"}}, true},
		{"session_id plus regexp both required", []ddl.IgnoredSession{{SessionID: sid(53), AppName: "^Tableau"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ig, err := CompileIgnoredSessions(tt.rules)
			if err != nil {
				t.Fatalf("CompileIgnoredSessions() error = %v", err)
			}
			if got := ig.ignores(sess); got != tt.want {
				t.Errorf("ignores() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompileIgnoredSessionsBadRegexp(t *testing.T) {
	if _, err := CompileIgnoredSessions([]ddl.IgnoredSession{{AppName: "("}}); err == nil {
		t.Fatal("CompileIgnoredSessions() error = nil, want a regexp compile error")
	}
}
