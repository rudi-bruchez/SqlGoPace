package report

import (
	"context"
	"database/sql"
	"fmt"

	// Register the pure-Go "sqlite" driver.
	_ "modernc.org/sqlite"
)

// RunRecord is one row of run history.
type RunRecord struct {
	Manifest   string
	Outcome    string
	StartedAt  string
	FinishedAt string
	Operations int
	DurationMS int64
	Error      string
}

// History persists run records to a SQLite database.
type History struct {
	db *sql.DB
}

const historySchema = `
CREATE TABLE IF NOT EXISTS runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    manifest    TEXT NOT NULL,
    outcome     TEXT NOT NULL,
    started_at  TEXT,
    finished_at TEXT,
    operations  INTEGER,
    duration_ms INTEGER,
    error       TEXT
);`

// OpenHistory opens (creating if needed) the SQLite history database at path.
func OpenHistory(path string) (*History, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open history db: %w", err)
	}
	if _, err := db.Exec(historySchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init history schema: %w", err)
	}
	return &History{db: db}, nil
}

// Record inserts one run record.
func (h *History) Record(ctx context.Context, r RunRecord) error {
	const q = `INSERT INTO runs (manifest, outcome, started_at, finished_at, operations, duration_ms, error)
	           VALUES (?, ?, ?, ?, ?, ?, ?);`
	if _, err := h.db.ExecContext(ctx, q,
		r.Manifest, r.Outcome, r.StartedAt, r.FinishedAt, r.Operations, r.DurationMS, r.Error); err != nil {
		return fmt.Errorf("record run: %w", err)
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

// Close closes the database.
func (h *History) Close() error { return h.db.Close() }
