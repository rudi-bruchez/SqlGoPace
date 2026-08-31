> From: https://gemini.google.com/app/67cf4f9a5fd85308
>
> Raw research transcript, kept as historical source material for the shrink driver's
> design. It is an input to a decision already made, not a specification, and it is not
> maintained. Translated from the original French.

# you asked

Is there a way to estimate how long a shrink of a SQL Server database file will take? And
how many pages it will have to move?

---

# gemini response

That is an excellent question, and a well-known challenge for database administrators. To be
completely frank: **it is extremely difficult to estimate precisely either the time or the
exact number of pages to move before starting the operation.**

There are, however, methods for following progress once the operation is under way, and ways
to estimate the volume of data involved. Here is how you can approach the problem.

---

### 1. Estimating the time needed (during execution)

Before starting the shrink, the duration depends on too many variables: the speed of your
disk subsystem (I/O), internal fragmentation, the presence of LOB data (Large Objects), and
the locks held by other active queries.

Once you have started `DBCC SHRINKFILE` or `DBCC SHRINKDATABASE`, SQL Server exposes its
progress through the dynamic management views (DMVs). You can use the following query to see
the percentage complete and the estimated time remaining:

```sql
SELECT
    session_id,
    command,
    status,
    percent_complete,
    estimated_completion_time / 1000 / 60 AS estimated_minutes_left,
    total_elapsed_time / 1000 / 60 AS elapsed_minutes,
    wait_type,
    wait_time,
    blocking_session_id
FROM sys.dm_exec_requests
WHERE command IN ('DbccFilesCompact', 'DbccSpaceReclaim') -- internal commands for the shrink
   OR command LIKE 'DBCC%';
```

> **Caution:** `estimated_completion_time` is computed in real time by SQL Server. It can
> fluctuate enormously. If the engine reaches a heavily fragmented section of the file, or
> LOB objects, the estimated time can suddenly jump.

---

### 2. Estimating the number of pages to move (before execution)

To understand why this is hard to predict, you have to understand how the shrink works: SQL
Server takes the data pages located **at the end** of the physical file and moves them into
the **empty (unallocated) space at the beginning of the file**.

* **The ideal case:** if your file is 100 GB, 50 GB are used, and all that data is perfectly
  packed at the beginning of the file, the shrink will move **zero pages**. It will simply
  cut off the end of the file.
* **The disaster case:** if the 50 GB of data are scattered all the way to the very end of
  the 100 GB file, the engine will have to read and rewrite a massive number of pages to
  pack the data toward the beginning.

You cannot easily count the pages that will be moved without physically analysing the
location of every extent, but you can **estimate the free space and the used space** in
pages (1 page = 8 KB) to know the theoretical limit.

Use this query to analyse your files:

```sql
SELECT
    name AS file_name,
    physical_name AS physical_path,
    size AS total_size_pages,
    CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT) AS used_pages,
    size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT) AS free_pages,
    (size / 128.0) AS total_size_mb,
    (CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT) / 128.0) AS used_space_mb,
    ((size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)) / 128.0) AS free_space_mb
FROM sys.database_files;
```

If you decide to reduce the file size as close as possible to `used_space_mb`, the distance
between the position of the last allocated page and the target size determines the volume of
pages to be moved.

---

### ⚠️ A word of caution (a friendly reminder)

Since we are talking about shrink, it is my duty to remind you of a few realities of this
operation:

* **Massive fragmentation:** unlike an index rebuild, the shrink moves pages out of order to
  fill holes. This produces near-total fragmentation (close to 100%) of your indexes.
* **Transaction log activity:** every page move is logged. A heavy shrink will inflate your
  log file (`.ldf`), which can cancel out the space gained if the log sits on the same disk.
* **Future growth:** if the database is going to grow again over the coming months, shrinking
  the file is counterproductive, because SQL Server will have to allocate the space on disk
  all over again, at a performance cost.

