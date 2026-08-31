# SQL Server Shrink: a complete technical reference

> Raw research synthesis, kept as historical source material: it was the technical basis for
> designing the shrink driver in SqlGoPace and for writing a technical article. It is an input
> to decisions already made, not a specification, and it is not maintained. Translated from
> the original French.

***

## 1. Overview: what is a shrink?

**Shrink** covers the operations that reduce the physical size of a SQL Server database's
files on disk. There are two main commands:[^1][^2]

- `DBCC SHRINKDATABASE` acts on every file of the database, data and log alike, in a single
  command.
- `DBCC SHRINKFILE` targets one specific file, by logical name or `file_id`, with fine control.

Microsoft's own documentation is explicit: **a shrink should not be treated as routine
maintenance**. Files that grow because of the database's normal activity do not need to be
reduced.[^1]

***

## 2. How it works, and the algorithm

### 2.1 The page-movement principle (data file)

The SQL Server engine organises data files into 8 KB **pages**, grouped into **extents** of
eight pages (64 KB). During a data-file shrink:[^3][^4]

1. SQL Server computes a **target extent**, the boundary beyond which the file will be
   truncated.
2. A **GAM scan** (Global Allocation Map) runs from the start of the file to identify the
   extents allocated beyond that target.[^5]
3. For each allocated extent beyond the target, SQL Server moves the pages into the first
   free space available **before** the target.
4. Once the region to be released is empty, the file is truncated and the space returned to
   the OS.[^3]

The movement is performed **in batches of about 32 pages** since SQL Server 2005. Each batch
is an independent transaction: if the operation is interrupted, only the batch in flight is
rolled back and the earlier work is kept. That is what makes the operation
**restartable**.[^6][^1]

### 2.2 Behaviour by page type

- **Ordinary data pages:** moved as a delete and insert pair.[^5]
- **Index pages:** moved without preserving logical order, which fragments immediately.[^7]
- **BLOB pages (TEXT/IMAGE/LOB):** particularly expensive. A full IAM scan of the whole
  associated BLOB chain is triggered for every BLOB page encountered. On a 1 TB database with
  500 GB of BLOB, that generates a massive volume of I/O.[^5]
- **LOB pages inside compressed columnstores:** **not processed** by `DBCC SHRINKFILE` or
  `DBCC SHRINKDATABASE`, a known and documented limitation.[^1]

### 2.3 What a shrink does NOT do

`DBCC SHRINKFILE` works **at extent level**, not at the level of the individual page:[^8]

- It does not compact partially filled pages.
- It does not merge mixed extents.
- It does not remove empty pages inside an allocated extent.

The consequence: if a database holds many extents with only one or two pages in use, the
shrink will be ineffective. It cannot release as much space as the logically empty space
suggests.[^8]

### 2.4 The available shrink modes

| Mode | Effect on pages | Space returned to the OS | Fragmentation |
|---|---|---|---|
| Normal (`target_size`) | Moves pages from the end toward the front | Yes | High |
| `TRUNCATEONLY` | No movement | Yes, the trailing free space only | None |
| `NOTRUNCATE` | Moves pages toward the front | No | High |
| `EMPTYFILE` | Migrates all content to the other files of the filegroup | N/A | High |

`TRUNCATEONLY` is the only option that generates no fragmentation: it simply cuts the end of
the file off when that end is empty. It is the method to prefer when the free space is already
at the end of the file, after a large `DROP TABLE` or `TRUNCATE TABLE`.[^9][^1]

***

## 3. Why to avoid a shrink: the fundamental problems

### 3.1 Immediate, massive index fragmentation

This is the most serious problem. The algorithm places moved pages **in the first free space
available**, taking no account of the logical order of index keys. The result is index
fragmentation close to 100% across the whole affected region. That fragmentation:[^4][^10][^7]

- Degrades the performance of range scans.
- Increases logical and physical reads for every batch operation.
- Requires an index rebuild afterwards, which consumes space again and can cancel out the
  benefit.

### 3.2 Massive transaction-log generation

