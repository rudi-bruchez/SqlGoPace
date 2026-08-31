---
name: sqlgopace-operator
description: >-
  Write and review SqlGoPace manifests — the YAML that drives its DDL runner for SQL Server.
  Use when the user describes a SQL Server DDL or maintenance operation in natural language
  (compress or recompress indexes, rebuild or reorganize an index, add / alter / drop a
  column or constraint, create or drop an index, update statistics, run DBCC CHECKDB, shrink
  a data or log file or tempdb, or update/delete a large number of rows in batches) and needs
  a manifest written, reviewed, or explained — or asks what an operation will do and what to
  check before running it. Triggers include "compress these indexes", "make a sqlgopace
  manifest", "how do I … with sqlgopace", "rebuild/shrink/checkdb on <db>", "delete these
  rows without filling the log". For running a manifest and watching it, use
  sqlgopace-run instead.
---

# Writing SqlGoPace manifests

Turn a natural-language DDL or maintenance request into a correct manifest, explain what it
will do, and name what to verify before it runs.

## Read these first

- [`docs/operations.md`](../../../docs/operations.md) — the 16 operations with their exact
  fields. Use it rather than recalling T-SQL.
- [`docs/manifests.md`](../../../docs/manifests.md) — top-level fields, `intent`, `window`,
  per-operation `options`, and the sidecar files a run writes.
- [`docs/llm-operator-guide.md`](../../../docs/llm-operator-guide.md) — the same ground
  written for an assistant, with worked examples and a checklist of warnings.

## The method

1. **Map the request to an operation.** If it does not map — raw SQL, creating a table,
   a `MERGE`, anything not in the sixteen — say so plainly. SqlGoPace runs only the
   operation types it knows, and that is the property everything else depends on.
2. **Ask what you cannot infer.** Which database. Whether a `rebuild_index` is meant to
   recompress or to defragment, because that decides `intent` and therefore whether a re-run
   skips it. The target compression tier. The maintenance window. Guessing any of these
   produces a manifest that looks right and does the wrong thing.
3. **Write the manifest.** Top-level `description`, `database`, `operations`; one operation
   per object. Leave `options:` empty unless the user is deliberately overriding: the engine
   injects `ONLINE`, `RESUMABLE`, `WAIT_AT_LOW_PRIORITY` and `MAXDOP` from the version and
   edition it detects. Never hand-write a `WITH (...)` clause.
4. **Hand over the next step**, which is always the dry run, never the run:
   `sqlgopace --config config.yaml --dry-run --explain 01.to_run/<file>.yaml`.

## Rules that are easy to get wrong

- **Compression is a `rebuild_index` with `data_compression`.** There is no compress
  operation, and `reorganize_index` cannot change compression.
- **`index: ALL` is not the same as naming an index.** It expands to one operation per real
  index, which is what lets each one carry `RESUMABLE`; `ALTER INDEX ALL` cannot.
- **Session policy has a direction, and the wrong list fails silently.**
  `ignore_blocked_sessions` is for sessions *we* block, the ones that appear in
  `<manifest>.blocked.yaml`. `kill_blocking_sessions` is for sessions that block *us*. Same
  match fields, opposite meanings. Never write either from a login the user names offhand:
  read the run's `.blocked.yaml` first, ask whether the session holds open transactions, and
  ask for the kill delay rather than choosing it. `kill_blocking_sessions` also does nothing
  unless `kill_blockers.enabled: true` in the config, so say that when you write one.
- **Batched DML needs `SELECT` as well as `UPDATE` or `DELETE`.** Every batch is an
  `UPDATE`/`DELETE TOP (n)`, so `db_datawriter` alone fails mid-run. It also refuses to run
  with no predicate at all unless `confirm_full_table: true` says you meant it.
- **`maxdop` is bounded to 0..32767.** Outside that the manifest is rejected, because SQL
  Server would reject the statement with Msg 304.

## Warnings to surface, when they apply

- **Standard edition rebuilds offline.** `ONLINE` is Enterprise and Azure only, so on
  Standard the table is locked for the duration and there is no resumable fallback. Say this
  before a large rebuild, not after.
- **Permissions look like a missing table.** Without rights on an object, `OBJECT_ID(...)`
  returns NULL and preflight reports "table does not exist" for a table that exists. If the
  user sees that, suspect permissions first. [`docs/permissions.md`](../../../docs/permissions.md)
  has the grant per operation.
- **A shrink fragments what it moves.** Only suggest it to reclaim space that is genuinely
  needed, and say a rebuild or reorganize afterwards may be required.
- **`shrink_tempdb` needs `sysadmin`**, and it is the only operation that does.
- **Heaps and special indexes.** Heap rebuilds never get `RESUMABLE` or
  `WAIT_AT_LOW_PRIORITY`; columnstore, XML and spatial indexes reject the whole `ONLINE`
  family.

## When the request is "decide for me"

If the user wants the tool to work out what needs rebuilding, reorganizing, compressing or
checking from the database's real state, rather than acting on a hand-listed set of objects,
that is the `plan` subcommand and `maintenance_profile.yaml`, not a hand-written manifest.
See [`docs/maintenance-planner.md`](../../../docs/maintenance-planner.md).

## Do not execute

This skill writes and explains manifests. It does not run DDL. Running mutates a production
server and is the user's explicit, separately authorised step; when they ask for it, use the
`sqlgopace-run` skill.
