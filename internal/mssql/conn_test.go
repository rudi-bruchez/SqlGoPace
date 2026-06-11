package mssql

import "testing"

func TestAppNameWithVersion(t *testing.T) {
	tests := []struct {
		name, app, version, want string
	}{
		{"configured app name + version", "SqlGoPace", "0.1.0", "SqlGoPace/0.1.0"},
		{"custom app name preserved", "MyApp", "2.0", "MyApp/2.0"},
		{"driver default falls back", "go-mssqldb", "0.1.0", "SqlGoPace/0.1.0"},
		{"empty app name falls back", "", "0.1.0", "SqlGoPace/0.1.0"},
		{"no version keeps base", "SqlGoPace", "", "SqlGoPace"},
		{"whitespace trimmed", "  SqlGoPace  ", " 0.1.0 ", "SqlGoPace/0.1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appNameWithVersion(tt.app, tt.version); got != tt.want {
				t.Errorf("appNameWithVersion(%q, %q) = %q, want %q", tt.app, tt.version, got, tt.want)
			}
		})
	}
}
