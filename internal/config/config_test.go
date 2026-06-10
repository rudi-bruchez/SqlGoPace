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
