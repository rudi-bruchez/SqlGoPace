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
	Shrink          ShrinkConfig          `yaml:"shrink"`
	BatchDML        BatchDMLConfig        `yaml:"batch_dml"`
	KillBlockers    KillBlockersConfig    `yaml:"kill_blockers"`
	MatrixFile      string                `yaml:"matrix_file"`
}

// KillBlockersConfig arms the selective blocker-kill policy. The match rules themselves
// live per-manifest in kill_blocked_sessions (hot-reloadable, TUI-appendable); this global
// block is the safety gate. Killing a session is destructive, so it is off by default and
// only happens when Enabled is true. DefaultAfterSeconds seeds a rule's delay when it sets
// none (0 = kill on sight).
type KillBlockersConfig struct {
	Enabled             bool `yaml:"enabled"`
	DefaultAfterSeconds int  `yaml:"default_after_seconds"`
}

// DefaultAfter returns the default kill delay for a rule that sets none.
func (k KillBlockersConfig) DefaultAfter() time.Duration {
	return time.Duration(k.DefaultAfterSeconds) * time.Second
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

// ShrinkConfig holds the tuning parameters for the DBCC SHRINKFILE driver. Every
// field is optional: an absent shrink: block (or any zero/negative field) falls
// back to the documented default. These are global only (never per-manifest) —
// they depend on the instance's storage and SLA, not on the operation. They are
// starting points and bounds that the driver's dynamic step calibration varies.
type ShrinkConfig struct {
	InitialStepSmallMB          int `yaml:"initial_step_small_mb"`           // legacy tier: reclaim < 5 GB (used only when target_chunks <= 0)
	InitialStepMediumMB         int `yaml:"initial_step_medium_mb"`          // legacy tier: reclaim 5–50 GB
	InitialStepLargeMB          int `yaml:"initial_step_large_mb"`           // legacy tier: reclaim > 50 GB
	TargetChunks                int `yaml:"target_chunks"`                   // aim to finish in ~this many chunks: initial step = reclaim / target_chunks
	MaxStepPctOfFile            int `yaml:"max_step_pct_of_file"`            // per-file ceiling: cap the step at this % of the file (0 disables)
	MinStepMB                   int `yaml:"min_step_mb"`                     // floor: below this, per-loop overhead dominates
	MaxStepMB                   int `yaml:"max_step_mb"`                     // absolute ceiling: avoid saturating I/O in one move
	TargetBatchSeconds          int `yaml:"target_batch_seconds"`            // an "ideal" chunk lasts a few seconds
	MaxNoProgress               int `yaml:"max_no_progress"`                 // consecutive no-gain chunks before clean stop
	NoProgressBackoffSeconds    int `yaml:"no_progress_backoff_seconds"`     // wait before retry, doubled each no-progress
	NoProgressBackoffMaxSeconds int `yaml:"no_progress_backoff_max_seconds"` // backoff ceiling
	SelfWaitTimeoutMinutes      int `yaml:"self_wait_timeout_minutes"`       // max wait on Sch-M / snapshot before clean stop
	LogReuseWaitTimeoutMinutes  int `yaml:"log_reuse_wait_timeout_minutes"`  // max wait for a scheduled BACKUP LOG to free the log
}

// TargetBatch returns the ideal per-chunk duration.
func (s ShrinkConfig) TargetBatch() time.Duration {
	return time.Duration(s.TargetBatchSeconds) * time.Second
}

// NoProgressBackoff returns the initial backoff after a no-progress chunk.
func (s ShrinkConfig) NoProgressBackoff() time.Duration {
	return time.Duration(s.NoProgressBackoffSeconds) * time.Second
}

// NoProgressBackoffMax returns the backoff ceiling.
func (s ShrinkConfig) NoProgressBackoffMax() time.Duration {
	return time.Duration(s.NoProgressBackoffMaxSeconds) * time.Second
}

// SelfWaitTimeout returns how long the driver waits while blocked (Sch-M/snapshot)
// before stopping cleanly.
func (s ShrinkConfig) SelfWaitTimeout() time.Duration {
	return time.Duration(s.SelfWaitTimeoutMinutes) * time.Minute
}

// LogReuseWaitTimeout returns how long to wait for a scheduled log backup to free
// the log before abandoning a log shrink cleanly.
func (s ShrinkConfig) LogReuseWaitTimeout() time.Duration {
	return time.Duration(s.LogReuseWaitTimeoutMinutes) * time.Minute
}

// BatchDMLConfig holds the tuning parameters for the chunked UPDATE/DELETE driver.
// Like ShrinkConfig every field is optional (zero/negative → documented default) and
// global only: the values are starting points and bounds the driver's adaptive
// sizing varies. EscalationCapRows is the batch ceiling applied when the database has
// RCSI off, to keep a batch below the ~5000-lock table-escalation threshold.
type BatchDMLConfig struct {
	InitialSmallRows       int `yaml:"initial_small_rows"`        // estimated table < 100k rows
	InitialMediumRows      int `yaml:"initial_medium_rows"`       // 100k–1M rows
	InitialLargeRows       int `yaml:"initial_large_rows"`        // > 1M rows
	MinRows                int `yaml:"min_rows"`                  // batch floor
	MaxRows                int `yaml:"max_rows"`                  // batch ceiling
	EscalationCapRows      int `yaml:"escalation_cap_rows"`       // ceiling when RCSI is off (avoid table-lock escalation)
	TargetBatchSeconds     int `yaml:"target_batch_seconds"`      // ideal per-batch duration
	SelfWaitTimeoutMinutes int `yaml:"self_wait_timeout_minutes"` // max cumulative wait while stopped before a clean stop
}

// TargetBatch returns the ideal per-batch duration.
func (b BatchDMLConfig) TargetBatch() time.Duration {
	return time.Duration(b.TargetBatchSeconds) * time.Second
}

// SelfWaitTimeout returns the max cumulative wait while stopped under pressure
// before the driver stops cleanly.
func (b BatchDMLConfig) SelfWaitTimeout() time.Duration {
	return time.Duration(b.SelfWaitTimeoutMinutes) * time.Minute
}

// applyDefaults fills any unset (zero or negative) field with its default.
func (b *BatchDMLConfig) applyDefaults() {
	setIf := func(p *int, def int) {
		if *p <= 0 {
			*p = def
		}
	}
	setIf(&b.InitialSmallRows, 1000)
	setIf(&b.InitialMediumRows, 5000)
	setIf(&b.InitialLargeRows, 20000)
	setIf(&b.MinRows, 100)
	setIf(&b.MaxRows, 100000)
	setIf(&b.EscalationCapRows, 4000)
	setIf(&b.TargetBatchSeconds, 5)
	setIf(&b.SelfWaitTimeoutMinutes, 5)
}

// NotificationsConfig holds webhook and email settings.
type NotificationsConfig struct {
	WebhookURL string      `yaml:"webhook_url"`
	OnEvents   []string    `yaml:"on_events"`
	Email      EmailConfig `yaml:"email"`
}

// EmailConfig holds SMTP delivery settings. An empty Host disables the email
// channel (no-op). Port defaults to 25. Username/Password are optional (empty =
// anonymous relay); Password is injected from ${VAR} like every other secret.
type EmailConfig struct {
	Host     string   `yaml:"host"`
	Port     int      `yaml:"port"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`
	StartTLS bool     `yaml:"starttls"`
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
	// A zero blocking timeout would make the DDL react to (and the console surface) any
	// blocked session instantly — a dangerous default for an unset field. Floor these
	// timeouts to the shipped config.yaml values so omitting them is safe.
	if c.Monitoring.BlockingTimeoutMinutes <= 0 {
		c.Monitoring.BlockingTimeoutMinutes = 1
	}
	if c.Monitoring.LogDrainTimeoutMinutes <= 0 {
		c.Monitoring.LogDrainTimeoutMinutes = 30
	}
	if c.Monitoring.KillGraceSeconds <= 0 {
		c.Monitoring.KillGraceSeconds = 30
	}
	c.Shrink.applyDefaults()
	c.BatchDML.applyDefaults()
	if c.Notifications.Email.Host != "" && c.Notifications.Email.Port <= 0 {
		c.Notifications.Email.Port = 25
	}
}

// applyDefaults fills any unset (zero or negative) shrink field with its default,
// so a missing shrink: block or a partial override both yield a working config.
func (s *ShrinkConfig) applyDefaults() {
	setIf := func(p *int, def int) {
		if *p <= 0 {
			*p = def
		}
	}
	setIf(&s.InitialStepSmallMB, 100)
	setIf(&s.InitialStepMediumMB, 250)
	setIf(&s.InitialStepLargeMB, 500)
	setIf(&s.TargetChunks, 1000)
	setIf(&s.MaxStepPctOfFile, 5)
	setIf(&s.MinStepMB, 50)
	setIf(&s.MaxStepMB, 8192)
	setIf(&s.TargetBatchSeconds, 5)
	setIf(&s.MaxNoProgress, 3)
	setIf(&s.NoProgressBackoffSeconds, 30)
	setIf(&s.NoProgressBackoffMaxSeconds, 300)
	setIf(&s.SelfWaitTimeoutMinutes, 5)
	setIf(&s.LogReuseWaitTimeoutMinutes, 30)
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
	if c.Shrink.MinStepMB > c.Shrink.MaxStepMB {
		return fmt.Errorf("shrink.min_step_mb (%d) must be <= max_step_mb (%d): %w",
			c.Shrink.MinStepMB, c.Shrink.MaxStepMB, ErrInvalidConfig)
	}
	if c.BatchDML.MinRows > c.BatchDML.MaxRows {
		return fmt.Errorf("batch_dml.min_rows (%d) must be <= max_rows (%d): %w",
			c.BatchDML.MinRows, c.BatchDML.MaxRows, ErrInvalidConfig)
	}
	if e := c.Notifications.Email; e.Host != "" {
		if strings.TrimSpace(e.From) == "" || len(e.To) == 0 {
			return fmt.Errorf("notifications.email.from and at least one to are required when host is set: %w", ErrInvalidConfig)
		}
		if e.Username != "" && e.Password == "" {
			return fmt.Errorf("notifications.email.password is required when username is set: %w", ErrInvalidConfig)
		}
	}
	return nil
}
