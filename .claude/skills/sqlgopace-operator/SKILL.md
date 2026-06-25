---
name: sqlgopace-operator
description: >-
  Help a user operate SqlGoPace — the YAML-manifest-driven DDL runner for SQL Server.
  Use when the user wants to perform a SQL Server DDL/maintenance operation described in
  natural language (compress / recompress indexes, rebuild or reorganize an index, add /
  alter / drop a column or constraint, create or drop an index, update statistics, run
  DBCC CHECKDB, or shrink a data/log file) and needs a manifest written, reviewed, or
  explained — or asks how to use SqlGoPace, what an operation will do, or what to check
  before running. Triggers include "compress these indexes", "make a sqlgopace manifest",
  "how do I … with sqlgopace", "rebuild/shrink/checkdb on <db>".
---

# SqlGoPace Operator

Help the user turn a natural-language DDL/maintenance request into a **correct SqlGoPace
manifest**, explain usage, and warn about what to verify before running.

## How to use this skill

1. **Read `docs/llm-operator-guide.md`** (in the repo root's `docs/`). It is the source of
   truth: the manifest schema, the 13 valid operations with their exact fields, the
   option-injection matrix, the queue lifecycle, the safe dry-run-first workflow, and the
   pre-run warnings. Follow it rather than relying on memory of T-SQL.
2. **Map the request** to one of the 13 operations. If it does not map (raw SQL, data
   changes, table creation, MERGE, …), tell the user it is out of scope — SqlGoPace only
   runs the operation types it knows.
3. **Produce the manifest** (top-level `description` / `database` / `operations`, one
   operation per object). Leave `options:` empty unless the user explicitly overrides —
   the engine injects ONLINE/RESUMABLE/WAIT_AT_LOW_PRIORITY/MAXDOP from the version×edition
   matrix. **Never hand-write `WITH (...)`.**
4. **Give guidance**: a filename for `01.to_run/`, the dry-run command
   (`sqlgopace -config config.yaml -dry-run -explain <file>`), and the warnings relevant to
   this request.

## Hard rules (from the guide — do not skip)

- **Compression = a `rebuild_index` with `data_compression`.** There is no "compress"
  operation; `reorganize_index` cannot change compression.
- **No raw SQL, no invented operations**, no hand-written `WITH (...)` options.
- **Always recommend `--dry-run --explain` first.**
- **Surface the key warnings**: on **Standard** edition index rebuilds are OFFLINE
  (table locked, no resumable); the login needs `ALTER` (e.g. `db_ddladmin`) + `VIEW SERVER
  STATE` — missing rights make preflight report "table does not exist" (it's permissions,
  not a missing table); confirm the target database; interruptions currently restart the
  operation (no true resume yet).
- **Ask before guessing** the database, the target compression tier, or whether a rebuild
  is meant to recompress vs. defragment.

## When the user asks for automatic maintenance

If the request is "decide what needs rebuild/reorg/compress/stats/checkdb based on the
database's real state" (not a hand-listed set of objects), point them to the **`plan`
subcommand** + `maintenance_profile.yaml`, which generates reviewable manifests into
`01.to_run/`. See `specs/MAINTENANCE.md`.

## Do not execute

This skill writes and explains manifests; it does **not** run DDL against a database on its
own. Running is the user's explicit, separately-authorized step (and mutates the server).