Every page move is **fully logged**. Moving 1 GB of data from the end to the beginning of the
file generates at least 1 GB of entries in the `.ldf`. On a heavy shrink that can:

- Blow up the size of the log, potentially on the same disk.
- Trigger auto-growth of the log.
- Cause intense `WRITELOG` waits.
- In `FULL` recovery, block log truncation when the backups cannot keep up.

### 3.3 The auto-shrink and auto-grow death spiral

If the database needs its free space to function, for future inserts, a shrink merely forces a
later auto-growth, often in small and inefficient increments:[^7]

- Every auto-growth creates new, badly sized VLFs.
- File fragmentation at the filesystem level accumulates.
- The shrink, grow, shrink cycle is pure waste of I/O and CPU.[^11][^7]

Paul Randal, a former Microsoft engineer who wrote the storage engine code, described
`AUTO_SHRINK` as an option that should be removed from the product, being unable to identify
any legitimate use case for it.[^7]

### 3.4 Buffer pool pollution

During a shrink, SQL Server reads and writes pages massively, and they pass through the
**buffer pool**. Hot pages, the ones application queries touch frequently, are evicted from
the cache to make room for the pages being moved, degrading the performance of every
concurrent query.[^7]

### 3.5 Legitimate cases for a shrink

A shrink is only justified when space is being released **permanently and in bulk**:[^12]

- After purging millions of rows of history that will not be replaced.
- After dropping large unused tables or indexes.
- During a migration, or a permanent reduction in the database's scope.
- Never as recurring, scheduled maintenance.

***

## 4. Shrinking the data file: detailed behaviour

### 4.1 Preconditions

- The computed free space must exceed the target size.[^1]
- `DBCC SHRINKFILE` cannot reduce a file below the size needed to hold the data actually
  present: if 7 MB are used inside a 10 MB file, a 6 MB target yields 7 MB.[^13]
- Data sitting in extents near the end of the file, however little of it, can severely limit
  the reduction.

### 4.2 Estimating the reclaimable space before the operation

```sql
SELECT
    name                                                            AS file_name,
    physical_name                                                   AS physical_path,
    size                                                            AS total_size_pages,
    CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)                    AS used_pages,
    size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)             AS free_pages,
    (size / 128.0)                                                  AS total_size_mb,
    (CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT) / 128.0)          AS used_space_mb,
    ((size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)) / 128.0) AS free_space_mb
FROM sys.database_files
WHERE type_desc = 'ROWS';
```

### 4.3 Behaviour while pages are moving

The operation is **online**: other users can read and write while the shrink runs. SQL Server
acquires locks page by page as it moves them, not an exclusive lock on the whole database.
`Sch-M` (schema modify) locks are nevertheless needed while manipulating the IAM (Index
Allocation Map) pages, which conflicts with the `Sch-S` (schema stability) locks held by
active queries.[^14][^8][^1]

The shrink works **in whole extents** rather than in individual pages, so it cannot release
the space of an extent in which a single page is in use.[^8]

***

## 5. Shrinking the transaction log: specific behaviour

### 5.1 Internal architecture: the VLFs

The `.ldf` log file is divided into **virtual log files (VLFs)**. These are internal logical
units whose size SQL Server determines dynamically:[^15][^16]

- When the file is created or extended, SQL Server allocates VLFs whose size depends on the
  growth increment.
- A VLF is either `active`, holding live log entries, or `inactive`, already backed up or
  truncated.

Log fragmentation, meaning too many VLFs that are too small, slows down backups, crash
recovery and restores.[^17][^18]

### 5.2 The fundamental difference from a data file

**A log shrink moves no pages.** It can only **remove the inactive VLFs at the end of the
file**. It is pure truncation, not compaction.[^19]

The direct consequence: if the active VLF, the one holding the active portion of the log, sits
at the end of the file, **the shrink is impossible**. SQL Server cannot shrink the log past
the last active VLF.[^20]

### 5.3 Preconditions for a log shrink

A log shrink only works if the VLFs at the end of the file are **inactive**. To free
them:[^21][^19]

