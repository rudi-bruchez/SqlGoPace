# Manifest format

A manifest is one logical task: an ordered list of operations executed sequentially
against one database. It is the only way to tell SqlGoPace what to do. There is no way
to hand it a `.sql` file, deliberately: parsing and rewriting unknown T-SQL is fragile,
and knowing each operation's exact shape is what lets the tool build the `WITH (...)`
clause correctly, preflight the object, and resume after a crash.

Options left empty are injected automatically from the detected version and edition. You
describe the intent; the tool writes the statement.

For the operation catalogue and every operation's fields, see
[`operations.md`](operations.md).

## A complete example

```yaml
# 01.to_run/010_rebuild_dispatch.yaml
description: "Recompress DISPATCH indexes and add a tracking column"
database: MYDB          # optional; defaults to the connection's database
on_failure: stop        # optional: stop (default) | continue
intent: compression     # optional manifest-level default for rebuild_index operations

operations:
  - operation: rebuild_index
    schema: dbo
    table: DISPATCH
    index: IX_DISPATCH        # or "ALL" to rebuild every index on the table
    data_compression: PAGE
    options:
      maxdop: 4               # explicit override for THIS operation

  - operation: add_column
    schema: dbo
    table: DISPATCH
    column: PROCESSED
    type: BIT
    nullable: false
    default: 0                # constant → metadata-only on Enterprise
```

## Top-level fields

| Field | Required | Purpose |
|---|---|---|
| `description` | no | Free text, echoed into the run report. |
| `database` | no | The database these operations target. Defaults to the connection's. A run opens one engine per database the queue targets. |
| `operations` | yes | The ordered list. At least one. |
| `on_failure` | no | `stop` (default, fail-fast) or `continue` (quarantine the failure and carry on). See [`running.md`](running.md). |
| `intent` | no | Default `intent` for every `rebuild_index` below. See below. |
| `window` | no | Restrict the manifest to a recurring time window. See below. |
| `ignore_blocked_sessions` | no | Sessions allowed to stay blocked by these operations. See [`blocking-and-kills.md`](blocking-and-kills.md). |
| `kill_blocking_sessions` | no | Sessions that may be killed when they block these operations. Inert unless armed in `config.yaml`. |
| `abort_blocking_resumable` | no | Clear a foreign paused resumable before a fresh rebuild. Off by default. See [`running.md`](running.md). |

## `intent` (rebuild_index only)

A `rebuild_index` does two unrelated things: it applies a `data_compression` target (a
state, idempotent, nothing to do if already there) and it rebuilds the index (an act that
defragments, rebuilds statistics and reclaims pages, never idempotent). `intent` tells the
engine which one motivated the operation, so a re-run knows whether skipping it is safe.

| Value | A re-run |
|---|---|
| `compression` | Skips the operation when every partition already carries the target compression. A cheap catalog read, reported as `skipped: already PAGE`. |
| `fragmentation` | Always runs. The defrag still needs doing whatever the compression state. |
| unset | Same as `fragmentation`. |

Unset defaults to always running because a wrongly skipped rebuild is silent, reported as
a success that did nothing, while a wrongly repeated one only costs time.

`intent` can be set per operation or once at the manifest level as a default each
`rebuild_index` inherits. Precedence: operation value, then manifest default, then unset.

This replaced the old `skip_if_satisfied` flag, which applied uniformly and could not tell
a defrag rebuild from a compression rebuild. A manifest still carrying it fails to load.
[`specs/OPERATION-INTENT.md`](specs/OPERATION-INTENT.md) has the design.

## `window`

Restrict a manifest to a recurring window, evaluated against the SQL Server's local clock
(`SYSDATETIME()`), not the client's.

```yaml
window:
  start: "01:00"      # HH:MM, 24h, server local time
  end:   "05:00"
  days:  [Sat, Sun]   # optional; Mon..Sun; default every day
```

- `end` earlier than `start` is an overnight window crossing midnight, for example
  `22:00` to `05:00`. `days` selects the day the window opens.
- Outside the window the manifest is deferred: left in `01.to_run`, not run. Schedule the
  process itself (cron, Task Scheduler) to launch during the window.
- If the window closes mid-run, the current operation finishes, then the run stops and the
  manifest stays in `02.processing` with its resume cursor, continuing in the next window.
- `start == end` is rejected. An offline `--dry-run` has no connection and so cannot
  evaluate a window; it annotates it instead.

## Per-operation `options`

Every option in this block is an override. Omit it and the engine resolves the value from
the compatibility matrix, then the config policy. Set it and your value wins.

```yaml
options:
  online: true               # T-SQL options
  resumable: true
  wait_at_low_priority: true
  sort_in_tempdb: true       # cannot be combined with resumable (Msg 11438)
  maxdop: 4                  # 0..32767; outside that range the manifest is rejected

  ignore_blocking: true      # reaction policy, NOT a T-SQL option
  max_block_minutes: 30      # reaction policy: yield after N minutes even if ignored
```

The last two are reaction-policy overrides rather than T-SQL. `ignore_blocking` holds the
lock through any blocking instead of yielding; `max_block_minutes` is the backstop that
yields anyway after N minutes. Both are covered in
[`blocking-and-kills.md`](blocking-and-kills.md).

Note that some combinations are refused by the server, not by us, and the resolver drops
one side rather than emitting a statement that fails: `RESUMABLE` is rejected in `tempdb`
(Msg 11439) and cannot be combined with `SORT_IN_TEMPDB` (Msg 11438). `--explain` names
every such decision.

## Sidecar files

A run writes advisory files next to the report, all machine-readable and none of them read
back by SqlGoPace. Copying something out of one into a manifest is always a deliberate act.

| File | Written when | Contains |
|---|---|---|
| `<manifest>.log` | always | The run report: statements, decisions, reactions, timings. |
| `<manifest>.state.json` | while in `02.processing` | The resume cursor and the run's identity, used by crash recovery. |
| `<manifest>.blocked.yaml` | the run reacted to blocking | The sessions it was blocking, as pasteable `ignore_blocked_sessions` entries. |
| `<manifest>.contended.yaml` | a shrink hit an object it could not pass | Confirmed blocker objects, consumable by `plan --confirmed`. |
| `<manifest>.amplifiers.yaml` | an amplifying victim was killed | One entry per kill, plus `sp_update_job` statements to disable the jobs by hand. |
| `<manifest>.heaps.yaml` | `plan` with `shrink.enabled` | Heaps a shrink cannot benefit from. |
| `<manifest>.recovery.yaml` | `on_failure: continue` and something failed | A re-runnable manifest holding only the failed operations. |
