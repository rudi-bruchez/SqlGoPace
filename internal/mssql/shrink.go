package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// File type filters for FileSpace. They match sys.database_files.type_desc.
const (
	FileTypeRows = "ROWS" // data files (.mdf/.ndf)
	FileTypeLog  = "LOG"  // transaction log files (.ldf)
)

// FileSpace is the space accounting for one database file, in megabytes. UsedMB is
// rounded up (CEILING): it is the floor below which a file cannot be shrunk, so
// rounding down could suggest an impossible target.
type FileSpace struct {
	Name     string
	TypeDesc string
	SizeMB   int
	UsedMB   int
	FreeMB   int
}

// fileSpaceSQL reads size and used space per file of one type in the current
// database. size is in 8-KB pages (/128 = MB); FILEPROPERTY('SpaceUsed') likewise.
const fileSpaceSQL = `
SELECT name, type_desc,
       CAST(size / 128.0 AS INT)                                                 AS size_mb,
       CAST(CEILING(CAST(FILEPROPERTY(name, 'SpaceUsed') AS BIGINT) / 128.0) AS INT) AS used_mb
FROM sys.database_files
WHERE type_desc = @type
ORDER BY file_id;`

// FileSpace lists the space accounting for every file of the given type (ROWS or
// LOG) in the connected database, in file_id order. files:all expands over this.
func (c *Conn) FileSpace(ctx context.Context, fileType string) ([]FileSpace, error) {
	rows, err := c.pool.QueryContext(ctx, fileSpaceSQL, sql.Named("type", fileType))
	if err != nil {
		return nil, fmt.Errorf("read file space: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []FileSpace
	for rows.Next() {
		var f FileSpace
		if err := rows.Scan(&f.Name, &f.TypeDesc, &f.SizeMB, &f.UsedMB); err != nil {
			return nil, fmt.Errorf("scan file space: %w", err)
		}
		f.FreeMB = f.SizeMB - f.UsedMB
		out = append(out, f)
	}
	return out, rows.Err()
}

const fileSizeSQL = `SELECT CAST(size / 128.0 AS INT) FROM sys.database_files WHERE name = @name;`

// FileSizeMB reads the current size of one logical file, in megabytes. The shrink
// driver re-reads it after each chunk to measure progress and detect no-progress.
func (c *Conn) FileSizeMB(ctx context.Context, file string) (int, error) {
	var mb int
	err := c.pool.QueryRowContext(ctx, fileSizeSQL, sql.Named("name", file)).Scan(&mb)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("file %q not found in sys.database_files: %w", file, sql.ErrNoRows)
	case err != nil:
		return 0, fmt.Errorf("read file size: %w", err)
	default:
		return mb, nil
	}
}

const logReuseSQL = `SELECT recovery_model_desc, log_reuse_wait_desc FROM sys.databases WHERE name = DB_NAME();`

// LogReuse reads the connected database's recovery model and the reason its log
// cannot currently be truncated (log_reuse_wait_desc, e.g. NOTHING, LOG_BACKUP,
// ACTIVE_TRANSACTION). The log-shrink gating in the driver reads this on each poll.
func (c *Conn) LogReuse(ctx context.Context) (recoveryModel, reuseWaitDesc string, err error) {
	err = c.pool.QueryRowContext(ctx, logReuseSQL).Scan(&recoveryModel, &reuseWaitDesc)
	if err != nil {
		return "", "", fmt.Errorf("read log reuse: %w", err)
	}
	return recoveryModel, reuseWaitDesc, nil
}

// activeLogFloorSQL sums the sizes of the active VLFs: the log cannot be truncated
// below this, so it is the recoverable floor for a log shrink.
const activeLogFloorSQL = `
SELECT CAST(CEILING(ISNULL(SUM(vlf_size_mb), 0)) AS INT)
FROM sys.dm_db_log_info(DB_ID())
WHERE vlf_active = 1;`

// ActiveLogFloorMB returns the total size of the active virtual log files in the
// connected database, in megabytes — the smallest the log can be shrunk to right
// now. Zero means the log is fully truncatable (no active VLFs beyond the head).
func (c *Conn) ActiveLogFloorMB(ctx context.Context) (int, error) {
	var mb int
	if err := c.pool.QueryRowContext(ctx, activeLogFloorSQL).Scan(&mb); err != nil {
		return 0, fmt.Errorf("read active log floor: %w", err)
	}
	return mb, nil
}