| Recovery model | Condition for log truncation |
|---|---|
| `SIMPLE` | Automatic after each `CHECKPOINT` |
| `FULL` | After each transaction-log backup |
| `BULK_LOGGED` | After each log backup |

**The correct procedure for a log shrink in FULL recovery:**

```sql
-- 1. Back up the log so it can be truncated
BACKUP LOG [MyDatabase] TO DISK = 'NUL'; -- or to a real backup file

-- 2. Check the free space in the log
DBCC SQLPERF(LOGSPACE);

-- 3. Shrink the log file
USE [MyDatabase];
DBCC SHRINKFILE (N'MyDatabase_log', 512); -- target in MB

-- 4. Repeat if the space was not fully released
```

**The procedure in SIMPLE recovery:**

```sql
USE [MyDatabase];
CHECKPOINT;
DBCC SHRINKFILE (N'MyDatabase_log', 512);
```

### 5.4 What a shrink does to the VLFs

A shrink followed by a re-growth of the log produces badly sized VLFs. The best practice after
a log shrink is to pre-allocate the log to its target size in a single `ALTER DATABASE`, which
yields a minimal number of well-sized VLFs. Repeated auto-growths in small increments create
dozens or hundreds of small VLFs, and beyond about 50 VLFs that becomes critical.[^18][^22]

```sql
-- After the shrink, recreate the log cleanly
ALTER DATABASE [MyDatabase]
MODIFY FILE (NAME = N'MyDatabase_log', SIZE = 8000MB); -- in one step
```

### 5.5 `TRUNCATEONLY` on the log

The `TRUNCATEONLY` option **does work on the log**: it removes the inactive VLFs at the end of
the file with no movement at all. It is the non-destructive equivalent of a shrink for the
log.[^23]

***

## 6. Concurrency and locking problems

### 6.1 The `Sch-S` and `Sch-M` locks

A shrink requires a `Sch-M` (schema modify) lock to manipulate the IAM pages. That lock is
**incompatible** with the `Sch-S` (schema stability) locks every active query holds on its
tables. The result is a blocking chain:[^14][^1]

```
Active query  → holds Sch-S
   ↓ blocks
Shrink        → waits for Sch-M
   ↓ blocks
Every new query → queues behind the shrink
```

This "blocking train" can paralyse all application activity.[^14]

### 6.2 Blocking by snapshot transactions (RCSI and SI)

Transactions using a row-versioning isolation level, Read Committed Snapshot Isolation or
Snapshot Isolation, **block the shrink**:[^14][^1]

```
DBCC SHRINKFILE for file ID 1 is waiting for the snapshot
transaction with timestamp 15 and other snapshot transactions linked to
timestamp 15 or with timestamps older than 109 to finish.
```

That message, informational error 5203 in the SQL Server log, is written every five minutes
for the first hour, then hourly. To identify the blocking transaction:[^1]

```sql
SELECT transaction_sequence_num, first_snapshot_sequence_num, *
FROM sys.dm_tran_active_snapshot_database_transactions
WHERE transaction_sequence_num < 109; -- replace with the timestamp from the message
```

### 6.3 `WAIT_AT_LOW_PRIORITY` (SQL Server 2022 and later)

Since SQL Server 2022, `WITH WAIT_AT_LOW_PRIORITY` lets the shrink acquire the `Sch-M` lock at
low priority:[^24][^1]

```sql
DBCC SHRINKFILE (5, 1024)
WITH WAIT_AT_LOW_PRIORITY (ABORT_AFTER_WAIT = SELF);
```

Its behaviour:[^1]

- New queries are **not** blocked by the shrink's wait.
- If the shrink cannot obtain the `Sch-M` lock within a minute, the default, it expires
  **silently**.
- Error **49516** is written to the SQL Server log on expiry.
- `ABORT_AFTER_WAIT = SELF`: the shrink cancels itself when the timeout expires.
- `ABORT_AFTER_WAIT = BLOCKERS`: the shrink kills the blocking sessions, which requires
  `ALTER ANY CONNECTION`.

