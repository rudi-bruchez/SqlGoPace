package report

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	// Register the pure-Go "sqlite" driver.
	_ "modernc.org/sqlite"
)

// RunRecord is one row of run history.
type RunRecord struct {
	Manifest    string
	Outcome     string
	StartedAt   string
	FinishedAt  string
	Operations  int
	DurationMS  int64
	PeakBlocked int // most sessions any one operation blocked at once during the run
	Skipped     int // operations skipped as already-satisfied (skip_if_satisfied); excludes resume-cursor skips
	Error       string
}

// History persists run records to a SQLite database.
type History struct {
	db *sql.DB
}

// schemaStatements are the table definitions applied at open. Each is idempotent
// (IF NOT EXISTS), so opening an existing database is an additive migration.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    manifest    TEXT NOT NULL,
    outcome     TEXT NOT NULL,
    started_at  TEXT,
    finished_at TEXT,
    operations  INTEGER,
    duration_ms INTEGER,
    error       TEXT
);`,
	`CREATE TABLE IF NOT EXISTS maintenance_plan (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    database     TEXT NOT NULL,
    generated_at TEXT,
    manifests    INTEGER,
    operations   INTEGER
);`,
	`CREATE TABLE IF NOT EXISTS maintenance_analysis (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_id             INTEGER NOT NULL REFERENCES maintenance_plan(id),
    category            TEXT,
    target              TEXT,
    decision            TEXT,
    reason              TEXT,
    page_count          INTEGER,
    size_mb             INTEGER,
    fragmentation       REAL,
    forwarded_percent   REAL,
    write_ratio         REAL,
    current_compression TEXT,
    chosen_compression  TEXT
);`,
}

// columnMigrations add columns that postdate a table's original definition.
// CREATE TABLE IF NOT EXISTS never alters an existing table, so a new column must be
// added with ALTER TABLE. Each is idempotent: a duplicate-column error on a database
// that already has the column is expected and ignored.
var columnMigrations = []string{
	`ALTER TABLE runs ADD COLUMN peak_blocked INTEGER;`,
	`ALTER TABLE runs ADD COLUMN skipped INTEGER;`,
}

// OpenHistory opens (creating if needed) the SQLite history database at path.
func OpenHistory(path string) (*History, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open history db: %w", err)
	}
	for _, stmt := range schemaStatements {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("init history schema: %w", err)
		}
	}
	for _, stmt := range columnMigrations {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("migrate history schema: %w", err)
		}
	}
	return &History{db: db}, nil
}

// Record inserts one run record.
func (h *History) Record(ctx context.Context, r RunRecord) error {
	const q = `INSERT INTO runs (manifest, outcome, started_at, finished_at, operations, duration_ms, peak_blocked, skipped, error)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);`
	if _, err := h.db.ExecContext(ctx, q,
		r.Manifest, r.Outcome, r.StartedAt, r.FinishedAt, r.Operations, r.DurationMS, r.PeakBlocked, r.Skipped, r.Error); err != nil {
		return fmt.Errorf("record run: %w", err)
	}
	return nil
}

// MaintenancePlanRecord is the header for one `plan` analysis run.
type MaintenancePlanRecord struct {
	Database    string
	GeneratedAt string
	Manifests   int // number of manifests written
	Operations  int // total operations across them
}

// MaintenanceDecisionRecord is one analyzed object's outcome and the metrics
// behind it, for trend analysis (oscillation, compression hold, fragmentation
// return). It maps from a maint.Decision plus its Metrics.
type MaintenanceDecisionRecord struct {
	Category             string
	Target               string
	Decision             string // the chosen command type
	Reason               string
	PageCount            int64
	SizeMB               int64
	FragmentationPercent float64
	ForwardedPercent     float64
	WriteRatio           float64
	CurrentCompression   string
	ChosenCompression    string
}

// RecordMaintenance persists one plan run and its decisions in a single
// transaction, linking each decision row to the plan via plan_id.
func (h *History) RecordMaintenance(ctx context.Context, run MaintenancePlanRecord, decisions []MaintenanceDecisionRecord) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin maintenance tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	res, err := tx.ExecContext(ctx,
		`INSERT INTO maintenance_plan (database, generated_at, manifests, operations) VALUES (?, ?, ?, ?);`,
		run.Database, run.GeneratedAt, run.Manifests, run.Operations)
	if err != nil {
		return fmt.Errorf("record maintenance plan: %w", err)
	}
	planID, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("maintenance plan id: %w", err)
	}

	const q = `INSERT INTO maintenance_analysis
        (plan_id, category, target, decision, reason, page_count, size_mb,
         fragmentation, forwarded_percent, write_ratio, current_compression, chosen_compression)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`
	for _, d := range decisions {
		if _, err := tx.ExecContext(ctx, q,
			planID, d.Category, d.Target, d.Decision, d.Reason, d.PageCount, d.SizeMB,
			d.FragmentationPercent, d.ForwardedPercent, d.WriteRatio, d.CurrentCompression, d.ChosenCompression); err != nil {
			return fmt.Errorf("record maintenance decision %q: %w", d.Target, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit maintenance tx: %w", err)
	}
	return nil
}

// Count returns the number of recorded runs.
func (h *History) Count(ctx context.Context) (int, error) {
	var n int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs;`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count runs: %w", err)
	}
	return n, nil
}

// MaintenanceCounts returns the number of recorded plan runs and analysis rows.
func (h *History) MaintenanceCounts(ctx context.Context) (plans, decisions int, err error) {
	if err = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_plan;`).Scan(&plans); err != nil {
		return 0, 0, fmt.Errorf("count maintenance plans: %w", err)
	}
	if err = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_analysis;`).Scan(&decisions); err != nil {
		return 0, 0, fmt.Errorf("count maintenance analysis: %w", err)
	}
	return plans, decisions, nil
}

// Close closes the database.
func (h *History) Close() error { return h.db.Close() }
