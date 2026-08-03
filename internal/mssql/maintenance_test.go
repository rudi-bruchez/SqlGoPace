package mssql_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestIsMaintenanceCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"ALTER INDEX", true},
		{"alter index", true},
		{"  ALTER INDEX  ", true},
		{"DBCC", true},
		{"dbcc", true},
		{"SELECT", false},
		{"INSERT", false},
		{"BACKUP DATABASE", false},
		{"", false},
		// DbccFilesCompact is an internal wait/task name, never a command verb.
		{"DbccFilesCompact", false},
	}
	for _, c := range cases {
		if got := mssql.IsMaintenanceCommand(c.cmd); got != c.want {
			t.Errorf("IsMaintenanceCommand(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestIsAmplifyingCommand(t *testing.T) {
	tests := []struct {
		name  string
		cmd   string
		allow []string
		want  bool
	}{
		{name: "update statistics singular verb", cmd: "UPDATE STATISTIC", want: true},
		{name: "update statistics plural verb", cmd: "UPDATE STATISTICS", want: true},
		{name: "alter index", cmd: "ALTER INDEX", want: true},
		{name: "alter table", cmd: "ALTER TABLE", want: true},
		{name: "create index", cmd: "CREATE INDEX", want: true},
		{name: "create statistics", cmd: "CREATE STATISTICS", want: true},
		{name: "drop index", cmd: "DROP INDEX", want: true},
		{name: "drop table", cmd: "DROP TABLE", want: true},
		{name: "truncate table", cmd: "TRUNCATE TABLE", want: true},
		{name: "dbcc", cmd: "DBCC", want: true},
		{name: "lowercase and padded", cmd: "  alter index  ", want: true},
		{name: "select is not amplifying", cmd: "SELECT", want: false},
		{name: "insert is not amplifying", cmd: "INSERT", want: false},
		{name: "backup is not amplifying", cmd: "BACKUP DATABASE", want: false},
		{name: "empty is not amplifying", cmd: "", want: false},
		{name: "override narrows to stats only", cmd: "ALTER INDEX", allow: []string{"UPDATE STATISTIC"}, want: false},
		{name: "override matches its own entry", cmd: "UPDATE STATISTICS", allow: []string{"UPDATE STATISTIC"}, want: true},
		{name: "override entry is case folded", cmd: "UPDATE STATISTICS", allow: []string{"update statistic"}, want: true},
		{name: "empty override falls back to built-in", cmd: "ALTER INDEX", allow: []string{}, want: true},
		// A dangling YAML item leaves "" in the list. Every string has the empty
		// prefix, so treating it as one would kill every session we block — an
		// application SELECT, an INSERT, an open user transaction.
		{name: "empty entry does not match a select", cmd: "SELECT", allow: []string{"", "UPDATE STATISTIC"}, want: false},
		{name: "whitespace entry does not match a select", cmd: "SELECT", allow: []string{" ", "UPDATE STATISTIC"}, want: false},
		{name: "empty entry does not widen an insert", cmd: "INSERT", allow: []string{""}, want: false},
		{name: "real entries beside an empty one still match", cmd: "UPDATE STATISTICS", allow: []string{"", "UPDATE STATISTIC"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mssql.IsAmplifyingCommand(tt.cmd, tt.allow); got != tt.want {
				t.Errorf("IsAmplifyingCommand(%q, %v) = %v, want %v", tt.cmd, tt.allow, got, tt.want)
			}
		})
	}
}

func TestDefaultAmplifyingCommandsIsACopy(t *testing.T) {
	a := mssql.DefaultAmplifyingCommands()
	if len(a) == 0 {
		t.Fatal("DefaultAmplifyingCommands() is empty")
	}
	a[0] = "MUTATED"
	if b := mssql.DefaultAmplifyingCommands(); b[0] == "MUTATED" {
		t.Error("DefaultAmplifyingCommands() returned the backing array, not a copy")
	}
}