For SqlGoPace: detect the server version before using this option, SQL Server 2022 and later
only. On error 49516, wait a few minutes and retry.

***

## 7. Errors and messages worth knowing

| Error or message | Type | Meaning | Action |
|---|---|---|---|
| **5202** | Informational (SQL Server log) | `SHRINKDATABASE` blocked by a snapshot transaction | Wait, or kill the blocking transaction |
| **5203** | Informational (SQL Server log) | `SHRINKFILE` blocked by a snapshot transaction | The same. Repeated every 5 minutes for an hour, then hourly |
| **49516** | Error, level 16 | `WAIT_AT_LOW_PRIORITY` timeout: the `Sch-M` lock could not be obtained | Retry the operation after a few minutes |
| **No visible reduction** | Normal behaviour | Not enough free space, or free space not located at the end of the file | Check with `sys.database_files` and `FILEPROPERTY` |
| **Log will not shrink** | Normal behaviour | Active VLFs at the end of the file | Back up the log (FULL) or run `CHECKPOINT` (SIMPLE) |
| **9002** | Error | Transaction log full | The shrink itself generates log; reduce the batch size (step size) |
| **Possible corruption** | Critical | A shrink interrupted abruptly | Check with `DBCC CHECKDB`. Completed work is kept; only the active batch is rolled back |

***

## 8. Monitoring a shrink completely

### 8.1 Live progress

```sql
-- Progress and estimated time remaining
SELECT
    session_id,
    command,
    status,
    percent_complete,
    estimated_completion_time / 1000 / 60          AS estimated_minutes_left,
    total_elapsed_time / 1000 / 60                 AS elapsed_minutes,
    wait_type,
    wait_time / 1000                               AS wait_seconds,
    blocking_session_id,
    last_wait_type
FROM sys.dm_exec_requests
WHERE command IN ('DbccFilesCompact', 'DbccSpaceReclaim')
   OR command LIKE 'DBCC%';
```

> **Note:** `DbccFilesCompact` corresponds to `DBCC SHRINKFILE`, `DbccSpaceReclaim` to
> `DBCC SHRINKDATABASE`. Both `percent_complete` and `estimated_completion_time` fluctuate
> heavily in the presence of BLOB pages or heavy fragmentation.

### 8.2 Monitoring I/O during the operation

```sql
-- I/O latency per file (capture as a delta)
SELECT
    DB_NAME(fs.database_id)                           AS [Database],
    mf.physical_name,
    fs.io_stall_read_ms,
    fs.io_stall_write_ms,
    fs.io_stall,
    fs.num_of_reads,
    fs.num_of_writes,
    CASE WHEN fs.num_of_reads = 0 THEN 0
         ELSE fs.io_stall_read_ms / fs.num_of_reads END  AS avg_read_ms,
    CASE WHEN fs.num_of_writes = 0 THEN 0
         ELSE fs.io_stall_write_ms / fs.num_of_writes END AS avg_write_ms
FROM sys.dm_io_virtual_file_stats(NULL, NULL) fs
JOIN sys.master_files mf
    ON fs.database_id = mf.database_id AND fs.file_id = mf.file_id
WHERE DB_NAME(fs.database_id) = DB_NAME()
ORDER BY fs.io_stall DESC;
```

Indicative alert thresholds:[^25]

- Reads: average latency above 20 to 25 ms means significant I/O pressure.
- Writes: above 5 to 10 ms means the subsystem is saturating, especially for the log.

### 8.3 Monitoring the wait statistics

```sql
-- Wait types that matter during a shrink
SELECT wait_type, wait_time_ms, waiting_tasks_count,
       wait_time_ms / NULLIF(waiting_tasks_count, 0) AS avg_wait_ms
FROM sys.dm_os_wait_stats
WHERE wait_type IN (
    'PAGEIOLATCH_EX', 'PAGEIOLATCH_SH',   -- data I/O pressure
    'WRITELOG',                            -- log write pressure
    'LCK_M_SCH_M',                         -- waiting for the Sch-M lock (shrink blocked)
    'LCK_M_SCH_M_LOW_PRIORITY',            -- waiting for Sch-M in low-priority mode
    'LCK_M_SCH_M_ABORT_BLOCKERS'           -- low-priority mode with BLOCKERS active
)
ORDER BY wait_time_ms DESC;
```

