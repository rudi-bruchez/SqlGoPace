package dotenv_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/dotenv"
)

func TestParse(t *testing.T) {
	in := strings.Join([]string{
		"# a comment",
		"",
		"SQLGOPACE_PLAIN=value",
		"export SQLGOPACE_EXPORTED=exported",
		`SQLGOPACE_DQUOTED="double quoted"`,
		"SQLGOPACE_SQUOTED='single quoted'",
		"SQLGOPACE_SPACED  =  spaced  ",
		"SQLGOPACE_EMPTY=",
		"SQLGOPACE_EQUALS=a=b=c",
	}, "\n")

	got, err := dotenv.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := map[string]string{
		"SQLGOPACE_PLAIN":    "value",
		"SQLGOPACE_EXPORTED": "exported",
		"SQLGOPACE_DQUOTED":  "double quoted",
		"SQLGOPACE_SQUOTED":  "single quoted",
		"SQLGOPACE_SPACED":   "spaced",
		"SQLGOPACE_EMPTY":    "",
		"SQLGOPACE_EQUALS":   "a=b=c",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Parse()[%q] = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("Parse() returned %d keys, want %d: %v", len(got), len(want), got)
	}
}

func TestParseRejectsLineWithoutEquals(t *testing.T) {
	if _, err := dotenv.Parse(strings.NewReader("NOT_AN_ASSIGNMENT")); err == nil {
		t.Errorf("Parse() error = nil, want an error for a line with no '='")
	}
}

func TestLoadMissingFileIsSilent(t *testing.T) {
	if err := dotenv.Load(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Errorf("Load(missing) = %v, want nil: .env is optional", err)
	}
}

// TestLoadDoesNotOverrideTheRealEnvironment is the rule that matters: an explicit
// export always wins over the file, so a developer can override one key for one
// command without editing .env.
func TestLoadDoesNotOverrideTheRealEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "SQLGOPACE_TEST_PRESET=from_file\nSQLGOPACE_TEST_NEW=from_file\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Setenv("SQLGOPACE_TEST_PRESET", "from_environment")

	if err := dotenv.Load(path); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := os.Getenv("SQLGOPACE_TEST_PRESET"); got != "from_environment" {
		t.Errorf("SQLGOPACE_TEST_PRESET = %q, want from_environment (the real environment wins)", got)
	}
	if got := os.Getenv("SQLGOPACE_TEST_NEW"); got != "from_file" {
		t.Errorf("SQLGOPACE_TEST_NEW = %q, want from_file", got)
	}
	t.Cleanup(func() { _ = os.Unsetenv("SQLGOPACE_TEST_NEW") })
}
