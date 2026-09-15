# Object sizes — resumable-pause probe results

Result of [OBJECT-SIZES-resume-probe.sql](OBJECT-SIZES-resume-probe.sql), which answers open
question 1 of the object-sizes design: what does the size read return while a resumable index
rebuild is paused?

The design document the probe refers to (`OBJECT-SIZES.md`) is not in the tree. This file
records the measurement so the decision it settles is not lost. The edits it calls for are
listed at the end.

## Verdict

**The size read returns the source index alone.** A "before" size read at RESUME is the true
old size, so the §5.2 exception for paused resumable rebuilds is no longer needed.

## Setup

- SQL Server 2022 Developer edition, `mcr.microsoft.com/mssql/server:2022-latest` under Podman
  5.8.3, a throwaway container, 2026-09-15.
- `dbo.sqlgopace_resume_probe`: 2,000,000 rows of `(int, char(500))`, clustered index
  `CIX_resume_probe`, about 1 GB.
- `ALTER INDEX ... REBUILD WITH (ONLINE = ON, RESUMABLE = ON)`, paused from a second session
  4 seconds in.

## Measurements

`sys.index_resumable_operations` after the pause: `PAUSED`, `percent_complete` 95.67,
`page_count` 131,009. This is late in the rebuild and the hardest case for the question,
because the partial target was almost as large as the source.

| Read | Before the rebuild | While paused |
|---|---|---|
| TableStructureSizes (§1 query) `used_kb` | 1,068,648 | 1,068,648 |
| `sys.indexes` rows (hypothetical included) | 1 (index_id 1) | 1 (index_id 1) |
| `sys.dm_db_partition_stats` rows | 1: 133,581 pages, 2,000,000 rows | 1: 133,581 pages, 2,000,000 rows |

All three reads were identical. None of them showed an extra row or extra pages.

## Where the partial target lives

The target does exist, under internal index ids that `sys.indexes` and
`sys.dm_db_partition_stats` never show. It is visible only through `sys.partitions` joined to
`sys.allocation_units`:

| index_id | Allocation unit | used_pages | What it is |
|---|---|---|---|
| 1 | IN_ROW_DATA | 133,581 | the source clustered index |
| 896001 | IN_ROW_DATA | 127,856 | the partial new clustered index |
| 896254 | IN_ROW_DATA | 2,635 | most likely the online rebuild's mapping index |

**The partial target's space is real on disk even though the size read does not show it.**
While paused, the data file's used space was 2,146,240 KB, about twice the table. After
`ABORT` and `DROP TABLE` it fell to 1,076,992 KB. A paused resumable rebuild therefore holds
roughly one extra copy of the index in the data file for as long as it stays paused. Any code
that reasons about free space, such as a shrink preflight or a data-free-space check, must not
use the size read to estimate that space.

## Observed behavior worth knowing

- **The session running the rebuild does not get a "paused" message.** When another session
  issues `PAUSE`, it is disconnected with Msg 1219 ("Your session has been disconnected
  because of a high priority DDL operation"), followed by a severity 21 "session is in the
  kill state". The probe script says to expect "an error saying it was paused"; this is what
  that error actually is.
- Space from the dropped table was released through deferred drop: the used space was
  unchanged right after `DROP TABLE` and had fallen by the next read.

## Edits this calls for in OBJECT-SIZES.md

To apply once that document is in the tree:

1. Close open question 1 with the verdict above and a link to this file.
2. Remove the §5.2 exception for paused resumable rebuilds.
3. Add the on-disk note: the paused partial target occupies real data-file space that
   TableStructureSizes does not report.