### 8.4 Monitoring the transaction log

```sql
-- Space used in the log
SELECT
    name,
    log_reuse_wait_desc,                    -- why the log cannot be truncated
    log_size_mb     = size * 8.0 / 1024,
    log_used_mb     = FILEPROPERTY(name, 'SpaceUsed') * 8.0 / 1024,
    recovery_model_desc
FROM sys.databases
WHERE name = DB_NAME();

-- Detail from the DMV (SQL Server 2016 and later)
SELECT
    database_id,
    total_log_size_mb       = total_log_size_bytes / 1048576.0,
    used_log_space_mb       = used_log_space_in_bytes / 1048576.0,
    used_log_space_pct      = used_log_space_in_percent,
    log_space_since_backup  = log_space_in_bytes_since_last_backup / 1048576.0
FROM sys.dm_db_log_space_usage;

-- VLF contents
DBCC LOGINFO;
```

### 8.5 Monitoring blocking

```sql
-- Sessions blocked by, or blocking, the shrink
SELECT
    r.session_id,
    r.blocking_session_id,
    r.command,
    r.status,
    r.wait_type,
    r.wait_time / 1000 AS wait_seconds,
    s.login_name,
    s.program_name,
    t.text AS sql_text
FROM sys.dm_exec_requests r
JOIN sys.dm_exec_sessions s ON r.session_id = s.session_id
CROSS APPLY sys.dm_exec_sql_text(r.sql_handle) t
WHERE r.blocking_session_id > 0
   OR r.command LIKE 'DBCC%';

-- Active snapshot transactions, to diagnose a 5202/5203 block
SELECT
    transaction_id,
    transaction_sequence_num,
    first_snapshot_sequence_num,
    elapsed_time_seconds,
    is_snapshot,
    session_id
FROM sys.dm_tran_active_snapshot_database_transactions
ORDER BY transaction_sequence_num;
```

### 8.6 File size and free space

```sql
-- Summary of the current database's files
SELECT
    name,
    type_desc,
    physical_name,
    size / 128.0                                                   AS size_mb,
    CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT) / 128.0           AS used_mb,
    (size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)) / 128.0  AS free_mb,
    CAST(
        (size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS FLOAT)) / size * 100
    AS DECIMAL(5,1))                                               AS free_pct
FROM sys.database_files
ORDER BY type_desc, file_id;
```

***

## 9. Managing the step size dynamically (chunking)

### 9.1 Why chunking is necessary

A monolithic shrink on a large file:

- generates a massive transaction, so the log explodes;
- saturates the I/O subsystem;
- blocks other sessions for long stretches;
- is very hard to interrupt cleanly.

Chunking, taking `Invoke-DbaDbShrink -StepSize` as the inspiration, breaks the operation into
a loop of successive `DBCC SHRINKFILE` calls whose target size is decremented at each
iteration.

### 9.2 Calibrating the step size

**A starting heuristic:**

| Volume to reclaim | Recommended step size |
|---|---|
| Under 5 GB | 100 to 250 MB |
| 5 to 50 GB | 250 to 500 MB |
| Over 50 GB | 500 MB to 1 GB |
| FULL recovery, backups under 15 minutes apart | No more than the log volume one backup frees |

**Signals to reduce the step size:**

- `WRITELOG` average wait above 10 ms: the log is under pressure.
- `PAGEIOLATCH_EX` average wait above 20 ms: the data disk is saturated.
- `blocking_session_id` greater than zero for more than 30 seconds: sessions are blocked.

**Signals that allow increasing it:**

- I/O latency under 5 ms.
- No significant wait type.
- `log_space_in_bytes_since_last_backup` staying low.

### 9.3 A Go implementation pattern for SqlGoPace