If you absolutely must do it, for instance after purging millions of rows of history you will
not replace, it is often recommended to proceed in **small increments** (say 5 GB at a time)
rather than launching one enormous operation, so that you can stop it cleanly if you need to.

---

# you asked

Explain in detail how https://dbatools.io/Invoke-DbaDbShrink/ works

---

# gemini response

The `Invoke-DbaDbShrink` command is part of the well-known **dbatools** PowerShell module. It
is a wrapper, an intelligent layer, around the native SQL Server commands `DBCC
SHRINKDATABASE` and `DBCC SHRINKFILE`.

Rather than simply issuing a raw shrink, which is often a risky and blind operation in T-SQL,
`Invoke-DbaDbShrink` adds control, measurement and safety mechanisms that database
administrators value highly.

Here is how the command works in detail, and what makes it better than a plain SQL query.

### 1. What happens behind the scenes (the command's algorithm)

When you run `Invoke-DbaDbShrink`, the PowerShell script performs several steps in sequence:

1. **Updating the space statistics (`DBCC UPDATEUSAGE`):** by default the command begins by
   forcing an update of the database's space counters. This guarantees that the free-space and
   used-space calculations are exact before any decision is taken. It can be disabled with
   `-ExcludeUpdateUsage`.
2. **Measuring the initial fragmentation:** before touching the files, the tool queries the
   DMVs to measure the average and maximum fragmentation of your indexes. This serves as the
   point of comparison. It can be disabled with `-ExcludeIndexStats` to save time.
3. **Running the shrink:** the command generates and executes the necessary T-SQL dynamically,
   according to the parameters you chose (see below).
4. **Measuring the final fragmentation:** once the operation is finished, the tool recomputes
   your index fragmentation to show you the exact impact of the shrink, which generally
   fragments the data massively.
5. **Producing a report:** the command returns a complete PowerShell object listing what was
   done, the space reclaimed and the time elapsed.

### 2. The key features and parameters

This is where `Invoke-DbaDbShrink` really shines compared to native SQL:

* **Chunking (`-StepSize`):** the most important feature. If you ask to reduce a 50 GB file in
  one go, SQL Server will generate a massive transaction, saturate the CPU, potentially block
  other queries, and blow up the size of the transaction log.
  With `-StepSize 25MB` or `-StepSize 1GB`, dbatools performs the shrink in small pieces
  (successive loops). This reduces the pressure on the system, allows progress to be followed
  through a PowerShell progress bar, and avoids saturating the log.

* **Targeting free space (`-PercentFreeSpace`):** rather than emptying the file completely and
  leaving zero bytes of free space, you can tell it `-PercentFreeSpace 15`. The tool computes
  dynamically what size to reduce the file to so that it keeps 15% free, letting the database
  breathe and grow a little without triggering an immediate auto-growth.

* **Targeting files (`-FileType`):** you can specifically target data files (`Data`), log
  files (`Log`), or both (`All`). This is very useful for shrinking only the `.ldf` after a
  large incident, without touching the data.

* **Shrink methods (`-ShrinkMethod`):** it supports the native options:
  * `TruncateOnly`: simply cuts the empty space off the end of the file and returns it to the
    OS. It moves no data pages, is instantaneous, and creates no fragmentation.
  * `EmptyFile`: empties a data file so it can be removed, moving its content into the other
    files of the same filegroup.
  * `NoTruncate`: packs the data toward the beginning of the file but does not return the
    space to the OS.

* **Blocking management (SQL Server 2022 and later):** on a recent instance you can use
  `-WaitAtLowPriority`. Instead of competing with active users for object locks, the shrink
  waits in the background at low priority. You can even tell it to cancel itself if the wait
  is too long (`-AbortAfterWait Self`) or to kill the blocking sessions (`Blockers`).

### 3. What the tool returns

At the end of the operation, instead of a bare "command completed successfully" message,
`Invoke-DbaDbShrink` returns a very rich PowerShell object containing, among other things:

* `InitialSize` and `FinalSize`, the size before and after.
* `InitialAvailable` and `FinalAvailable`, the free space before and after.
* The start time, the end time and the total duration (`Elapsed`).
* `InitialAverageFragmentation` and `FinalAverageFragmentation`, showing visually how far the
  shrink destroyed the organisation of your indexes.

### In summary

`Invoke-DbaDbShrink` turns a heavy, blind and dangerous operation into a controlled, chunked,
measurable and automatable process. It is today the method recommended by the SQL
administration community if you are **absolutely obliged** to shrink your database files.

---

# you asked

Is there a good way to estimate the most effective step size?

---

# gemini response

That is the genuinely hard question. There is no universal formula for `StepSize`, because
what counts as "effective" is not merely what goes fastest, but what **keeps the instance
stable** while the operation runs.

When you design massive archiving routines or heavy database maintenance, choosing the step
size really comes down to balancing I/O pressure, locking, and above all the management of
the transaction log (write-ahead logging).

Here is the architectural method for estimating the most effective chunk size for your
environment.

### 1. The main bottleneck: the transaction log

This is the golden rule of the shrink: **moving pages is fully logged.**
Moving 1 GB of data from the end of the file to the beginning generates at least 1 GB of
writes to the `.ldf`, and usually somewhat more because of the overhead.

If you pick a `StepSize` that is too large, here is what happens:

1. The transaction becomes massive.
2. The transaction log fills faster than the checkpoint process (in SIMPLE recovery) or your
   log backups (in FULL) can empty it.
3. The `.ldf` undergoes an auto-growth, creating new VLFs (virtual log files) and potentially
   physical fragmentation of the log at the storage level.
4. The operation collapses in performance under `WRITELOG` waits.

**The estimation approach:** the ideal `StepSize` should generate a transaction volume smaller
than the free space currently available in your log, or at least smaller than your log's
auto-growth increment, to leave the truncation mechanism time to do its work between two
iterations.

### 2. Overhead versus I/O throughput

There is a tipping point to find:

* **Too small (say 5 MB or 10 MB):** the overhead of running the command, analysing the pages,
  and the round trips of the loop, whether through PowerShell or a T-SQL `WHILE`, consumes
  more CPU than moving the data does.
* **Too large (say 5 GB or 10 GB):** the disk subsystem is saturated by read and write
  queues, generating intense `PAGEIOLATCH_EX` waits that will affect every other query on the
  same LUN or disk group.

### 3. How to calibrate the step size (methodology)

Since storage systems vary, from an old SAN to direct-attached NVMe, the best method remains
empirical, but guided:

**Step A: establish a baseline (the safe zone).**
The community generally agrees that a safe starting point lies between **50 MB and 500 MB**.

* **250 MB** is often considered the sweet spot to start from on modern hardware. It moves
  about 32,000 pages per iteration, which is processed in a few seconds without saturating the
  buffer.

**Step B: the load test (monitor the wait statistics).**
Start the process with a `StepSize` of 250 MB and watch your DMVs:

```sql
-- Watch the log growing during the iteration
SELECT database_id, log_space_in_bytes_since_last_backup
FROM sys.dm_db_log_stats(DB_ID());
```

If you notice your `WRITELOG` or `PAGEIOLATCH_EX` waits climbing beyond 20 to 30 ms of average
latency, your I/O is not keeping up and you should reduce the step size, to 100 MB for
instance.

**Step C: adjust for the recovery model.**

* **SIMPLE recovery:** you can afford a slightly larger `StepSize`, say 500 MB, because the
  implicit checkpoint at the end of each chunk truncates the log almost immediately.
* **FULL recovery:** the log only empties when the log is backed up. If your backup runs every
  fifteen minutes, you have to work out how many gigabytes of shrink your log disk can absorb
  in fifteen minutes without saturating.

---
