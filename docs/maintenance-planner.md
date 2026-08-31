# The maintenance planner

`sqlgopace plan` turns the tool around: instead of you writing manifests, it inspects the
database and generates the maintenance work itself, as manifests you review before anything
runs.

It covers fragmentation-driven `REORGANIZE` and `REBUILD`, data compression (`ROW` or
`PAGE`, chosen on measured gain and write-intensity), heap rebuilds where forwarded records
justify them, `UPDATE STATISTICS`, and `DBCC CHECKDB`. The rules and thresholds live in
`maintenance_profile.yaml`.

Nothing is executed by `plan`. It writes reviewable manifests into the queue; you read them,
then run them through the normal engine.

## Cheap first

One metadata sweep selects candidates, and the expensive reads,
`sp_estimate_data_compression_savings` and a sampled `sys.dm_db_index_physical_stats`, run
only over the survivors. That ordering is what makes planning a whole database affordable.

## Scope

Index, compression, heap and statistics maintenance analyse and act on the single database
the connection string points at, because the analysis DMVs and the generated DDL are
database-scoped. Only `DBCC CHECKDB` can span several databases, through `checkdb.databases`
in the profile.

For a server-wide pass, `--all-databases` (or `--databases a,b,c`) materialises a
per-database block of manifests, scoped by a `scope:` selector in the profile. The run then
processes the queue one connection per database, sequentially. Ineligible databases, an AG
secondary, read-only, offline, or simply not accessible, are skipped with the reason logged.

Crash recovery is database-aware: each in-flight operation records the database it ran in,
and a later run reconciles it against that database. An orphan whose database is unreachable,
because it is now an AG secondary for instance, is left for a future run.

## Using it

```bash
# Analyse and print the manifests it would write. No files, no locks.
sqlgopace plan --config config.yaml --dry-run

# The same, with the reasoning behind every decision
sqlgopace plan --config config.yaml --dry-run --explain

# Write reviewable manifests into the queue
sqlgopace plan --config config.yaml

# Narrow the categories, and target check_db at another database
sqlgopace plan --config config.yaml --categories index,compression,heaps --database MYDB

# Every eligible user database
sqlgopace plan --config config.yaml --all-databases

# Or a chosen set
sqlgopace plan --config config.yaml --databases SALES,INVENTORY

# Then review 01.to_run/*.yaml and run them as usual
sqlgopace --config config.yaml
```

| Flag | Effect |
|---|---|
| `--config` | Config file, for the connection and the default output directory. Required. |
| `--profile` | Maintenance profile path. Default `maintenance_profile.yaml`. |
| `--categories` | Comma-separated subset of `index,compression,heaps,statistics,checkdb`. Default all. |
| `--database` | Single-database mode: the database to plan. Default the connected one. |
| `--all-databases` | Plan every eligible user database. |
| `--databases` | Comma-separated list of databases to plan. |
| `--out` | Directory to write manifests into. Default the config's `to_run`. |
| `--dry-run` | Print the manifests instead of writing them. |
| `--explain` | Show the reasoning behind each decision. |
| `--confirmed` | Path to a `.contended.yaml` from a prior shrink run. See below. |

## Unattended

`--auto` on the main command does the whole thing in one step: analyse, write the manifests
into the queue, then process the queue. No review.

```bash
sqlgopace --config config.yaml --auto
sqlgopace --config config.yaml --auto --all-databases
```

It accepts the same scoping flags as `plan`. Use it when the profile is settled and you
trust it; use `plan` plus a read when it is not.

## Feeding a shrink back into planning

`--confirmed <path>` takes a `.contended.yaml` written by a previous shrink: the objects
that shrink blocked on, or the tail object it could not get past. Those objects are
prioritised in the pre-shrink reorganize pass, tail-position blockers first, and matching
heap advisories are marked `CONFIRMED`.

It requires `shrink.enabled` in the profile. This is the loop that makes a stubborn shrink
converge: shrink, read what stopped it, reorganize exactly that, shrink again.

## The `shrink:` profile block

Optional. When enabled, `plan` emits an extra manifest for the connected database that
reorganizes the low-density rowstore indexes, the tables large deletes left half-empty, and
then shrinks the data file. It also writes a `.heaps.yaml` advisory listing the heaps a
shrink cannot benefit from, since reorganize cannot compact a heap's in-row data; rebuild
those in a window.

```yaml
shrink:
  enabled: true          # absent or false means no shrink manifest
  type: data             # data | log; log skips the reorganize pass and the advisory
  files: all             # all, or a logical file name
  targetfreespace: 10%   # percent or absolute MB
  pre_reorganize: true   # false emits the shrink operation alone
  reorganize_below_density_percent: 65   # reorganize rowstore indexes below this SAMPLED page density
  max_block_minutes: 10  # optional; carried into the shrink operation's options
  identify_tail_object: true             # optional; 2019+
```

Four things worth knowing before you enable it:

- The index size floor reuses `index.page_count_floor` from the profile.
- Session policy is never generated. Add `ignore_blocked_sessions` or
  `kill_blocking_sessions` by editing the generated manifest.
- The reorganize selection runs a SAMPLED `sys.dm_db_index_physical_stats` scan at plan
  time, which is heavier than the LIMITED scan the ordinary maintenance pass uses.
- With `pre_reorganize: false` the heap advisory is skipped too, because it needs the
  page-density scan that the pre-reorganize pass performs.

## Design

[`specs/MAINTENANCE.md`](specs/MAINTENANCE.md) is the full design, including the
multi-database mode in §17.