```go
// Pseudo-code for the shrink loop with dynamic adjustment
for currentTarget > finalTarget {
    nextTarget := currentTarget - stepSizeMB
    if nextTarget < finalTarget {
        nextTarget = finalTarget
    }

    startTime := time.Now()
    err := executeShrinkFile(db, fileName, nextTarget)
    elapsed := time.Since(startTime)

    // Measure the waits produced by this batch
    waits := queryWaitStats(db)
    ioStats := queryIOStats(db)

    // Adjust dynamically
    if waits.WRITELOG > 10 || waits.PAGEIOLATCH_EX > 20 {
        stepSizeMB = max(stepSizeMB/2, minStepSizeMB)
    } else if elapsed < targetBatchDuration && waits.AllOK() {
        stepSizeMB = min(stepSizeMB*2, maxStepSizeMB)
    }

    logBatch(currentTarget, nextTarget, elapsed, stepSizeMB, waits, ioStats)
    currentTarget = nextTarget
}
```

***

## 10. Watching the log during a data shrink

This is a critical point that is often overlooked: **a data-file shrink generates log of its
own**. It has to be watched continuously:

```sql
-- Poll this during the operation
SELECT
    used_log_space_in_percent,
    used_log_space_in_bytes / 1048576.0 AS used_log_mb,
    log_space_in_bytes_since_last_backup / 1048576.0 AS since_backup_mb
FROM sys.dm_db_log_space_usage;
```

If `used_log_space_in_percent` goes past 70 to 80% during the shrink:

- in FULL recovery, trigger a log backup immediately;
- in SIMPLE recovery, issue a `CHECKPOINT`;
- reduce the step size for the next batch.

***

## 11. What to do about each problem

| Problem | Diagnosis | Action |
|---|---|---|
| The file does not shrink | `sys.database_files` shows free_mb near zero | There is no real free space; the operation is pointless |
| The log does not shrink | `sys.databases.log_reuse_wait_desc` | FULL: back up the log. SIMPLE: `CHECKPOINT` |
| Shrink blocked (5202/5203) | `sys.dm_tran_active_snapshot_database_transactions` | Wait for the transaction to end, or kill it carefully |
| `WAIT_AT_LOW_PRIORITY` timeout (49516) | The SQL Server log | Wait a few minutes, identify the long-running queries, retry |
| I/O saturation (`PAGEIOLATCH_EX`) | `sys.dm_io_virtual_file_stats` | Reduce the step size, move the operation to a quiet window |
| Log full during the shrink (9002) | `sys.dm_db_log_space_usage` | Stop the shrink, back up the log, resume with a smaller step |
| Excessive slowness (BLOB/LOB pages) | `percent_complete` advances very slowly | Normal for tables with `varbinary(max)`, `text` or `image`; expect hours |
| Shrink blocked by an index rebuild | `sys.dm_exec_requests` with `wait_type = LCK_M_SCH_M` | Never run one alongside an index rebuild; schedule them apart |
| Fragmentation after the shrink | `sys.dm_db_index_physical_stats` | Rebuild the indexes afterwards; budget twice the total time |

***

## 12. Operational best practice

1. **Always measure first.** Check `free_pct` and the genuinely free space with
   `sys.database_files`. A shrink that moves zero pages is instantaneous; one that moves
   50 GB can take hours.

2. **Prefer `TRUNCATEONLY` where it applies.** If the free space is already at the end of the
   file, after a recent `TRUNCATE TABLE` for instance, `TRUNCATEONLY` is instantaneous and
   fragments nothing.[^9]

3. **Never run several files of the same filegroup at once.** Contention on the system tables
   produces extra delays and blocking.[^1]

4. **Plan the index rebuild** that follows any shrink which moved pages. That step is
   mandatory to restore performance, and it needs free space: budget 20 to 30% extra.[^19]

5. **Turn `AUTO_SHRINK` off on every production database:**

   ```sql
   ALTER DATABASE [MyDatabase] SET AUTO_SHRINK OFF;
   ```

6. **After a log shrink, pre-allocate the log** to its target size in one step, to control the
   number of VLFs.[^22]

