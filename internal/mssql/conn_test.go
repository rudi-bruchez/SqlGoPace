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

// The victim killer excludes our own sessions by prefix-matching program_name against
// this value, so it has to be what the DSN actually configured — not the constant.
func TestAppNameBase(t *testing.T) {
	tests := []struct {
		name, app, want string
	}{
		{"dsn app name wins over the constant", "DBAToolkit", "DBAToolkit"},
		{"driver default falls back", "go-mssqldb", AppNamePrefix},
		{"empty falls back", "", AppNamePrefix},
		{"whitespace trimmed", "  DBAToolkit  ", "DBAToolkit"},
		{"already a base is idempotent", AppNamePrefix, AppNamePrefix},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appNameBase(tt.app); got != tt.want {
				t.Errorf("appNameBase(%q) = %q, want %q", tt.app, got, tt.want)
			}
		})
	}
}

func TestConnAppNamePrefix(t *testing.T) {
	c := &Conn{appName: appNameBase("DBAToolkit")}
	if got := c.AppNamePrefix(); got != "DBAToolkit" {
		t.Errorf("AppNamePrefix() = %q, want %q", got, "DBAToolkit")
	}
}
