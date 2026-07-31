package mssql

import "testing"

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
		if got := IsMaintenanceCommand(c.cmd); got != c.want {
			t.Errorf("IsMaintenanceCommand(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}
