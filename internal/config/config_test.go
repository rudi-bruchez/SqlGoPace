package config_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
)

const validYAML = `
database:
  connection_string: "server=${TEST_SERVER};database=db"
  login_timeout_seconds: 15
directories:
  to_run: ./01.to_run
  processing: ./02.processing
  done: ./03.done
  failed: ./04.failed
monitoring:
  blocking_poll_seconds: 10
  log_poll_seconds: 60
  progress_poll_seconds: 30
  log_max_size_bytes: 1000
  log_max_percent: 80
  blocking_timeout_minutes: 5
  log_drain_timeout_minutes: 30
  max_retry_attempts: 3
  kill_grace_seconds: 30
  checkpoint_between_operations: false
preflight:
  require_data_free_space: true
  check_tempdb: true
  ag_send_queue_warn: true
options_override:
  online: { force: true }
  resumable: { force: null }
  wait_at_low_priority: { force: null }
  sort_in_tempdb: { force: null }
  maxdop: { force: 4 }
  allow_abort_blockers: true
  wait_max_duration_minutes: 5
notifications:
  webhook_url: ""
  on_events: [fail]
history:
  enabled: false
  destination: ""
matrix_file: ./ddl_compatibility.yaml
`

func TestParseExpandsEnvAndMapsPolicy(t *testing.T) {
	t.Setenv("TEST_SERVER", "myhost")

	cfg, err := config.Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}

	if got, want := cfg.Database.ConnectionString, "server=myhost;database=db"; got != want {
		t.Errorf("ConnectionString = %q, want %q", got, want)
	}
	if got, want := cfg.Monitoring.BlockingPoll(), 10*time.Second; got != want {
		t.Errorf("BlockingPoll() = %v, want %v", got, want)
	}

	p := cfg.Policy()
	if p.Online == nil || !*p.Online {
		t.Errorf("Policy().Online = %v, want *true", p.Online)
	}
	if p.MaxDOP == nil || *p.MaxDOP != 4 {
		t.Errorf("Policy().MaxDOP = %v, want *4", p.MaxDOP)
	}
	if !p.AllowAbortBlockers {
		t.Errorf("Policy().AllowAbortBlockers = false, want true")
	}
	if p.WaitMaxDurationMinutes != 5 {
		t.Errorf("Policy().WaitMaxDurationMinutes = %d, want 5", p.WaitMaxDurationMinutes)
	}
	if p.Resumable != nil {
		t.Errorf("Policy().Resumable = %v, want nil (auto)", p.Resumable)
	}
}

func TestParseDefaultsWaitDuration(t *testing.T) {
	yaml := strings.Replace(validYAML, "wait_max_duration_minutes: 5", "wait_max_duration_minutes: 0", 1)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if got := cfg.Policy().WaitMaxDurationMinutes; got != 1 {
		t.Errorf("WaitMaxDurationMinutes = %d, want default 1", got)
	}
}

func TestParseDefaultsBlockingTimeout(t *testing.T) {
	// A zero blocking timeout would make the DDL react to (and the console show) any block
	// instantly; an unset field must default, not mean react-immediately.
	yaml := strings.Replace(validYAML, "blocking_timeout_minutes: 5", "blocking_timeout_minutes: 0", 1)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if got := cfg.Monitoring.BlockingTimeout(); got != time.Minute {
		t.Errorf("BlockingTimeout() = %v, want default 1m", got)
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{"missing connection string", func(s string) string {
			return strings.Replace(s, `connection_string: "server=${TEST_SERVER};database=db"`, `connection_string: ""`, 1)
		}},
		{"empty directory", func(s string) string {
			return strings.Replace(s, "to_run: ./01.to_run", `to_run: ""`, 1)
		}},
		{"zero poll interval", func(s string) string {
			return strings.Replace(s, "blocking_poll_seconds: 10", "blocking_poll_seconds: 0", 1)
		}},
		{"percent out of range", func(s string) string {
			return strings.Replace(s, "log_max_percent: 80", "log_max_percent: 150", 1)
		}},
		{"unknown field", func(s string) string {
			return s + "\nbogus_field: 1\n"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := config.Parse([]byte(tt.mutate(validYAML))); err == nil {
				t.Errorf("Parse() error = nil, want non-nil")
			}
		})
	}
}