7. **Use `WAIT_AT_LOW_PRIORITY` on SQL Server 2022 and later** to avoid the blocking train.
   Watch for error 49516 in the SQL Server log and retry automatically.[^24][^1]

8. **On SQL Server 2022 and later**, `MAXDOP` is supported for some shrink operations, which
   allows the page movement to be parallelised. Use it carefully so as not to saturate I/O.

***

## Official references and documentation

- [DBCC SHRINKFILE, Microsoft Learn](https://learn.microsoft.com/en-us/sql/t-sql/database-console-commands/dbcc-shrinkfile-transact-sql)
- [Manage Transaction Log File Size, Microsoft Learn](https://learn.microsoft.com/en-us/sql/relational-databases/logs/manage-the-size-of-the-transaction-log-file)
- [How It Works: More on DBCC Shrink* Activities, by Bob Dorr, Microsoft CSS](https://techcommunity.microsoft.com/blog/sqlserverstudioteam/how-it-works-more-on-dbcc-shrink-activities/315499)
- [Turn AUTO_SHRINK off!!, by Paul Randal, Microsoft SQL Server Team](https://techcommunity.microsoft.com/blog/sqlserver/turn-auto-shrink-off/383234)
- [Invoke-DbaDbShrink, dbatools](https://dbatools.io/Invoke-DbaDbShrink/)
- [WAIT_AT_LOW_PRIORITY with shrink, a SQL Server 2022 feature](https://learn.microsoft.com/en-us/sql/t-sql/database-console-commands/dbcc-shrinkfile-transact-sql#wait_at_low_priority-with-shrink-operations)

---

## References

1. [DBCC SHRINKFILE (Transact-SQL) - SQL Server - Microsoft Learn](https://learn.microsoft.com/en-us/sql/t-sql/database-console-commands/dbcc-shrinkfile-transact-sql?view=sql-server-ver17) - Moves allocated pages from a data file's end to unallocated pages in a file's front with or without ...

2. [Master SQL Server SHRINKDB: Pros, Cons, Best Practices](https://stevestedman.com/2024/07/shrinking-databases/) - Explore the pros and cons of using SHRINKDB in SQL Server. Learn best practices for optimizing datab...

3. [Shrink a File - SQL Server](https://learn.microsoft.com/en-us/sql/relational-databases/databases/shrink-a-file?view=sql-server-ver17) - Learn how to shrink a data or log file in SQL Server by using SQL Server Management Studio or Transa...

4. [Shrinking Database Data Files - Simple SQL Server](https://simplesqlserver.com/2016/01/19/shrinking-database-data-files/) - The most common way to shrink a file is to have it reorganize pages before releasing free space, so ...

5. [How It Works: More on DBCC Shrink* Activities | Microsoft Community Hub](https://techcommunity.microsoft.com/blog/sqlserversupport/how-it-works-more-on-dbcc-shrink-activities/315499) - First published on MSDN on Jun 18, 2008 My peers are starting to tease me about becoming a dbcc shri...

6. [DBCC SHRINKFILE](https://sqldeepdives.blogspot.com/2015/08/dbcc-shrinkfile.html) - Select Query

7. [Turn AUTO_SHRINK off!!](https://techcommunity.microsoft.com/blog/sqlserver/turn-auto-shrink-off/383234) - First published on MSDN on Mar 28, 2007 This week's topic is data file shrinking.

8. [收缩数据文件](https://blog.csdn.net/weixin_30384031/article/details/98269907) - 文章浏览阅读306次。在执行DBCC ShrinkFile命令，收缩数据文件的时候，SQL Server首先将文件尾部的区（extent）移动到文件的开头，文件结尾的空闲的Disk空间会被收缩，释放给...

9. [Execute SQL Server DBCC SHRINKFILE Without Causing ...](https://www.mssqltips.com/sqlservertip/4368/execute-sql-server-dbcc-shrinkfile-without-causing-index-fragmentation/) - Learn how to execute SQL Server DBCC SHRINKFILE without causing index fragmentation and example cond...

10. [Shrink a File - SQL Server](https://learn.microsoft.com/sr-cyrl-rs/sql/relational-databases/databases/shrink-a-file?view=sql-server-ver17) - Learn how to shrink a data or log file in SQL Server by using SQL Server Management Studio or Transa...

11. [Autoshrink set to on](https://databasehealth.com/database-overview/database-warnings/autoshrink-set-to-on/) - Learn why Autoshrink set to on is a bad idea on SQL Server, and some better options for shrinking da...

12. [Why Shrinking SQL Server Databases Is Almost Always a Terrible Idea](https://www.linkedin.com/posts/markvarnas_shrinking-sql-server-databases-is-not-a-good-activity-7307400335709384704-WWSh) - Why Shrinking SQL Server Databases Is Almost Always a Terrible Idea DBAs often think shrinking SQL S...

13. [DBCC SHRINKFILE - Transact-SQL Reference Documentation](https://documentation.help/tsqlref/ts_dbcc_8b51.htm)


Objectif : ajouter une commande et un suivi de shrink dans ...

15. [SQL-Server: What are VLF's and why should I care about them?](https://www.dbi-services.com/blog/sql-server-what-are-vlfs-and-why-should-i-care-about-them/) - The answer is quite simple: Too many virtual log files can slow down the recovery time of a database...

16. [Setting a Fixed Size for Transaction Log VLFs](https://www.sql.kiwi/2023/11/fixed-size-vlfs/) - Using undocumented procedure sp_start_fixed_vlf so a SQL Server 2022 database uses fixed sized VLFs....

17. [Stairway to Transaction Log Management in SQL Server, Level 7](https://www.sqlservercentral.com/steps/stairway-to-transaction-log-management-in-sql-server-level-7-dealing-with-excessive-log-growth) - This level will examine the most common problems and forms of mismanagement that lead to excessive g...

18. [Log File Fragmentation A hidden cause of poor performance](https://sqlconsulting.com/archives/log-file-fragmentation-a-hidden-cause-of-poor-performance/) - Internal fragmentation in a database log file is a frequently overlooked cause of poor performance i...

19. [Manage Transaction Log File Size - SQL Server - Microsoft Learn](https://learn.microsoft.com/en-us/sql/relational-databases/logs/manage-the-size-of-the-transaction-log-file?view=sql-server-ver17) - Shrinking a log file removes one or more VLFs that hold no part of the logical log (that is, inactiv...

20. [Lisenet.com :: Linux | Security | Networking | Admin Blog](https://www.lisenet.com/2013/shrink-mssql-logs-and-rebuild-database-table-indexes/) - Technical admin blog about Linux, Security, Networking and IT.

21. [SQL Server Recovery Models & Log Truncation](https://www.sqlbackupmaster.com/wordpress/2020/07/23/sql-server-recovery-models-log-truncation/) - We get a fair number of questions from SQL Backup Master users about transaction log files, often ac...

22. [TransactionLog. VLF Fragmentation. – SQLServerCentral Forums](https://www.sqlservercentral.com/forums/topic/transactionlog-vlf-fragmentation) - TransactionLog. VLF Fragmentation. Forum – Learn more on SQLServerCentral

23. [Troubleshooting the Issues with DBCC ShrinkDatabase or ...](http://sqlserverandme.blogspot.com/2014/08/troubleshooting-issues-with-dbcc.html) - Summary : To shrink all data and log files for a specific database, use DBCC SHRINKDATABASE command....

24. [New in SQL Server 2022: the WAIT_AT_LOW_PRIORITY option (in Russian) - Habr](https://habr.com/ru/articles/775468/) - On the WAIT_AT_LOW_PRIORITY option of DBCC SHRINKDATABASE and how it reduces contention.

25. [What Virtual Filestats Do, and Do Not, Tell You About I/O ...](https://sqlperformance.com/2013/10/t-sql-queries/io-latency) - Erin Stellato (@erinstellato) of SQLskills.com shows us why I/O latency or high I/O-related waits ar...

