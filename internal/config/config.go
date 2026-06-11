// Package config loads and validates SqlGoPace's config.yaml. Secrets are never
// stored in the YAML: ${VAR} references are expanded from the environment (and a
// best-effort .env file) at load time. The resolved config maps to a ddl.Policy
// for option resolution.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// ErrInvalidConfig is returned for a structurally valid YAML that violates a
// configuration rule (missing required value, out-of-range threshold, etc.).
var ErrInvalidConfig = errors.New("invalid config")

// Config is the full SqlGoPace configuration.
type Config struct {
	Database        DatabaseConfig        `yaml:"database"`
	Directories     DirectoriesConfig     `yaml:"directories"`
	Monitoring      MonitoringConfig      `yaml:"monitoring"`
	Preflight       PreflightConfig       `yaml:"preflight"`
	OptionsOverride OptionsOverrideConfig `yaml:"options_override"`
	Notifications   NotificationsConfig   `yaml:"notifications"`
	History         HistoryConfig         `yaml:"history"`
	MatrixFile      string                `yaml:"matrix_file"`
}

// DatabaseConfig holds connection settings. LoginTimeout is the connection
// timeout only; the driver applies no query timeout (DDL is unbounded by design).
type DatabaseConfig struct {
	ConnectionString    string `yaml:"connection_string"`
	LoginTimeoutSeconds int    `yaml:"login_timeout_seconds"`
}

// LoginTimeout returns the connection timeout as a duration.
func (d DatabaseConfig) LoginTimeout() time.Duration {
	return time.Duration(d.LoginTimeoutSeconds) * time.Second
}

// DirectoriesConfig holds the manifest lifecycle directories.
type DirectoriesConfig struct {
	ToRun      string `yaml:"to_run"`
	Processing string `yaml:"processing"`
	Done       string `yaml:"done"`
	Failed     string `yaml:"failed"`
}

// MonitoringConfig holds the decoupled poll intervals and pressure thresholds.
type MonitoringConfig struct {
	BlockingPollSeconds         int   `yaml:"blocking_poll_seconds"`
	LogPollSeconds              int   `yaml:"log_poll_seconds"`
	ProgressPollSeconds         int   `yaml:"progress_poll_seconds"`
	LogMaxSizeBytes             int64 `yaml:"log_max_size_bytes"`
	LogMaxPercent               int   `yaml:"log_max_percent"`
	BlockingTimeoutMinutes      int   `yaml:"blocking_timeout_minutes"`
	LogDrainTimeoutMinutes      int   `yaml:"log_drain_timeout_minutes"`
	MaxRetryAttempts            int   `yaml:"max_retry_attempts"`
	KillGraceSeconds            int   `yaml:"kill_grace_seconds"`
	ReconnectTimeoutMinutes     int   `yaml:"reconnect_timeout_minutes"`
	CheckpointBetweenOperations bool  `yaml:"checkpoint_between_operations"`
}

// BlockingPoll returns the blocking-detection poll interval.
func (m MonitoringConfig) BlockingPoll() time.Duration {
	return time.Duration(m.BlockingPollSeconds) * time.Second
}

// LogPoll returns the transaction-log poll interval.
func (m MonitoringConfig) LogPoll() time.Duration {
	return time.Duration(m.LogPollSeconds) * time.Second
}

// ProgressPoll returns the progress poll interval.
func (m MonitoringConfig) ProgressPoll() time.Duration {
	return time.Duration(m.ProgressPollSeconds) * time.Second
}

// BlockingTimeout returns how long the DDL may block others before reacting.
func (m MonitoringConfig) BlockingTimeout() time.Duration {
	return time.Duration(m.BlockingTimeoutMinutes) * time.Minute
}

// LogDrainTimeout returns how long to wait for the log to drain before aborting.
func (m MonitoringConfig) LogDrainTimeout() time.Duration {
	return time.Duration(m.LogDrainTimeoutMinutes) * time.Minute
}

// KillGrace returns the grace period before an explicit KILL.
func (m MonitoringConfig) KillGrace() time.Duration {
	return time.Duration(m.KillGraceSeconds) * time.Second
}

// ReconnectTimeout returns how long to wait for the server to come back (and the
// resumable state to become readable) after a connection loss before deciding.
func (m MonitoringConfig) ReconnectTimeout() time.Duration {
	return time.Duration(m.ReconnectTimeoutMinutes) * time.Minute
}