func TestShrinkConfigDefaultsWhenAbsent(t *testing.T) {
	t.Setenv("TEST_SERVER", "myhost")

	// validYAML has no shrink: block at all.
	cfg, err := config.Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	s := cfg.Shrink
	checks := []struct {
		name string
		got  int
		want int
	}{
		{"initial_step_small_mb", s.InitialStepSmallMB, 100},
		{"initial_step_medium_mb", s.InitialStepMediumMB, 250},
		{"initial_step_large_mb", s.InitialStepLargeMB, 500},
		{"target_chunks", s.TargetChunks, 1000},
		{"max_step_pct_of_file", s.MaxStepPctOfFile, 5},
		{"min_step_mb", s.MinStepMB, 50},
		{"max_step_mb", s.MaxStepMB, 8192},
		{"target_batch_seconds", s.TargetBatchSeconds, 5},
		{"max_no_progress", s.MaxNoProgress, 3},
		{"no_progress_backoff_seconds", s.NoProgressBackoffSeconds, 30},
		{"no_progress_backoff_max_seconds", s.NoProgressBackoffMaxSeconds, 300},
		{"self_wait_timeout_minutes", s.SelfWaitTimeoutMinutes, 5},
		{"log_reuse_wait_timeout_minutes", s.LogReuseWaitTimeoutMinutes, 30},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("Shrink.%s = %d, want default %d", c.name, c.got, c.want)
		}
	}
	if got, want := s.TargetBatch(), 5*time.Second; got != want {
		t.Errorf("TargetBatch() = %v, want %v", got, want)
	}
	if got, want := s.LogReuseWaitTimeout(), 30*time.Minute; got != want {
		t.Errorf("LogReuseWaitTimeout() = %v, want %v", got, want)
	}
}

func TestShrinkConfigPartialOverride(t *testing.T) {
	t.Setenv("TEST_SERVER", "myhost")

	yaml := validYAML + "shrink:\n  min_step_mb: 8\n  max_step_mb: 4096\n"
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if cfg.Shrink.MinStepMB != 8 {
		t.Errorf("MinStepMB = %d, want overridden 8", cfg.Shrink.MinStepMB)
	}
	if cfg.Shrink.MaxStepMB != 4096 {
		t.Errorf("MaxStepMB = %d, want overridden 4096", cfg.Shrink.MaxStepMB)
	}
	// Untouched fields keep their defaults.
	if cfg.Shrink.TargetBatchSeconds != 5 {
		t.Errorf("TargetBatchSeconds = %d, want default 5", cfg.Shrink.TargetBatchSeconds)
	}
}

func TestShrinkConfigRejectsMinAboveMax(t *testing.T) {
	t.Setenv("TEST_SERVER", "myhost")

	yaml := validYAML + "shrink:\n  min_step_mb: 2048\n  max_step_mb: 1024\n"
	if _, err := config.Parse([]byte(yaml)); err == nil {
		t.Errorf("Parse() with min > max error = nil, want non-nil")
	}
}

// yamlWithoutNotifications strips validYAML's existing notifications: block so a
// test can append its own without producing a duplicate top-level key.
func yamlWithoutNotifications(t *testing.T) string {
	t.Helper()
	const block = "notifications:\n  webhook_url: \"\"\n  on_events: [fail]\n"
	stripped := strings.Replace(validYAML, block, "", 1)
	if stripped == validYAML {
		t.Fatal("yamlWithoutNotifications: notifications block not found in validYAML")
	}
	return stripped
}

func TestEmailConfigParsesAndDefaultsPort(t *testing.T) {
	t.Setenv("TEST_SERVER", "myhost")
	t.Setenv("SMTP_PASS", "s3cret")
	yaml := yamlWithoutNotifications(t) + "notifications:\n" +
		"  on_events: [fail, incomplete]\n" +
		"  email:\n" +
		"    host: smtp.internal\n" +
		"    from: sqlgopace@example.com\n" +
		"    to: [dba@example.com]\n" +
		"    password: \"${SMTP_PASS}\"\n"
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	e := cfg.Notifications.Email
	if e.Host != "smtp.internal" || e.Port != 25 || e.From != "sqlgopace@example.com" ||
		len(e.To) != 1 || e.To[0] != "dba@example.com" || e.Password != "s3cret" {
		t.Errorf("email config = %+v, want host/port(25 default)/from/to/expanded password", e)
	}
}

func TestEmailConfigRequiresFromAndTo(t *testing.T) {
	t.Setenv("TEST_SERVER", "myhost")
	yaml := yamlWithoutNotifications(t) + "notifications:\n  email:\n    host: smtp.internal\n"
	if _, err := config.Parse([]byte(yaml)); err == nil {
		t.Fatal("Parse() want error when email.host set without from/to")
	}
}

func TestEmailConfigRequiresPasswordWhenUsername(t *testing.T) {
	t.Setenv("TEST_SERVER", "myhost")
	yaml := yamlWithoutNotifications(t) + "notifications:\n  email:\n" +
		"    host: smtp.internal\n    from: a@x\n    to: [b@y]\n    username: u\n"
	if _, err := config.Parse([]byte(yaml)); err == nil {
		t.Fatal("Parse() want error when username set without password")
	}
}

func TestLoadShippedConfig(t *testing.T) {
	t.Setenv("DB_SERVER", "localhost")
	t.Setenv("DB_NAME", "testdb")

	path := filepath.FromSlash("../../config.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if !strings.Contains(cfg.Database.ConnectionString, "server=localhost") {
		t.Errorf("ConnectionString = %q, want expanded server=localhost", cfg.Database.ConnectionString)
	}
	if cfg.MatrixFile == "" {
		t.Errorf("MatrixFile is empty, want a path")
	}
}
