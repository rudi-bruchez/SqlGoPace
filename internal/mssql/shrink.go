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
	FileID   int
	TypeDesc string
	SizeMB   int
	UsedMB   int
	FreeMB   int
}

// FileGrowth is one file's autogrowth configuration, from sys.database_files.
// Growth is kept in its raw form because the unit depends on IsPercent, and because
// 0 (autogrowth off) must stay distinguishable from an increment that rounds to
// under 1 MB. Use the helpers rather than reading it directly.
type FileGrowth struct {
	Name      string
	TypeDesc  string
	SizeMB    int
	IsPercent bool
	// Growth is a whole-number percentage when IsPercent, otherwise a count of 8-KB
	// pages. 0 means the file is fixed size and never grows.
	Growth int
	// MaxSizeMB is -1 when the file may grow until the disk is full, and 0 when no
	// growth is allowed at all. Otherwise it is the cap, in megabytes.
	MaxSizeMB int
}

// GrowthDisabled reports whether the file is fixed size and can never autogrow.
func (f FileGrowth) GrowthDisabled() bool { return f.Growth == 0 }

// Unlimited reports whether the file may grow until the disk fills, so its headroom
// cannot be derived from the catalog alone.
func (f FileGrowth) Unlimited() bool { return f.MaxSizeMB < 0 && !f.GrowthDisabled() }

// NextGrowthMB is the size of the next autogrowth event at the file's current size,
// in megabytes: the percentage applied to SizeMB, or the fixed increment. It is 0 when
// growth is disabled, and may round to 0 for an increment under 1 MB.
func (f FileGrowth) NextGrowthMB() int {
	switch {
	case f.GrowthDisabled():
		return 0
	case f.IsPercent:
		return f.SizeMB * f.Growth / 100
	default:
		return f.Growth / 128 // 8-KB pages to MB
	}
}

// HeadroomMB is how much the file may still grow, in megabytes: 0 when growth is
// disabled, and the distance to MaxSizeMB when capped. Unlimited files report 0 with
// ok=false, because the real bound is disk space, which the catalog cannot see.
func (f FileGrowth) HeadroomMB() (mb int, ok bool) {
	switch {
	case f.GrowthDisabled():
		return 0, true
	case f.Unlimited():
		return 0, false
	default:
		return max(f.MaxSizeMB-f.SizeMB, 0), true
	}
}

// fileGrowthSQL reads the autogrowth configuration per file of one type. size and
// max_size are in 8-KB pages (/128 = MB); max_size keeps its sentinels (-1 = until the
// disk is full, 0 = no growth), so only positive values are converted. growth is left
// raw: its unit depends on is_percent_growth.
const fileGrowthSQL = `
SELECT name, type_desc,
       CAST(size / 128.0 AS INT) AS size_mb,
       is_percent_growth,
       growth,
       CASE WHEN max_size <= 0 THEN max_size ELSE CAST(max_size / 128.0 AS INT) END AS max_size_mb
FROM sys.database_files
WHERE type_desc = @type
ORDER BY file_id;`

// FileGrowths lists the autogrowth configuration of every file of the given type
// (ROWS or LOG) in the connected database, in file_id order.
func (c *Conn) FileGrowths(ctx context.Context, fileType string) ([]FileGrowth, error) {
	rows, err := c.pool.QueryContext(ctx, fileGrowthSQL, sql.Named("type", fileType))
	if err != nil {
		return nil, fmt.Errorf("read file growth: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []FileGrowth
	for rows.Next() {
		var f FileGrowth
		if err := rows.Scan(&f.Name, &f.TypeDesc, &f.SizeMB, &f.IsPercent, &f.Growth, &f.MaxSizeMB); err != nil {
			return nil, fmt.Errorf("scan file growth: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// fileSpaceSQL reads size and used space per file of one type in the current
// database. size is in 8-KB pages (/128 = MB); FILEPROPERTY('SpaceUsed') likewise.
const fileSpaceSQL = `
SELECT name, file_id, type_desc,
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
		if err := rows.Scan(&f.Name, &f.FileID, &f.TypeDesc, &f.SizeMB, &f.UsedMB); err != nil {
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

// TailObject is the user object owning the physically-last allocated page of a data file:
// the object DBCC SHRINKFILE must relocate past, and the binding constraint on how far the
// file can shrink. PageFromEnd is how many pages from the file end that page sits (0 = the
// very last page).
type TailObject struct {
	ObjectID    int64
	Schema      string
	Table       string
	IndexID     int
	PageFromEnd int
}

// tailObjectSQL walks backward from the last page of @file_id, skipping pages with no user
// object (free/unallocated pages and allocation-bitmap pages return NULL object_id), and
// returns the first page owned by a user object. The size read and the walk are one batch,
// so the file size is consistent for the walk. Each page is read from sys.dm_db_page_info
// exactly once — object_id and index_id are captured into variables and the result row is
// built from them, so a concurrent deallocation cannot make the returned object_id NULL
// between two reads. Names are resolved from the captured @obj and may be NULL if the object
// is dropped in the meantime (recorded as object_id with empty schema/table). SQL 2019+.
const tailObjectSQL = `
SET NOCOUNT ON;
DECLARE @file_id int = @fid, @max_back int = @maxback;
DECLARE @last_page_id int, @page_id int, @floor int, @obj int, @idx int;
SELECT @last_page_id = CAST(size AS int) - 1 FROM sys.database_files WHERE file_id = @file_id;
IF @last_page_id IS NULL OR @last_page_id < 0 RETURN;
SET @page_id = @last_page_id;
SET @floor = @last_page_id - @max_back;
IF @floor < 0 SET @floor = 0;
WHILE @page_id >= @floor
BEGIN
    SELECT @obj = object_id, @idx = index_id
    FROM sys.dm_db_page_info(DB_ID(), @file_id, @page_id, 'LIMITED');
    IF @obj IS NOT NULL
    BEGIN
        SELECT @obj                          AS object_id,
               OBJECT_SCHEMA_NAME(@obj)      AS schema_name,
               OBJECT_NAME(@obj)             AS object_name,
               @idx                          AS index_id,
               @last_page_id - @page_id      AS page_from_end;
        RETURN;
    END
    SET @page_id -= 1;
END`

// FindTailObject walks backward from the last page of fileID via sys.dm_db_page_info,
// returning the first page owned by a user object. It scans at most maxPagesBack pages;
// found=false means it reached that bound (or the file end) without hitting an allocated
// page. SQL 2019+ only — the caller gates on version before calling. Names may be empty if
// the object was dropped mid-walk. It takes no transaction locks (only brief page latches).
func (c *Conn) FindTailObject(ctx context.Context, fileID, maxPagesBack int) (TailObject, bool, error) {
	var (
		t            TailObject
		schema, name sql.NullString
	)
	err := c.pool.QueryRowContext(ctx, tailObjectSQL,
		sql.Named("fid", fileID), sql.Named("maxback", maxPagesBack)).
		Scan(&t.ObjectID, &schema, &name, &t.IndexID, &t.PageFromEnd)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TailObject{}, false, nil
	case err != nil:
		return TailObject{}, false, fmt.Errorf("find tail object (file_id %d): %w", fileID, err)
	default:
		t.Schema, t.Table = schema.String, name.String
		return t, true, nil
	}
}