// PreflightConfig toggles individual pre-flight checks.
type PreflightConfig struct {
	RequireDataFreeSpace bool `yaml:"require_data_free_space"`
	CheckTempDB          bool `yaml:"check_tempdb"`
	AGSendQueueWarn      bool `yaml:"ag_send_queue_warn"`
}

// forceBool is a tri-state override: nil means "auto".
type forceBool struct {
	Force *bool `yaml:"force"`
}

// forceInt is an optional integer override: nil means "unset".
type forceInt struct {
	Force *int `yaml:"force"`
}

// OptionsOverrideConfig holds the global option policy from config.
type OptionsOverrideConfig struct {
	Online                 forceBool `yaml:"online"`
	Resumable              forceBool `yaml:"resumable"`
	WaitAtLowPriority      forceBool `yaml:"wait_at_low_priority"`
	SortInTempDB           forceBool `yaml:"sort_in_tempdb"`
	MaxDOP                 forceInt  `yaml:"maxdop"`
	AllowAbortBlockers     bool      `yaml:"allow_abort_blockers"`
	WaitMaxDurationMinutes int       `yaml:"wait_max_duration_minutes"`
}

// NotificationsConfig holds webhook settings.
type NotificationsConfig struct {
	WebhookURL string   `yaml:"webhook_url"`
	OnEvents   []string `yaml:"on_events"`
}

// HistoryConfig holds run-history persistence settings.
type HistoryConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Destination string `yaml:"destination"`
}

// Policy maps the configured option overrides to a ddl.Policy.
func (c *Config) Policy() ddl.Policy {
	o := c.OptionsOverride
	return ddl.Policy{
		Online:                 o.Online.Force,
		Resumable:              o.Resumable.Force,
		WaitAtLowPriority:      o.WaitAtLowPriority.Force,
		SortInTempDB:           o.SortInTempDB.Force,
		MaxDOP:                 o.MaxDOP.Force,
		AllowAbortBlockers:     o.AllowAbortBlockers,
		WaitMaxDurationMinutes: o.WaitMaxDurationMinutes,
	}
}

// Parse expands ${VAR} references from the current environment, decodes the YAML
// (rejecting unknown fields), applies defaults, and validates.
func Parse(data []byte) (*Config, error) {
	expanded := os.ExpandEnv(string(data))

	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Load reads a config file, expanding a best-effort .env from the working
// directory first (existing environment variables take precedence).
func Load(path string) (*Config, error) {
	_ = godotenv.Load() // .env is optional

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

func (c *Config) applyDefaults() {
	if c.OptionsOverride.WaitMaxDurationMinutes <= 0 {
		c.OptionsOverride.WaitMaxDurationMinutes = 1
	}
	if c.Monitoring.ReconnectTimeoutMinutes <= 0 {
		c.Monitoring.ReconnectTimeoutMinutes = 2
	}
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Database.ConnectionString) == "" {
		return fmt.Errorf("database.connection_string is required: %w", ErrInvalidConfig)
	}
	dirs := []struct {
		name, value string
	}{
		{"to_run", c.Directories.ToRun},
		{"processing", c.Directories.Processing},
		{"done", c.Directories.Done},
		{"failed", c.Directories.Failed},
	}
	for _, d := range dirs {
		if strings.TrimSpace(d.value) == "" {
			return fmt.Errorf("directories.%s is required: %w", d.name, ErrInvalidConfig)
		}
	}
	polls := []struct {
		name  string
		value int
	}{
		{"blocking_poll_seconds", c.Monitoring.BlockingPollSeconds},
		{"log_poll_seconds", c.Monitoring.LogPollSeconds},
		{"progress_poll_seconds", c.Monitoring.ProgressPollSeconds},
	}
	for _, p := range polls {
		if p.value <= 0 {
			return fmt.Errorf("monitoring.%s must be > 0: %w", p.name, ErrInvalidConfig)
		}
	}
	if c.Monitoring.LogMaxPercent < 1 || c.Monitoring.LogMaxPercent > 100 {
		return fmt.Errorf("monitoring.log_max_percent must be in 1..100: %w", ErrInvalidConfig)
	}
	if c.Monitoring.LogMaxSizeBytes <= 0 {
		return fmt.Errorf("monitoring.log_max_size_bytes must be > 0: %w", ErrInvalidConfig)
	}
	if c.Monitoring.MaxRetryAttempts < 0 {
		return fmt.Errorf("monitoring.max_retry_attempts must be >= 0: %w", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.MatrixFile) == "" {
		return fmt.Errorf("matrix_file is required: %w", ErrInvalidConfig)
	}
	return nil
}
