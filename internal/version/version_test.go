package version

import "testing"

func TestVersionFromFile(t *testing.T) {
	if Version() == "" {
		t.Fatal("Version() is empty; the VERSION file must contain a version")
	}
}

func TestVersionOverrideWins(t *testing.T) {
	old := override
	t.Cleanup(func() { override = old })

	override = "  9.9.9 "
	if got := Version(); got != "9.9.9" {
		t.Errorf("Version() = %q, want %q (override, trimmed)", got, "9.9.9")
	}
}
