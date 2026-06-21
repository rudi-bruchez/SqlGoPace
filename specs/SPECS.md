# SqlGoPace — DDL orchestrator for SQL Server

A Go application: a *task runner* that executes demanding DDL operations on SQL Server
(`ALTER INDEX`, `CREATE INDEX`, `ALTER COLUMN`, column additions, constraints…) while
continuously monitoring their impact, and reacting intelligently to blocking and to
transaction-log pressure.

Architecture: a Go orchestrator with an **execution thread** (dedicated connection) and a
**monitoring thread** (separate connection).

## Context — why a dedicated tool

These operations are risky in production:

- they **block** other sessions or **get blocked**: `LCK_M_SCH_S`, `LCK_M_SCH_M`, `LCK_M_IX` waits;
- they can **fill the transaction log**;
- a `KILL` triggers a `ROLLBACK` that is sometimes long and expensive;
- the right option set (`ONLINE`, `RESUMABLE`, `WAIT_AT_LOW_PRIORITY`, `MAXDOP`…) depends on the
  **version AND the edition** of the target server.

The tool automates the decision and the monitoring, and always favours the least destructive
mechanisms (pause/resume rather than kill/rollback).

## Execution modes

- **Silent + log** (default): non-interactive execution, everything is traced into the `.log` files.
- **TUI**: flag `--tui` — interactive incident console (see §14).
- **Dry-run**: flag `--dry-run` — prints the final DDL command (injected options included)
  **without executing anything** and without taking a single lock.
- **Explain**: flag `--explain` — for each operation, shows *why* each option was added or
  removed (detected version/edition + matrix entry + config override).

---

## 1. Declarative interface (DDL in YAML — no raw SQL)

**Architecture decision: the tool does NOT accept `.sql` files.** Accepting arbitrary T-SQL would
require parsing and rewriting `WITH (...)` clauses in a fragile way, and executing uncontrolled code.
That is too dangerous for this use case.

Instead, each task is described by a **YAML manifest** of operations. The tool **generates** the
T-SQL itself from the structured description: it knows the operation type with certainty (zero
parsing ambiguity), builds the option clause with no risk of duplication, runs a precise preflight on
the targeted object, and handles idempotency and resume.

### 1.1 Manifest schema

One file = one logical task = an ordered list of operations executed **sequentially**
(the structured equivalent of the old "`GO` batches").

```yaml
# 01.to_run/010_rebuild_dispatch.yaml
description: "Recompress and add a column on DISPATCH"
database: MYDB          # optional: otherwise the database from the connection string
on_failure: stop        # optional: stop (default) | continue (see §11.3)
operations:
  - operation: rebuild_index
    schema: dbo
    table: DISPATCH
    index: IX_DISPATCH        # or "ALL" to rebuild every index
    data_compression: PAGE
    # online / resumable / wait_at_low_priority / maxdop / sort_in_tempdb:
    # left empty → injected automatically based on version/edition + matrix + config

  - operation: add_column
    schema: dbo
    table: DISPATCH
    column: PROCESSED
    type: BIT
    nullable: false
    default: 0               # constant → metadata-only on Enterprise (see §1.4)
    options:
      maxdop: 4              # explicit override of an option for THIS operation
```

### 1.2 Supported operation types

Each `operation` maps directly to a key in the compatibility matrix (§8). Adding a new operation
type is done in the code **and** in `ddl_compatibility.yaml`.

| `operation`        | Generated T-SQL                       |
|--------------------|---------------------------------------|
| `rebuild_index`    | `ALTER INDEX … REBUILD WITH (…)`      |
| `create_index`     | `CREATE [UNIQUE] INDEX … WITH (…)`    |
| `drop_index`       | `DROP INDEX …`                        |
| `add_column`       | `ALTER TABLE … ADD …`                 |
| `alter_column`     | `ALTER TABLE … ALTER COLUMN … WITH (…)` |
| `drop_column`      | `ALTER TABLE … DROP COLUMN …`         |
| `add_constraint`   | `ALTER TABLE … ADD CONSTRAINT … WITH (…)` |
| `drop_constraint`  | `ALTER TABLE … DROP CONSTRAINT …`     |

Any construct that is not modelled is **out of scope** (no raw-SQL escape hatch).

### 1.3 Option injection and override

For each injectable option (`online`, `resumable`, `wait_at_low_priority`, `maxdop`,
`sort_in_tempdb`, `data_compression`), the effective value is resolved in this order:

1. **Per-operation override** (`operations[].options.<opt>`) — highest priority.
2. **Global override** (`config.yaml > options_override.<opt>.force`).
3. **Auto** (default): injected if and only if the matrix (§8) allows it for the target
   `version × edition × operation`.

Automatically applied dependencies:

- `resumable: true` ⇒ forces `online: true` (RESUMABLE requires ONLINE for an index).
- `wait_at_low_priority` is injected only if `online: true`.
- an option not supported by the target is **silently omitted** (and traced under `--explain`).

`options.ignore_blocking: true` is a **reaction-policy override, not a T-SQL `WITH` option**:
it suppresses the blocking reaction (§9) for that single operation, so the operation holds its
lock to completion and leaves the other sessions blocked. Transaction-log pressure is still
honored. It is per operation (the rest of the batch keeps yielding), edition-independent (it is
about reactions, not injectable T-SQL), and traced under `--explain`. Typical use: force the one
important index through in a `on_failure: continue` batch.

**Top-level `ignore_blocked_sessions:`** is the *targeted* counterpart of `ignore_blocking`: a
list of session matchers, applied to **every** operation in the manifest, naming sessions that
are allowed to remain blocked. A matcher has `session_id` (exact int) and `app_name` /
`host_name` / `login_name` / `statement` — **all string fields are regular expressions**,
evaluated app-side (SQL Server has no regex before 2025). A session is ignorable when it matches
**any** entry; an entry matches when **every** field it sets matches (AND-within, OR-across). The
blocking detection (§8.2) excludes matching sessions before they ever count as pressure, so the
operation still yields the moment it blocks a *non-matching* session. Only the **blocking**
branch is affected — transaction-log pressure is always honored. Validated at load (each entry
sets ≥1 field, each regexp compiles); it is the single durable source of exclusions (re-read live
and carried into the recovery manifest — see §9). When the engine reacts to blocking it writes an
advisory `<manifest>.blocked.yaml` next to the run report (ready-to-paste matcher entries +
`observed:` diagnostics); the engine never reads it back.

### 1.4 Metadata-only operations

Some operations are metadata-only changes (e.g. `add_column NOT NULL` with a **constant default** on
Enterprise edition, widening `varchar(n)→varchar(m)` with `m>n`…). The tool **classifies** these
cases to flag them in the preflight and the `--explain` output ("expected instantaneous,
metadata-only"), **but never disables monitoring**: reliable detection depends on the table's actual
state (compression, sparse columns, LOB types, edition) and is not guaranteed from the manifest
alone. Monitoring is cheap; a false "instantaneous" is not.

### 1.5 Idempotency

The tool automatically wraps the generated command with an existence guard where relevant
(`IF NOT EXISTS (…)` for `add_column`/`create_index`/`add_constraint`, `IF EXISTS` for `drop`s).
A resume after a partial failure therefore does not re-apply an operation that already completed.

### 1.6 `ALL` index rebuilds

`rebuild_index` with `index: ALL` is **expanded at runtime** into one rebuild per concrete index of
the table, queried from `sys.indexes`, **clustered index first** then by `index_id`. This is
deliberate: SQL Server does not allow `RESUMABLE` with `ALTER INDEX ALL` (a resumable rebuild
requires a single named index), so expanding lets each index rebuild be RESUMABLE — the tool's
preferred reaction. Options are resolved **per index kind**: rowstore indexes get the full ONLINE /
RESUMABLE / WAIT_AT_LOW_PRIORITY treatment, while columnstore / XML / spatial indexes (which reject
those options) rebuild offline. Disabled indexes and the heap are skipped; the original
`data_compression` and option overrides carry to each rowstore index. Expansion only happens when
connected (it needs the live index list); an offline dry-run renders the verbatim `ALTER INDEX ALL`
with a note.

---

## 2. Directory structure

```
├── 01.to_run/        # *.yaml manifests waiting to run
├── 02.processing/    # moved here during execution (avoids conflicts + marks the orphan)
├── 03.done/          # completed successfully, + <name>.log
└── 04.failed/        # error / aborted, + <name>.log
```

Processing is **strictly sequential**, one manifest at a time, in the **sort order of the file name**
(convention `010_`, `020_`…). Never two heavy DDL statements in parallel on the same database: they
would block each other and multiply the log.

Only `*.yaml` / `*.yml` files are picked up. Files whose name **starts with a dot** are ignored, so a
manifest can be temporarily disabled by prefixing it with `.` and editor/OS dotfiles (`.gitkeep`,
`.DS_Store`) are never treated as manifests.

During execution, the manifest lives in `02.processing/` alongside a **state sidecar**
`<name>.state.json` (see §13) used for crash recovery.

---

## 3. Connection architecture (the pool trap)

Go's `database/sql` driver uses a dynamic connection pool by default — a major risk here.

- **Execution thread**: opens an **exclusive, dedicated connection** via `db.Conn(ctx)`. This is the
  only guarantee that the `SELECT @@SPID` read at startup matches exactly the session that runs the
  DDL.
- **Monitoring thread**: uses a **separate connection** (or even a separate pool) so it is never
  blocked by the DDL it monitors.

On the execution connection, at session startup:

```sql
SET XACT_ABORT ON;
SET DEADLOCK_PRIORITY LOW;   -- the DDL becomes the designated victim, not the user query
```

Do **not** set a `LOCK_TIMEOUT`: lock waiting is handled cleanly by `WAIT_AT_LOW_PRIORITY` (§11).

Recommended driver: **`github.com/microsoft/go-mssqldb`**.

---

## 4. Preflight

Before taking a single lock, the tool runs a battery of checks for each manifest.
**Any preflight failure sends the manifest straight to `04.failed/`** without having touched any data.

The preflight checks the **overall** health (we want to start from a healthy situation):

1. **Version & edition** of the target server (§7).
2. **Target validity**: the database, schema, table, index/column/constraint exist (or do not exist,
   for `create`s).
3. **Recovery model**: `SELECT recovery_model_desc FROM sys.databases`.
4. **Log**: current size, % used, the log file(s)' `max_size`, `log_reuse_wait_desc` — must be
   healthy at the start (no `LOG_BACKUP`/`ACTIVE_TRANSACTION` already blocking).
5. **Pre-existing blocking**: no abnormal blocking chain in progress.
6. **Data space**: an ONLINE rebuild/create builds a **copy** of the object → ≈ the object's size of
   free space is required in the target filegroup.
7. **tempdb**: if `SORT_IN_TEMPDB` will be injected, check tempdb space.
8. **Availability Group**: replica state (`sys.dm_hadr_database_replica_states`) — a large DDL can
   saturate the *send queue* and block log truncation on the primary if the secondary lags.
   **We warn (live TUI + log) but continue** (configurable).
9. **ADR**: `is_accelerated_database_recovery_on` (influences the strategy in §11).

Object-size estimation uses `sys.allocation_units` (cheap). Avoid
`sys.dm_db_index_physical_stats` in `DETAILED` mode (expensive and IO-generating); use
`LIMITED`/`SAMPLED` if needed.

---

## 5. Version / edition detection

```sql
SELECT
    SERVERPROPERTY('EngineEdition')        AS EngineEdition,
    SERVERPROPERTY('ProductMajorVersion')  AS ProductMajorVersion;
```

### EngineEdition → edition tier

| EngineEdition | Product                                            | Matrix tier  |
|---------------|----------------------------------------------------|--------------|
| 2             | Standard / Web / BI / Standard Developer           | `standard`   |
| 3             | Enterprise / Developer / Evaluation                | `enterprise` |
| 4             | Express                                            | `express`    |
| 5             | Azure SQL Database                                 | `azure` *    |
| 8             | Azure SQL Managed Instance                         | `azure` *    |
| 6 / 9 / 11 / 12 | Synapse / SQL Edge / Fabric                      | unsupported  |

\* The Azure editions (5, 8) are **evergreen** (≈ Enterprise, `ONLINE`/`RESUMABLE` always
available): they are not indexed by year. The tool assigns them a **very high pseudo major version**
(`9999`) so that all `min_major` checks are satisfied, and the `azure` tier.

### ProductMajorVersion → year

| Major | Year  | Major | Year  |
|-------|-------|-------|-------|
| 13    | 2016  | 16    | 2022  |
| 14    | 2017  | 17    | 2025  |
| 15    | 2019  |       |       |

This mapping is embedded in `ddl_compatibility.yaml` (key `major_to_year`) for display, but
**capability resolution is done directly on the major number**.

---

## 6. Option injection logic (deterministic resolution)

For each operation in the manifest:

1. Determine `command_type` = the `operation` field (e.g. `rebuild_index`).
2. Load the matrix entry for that `command_type`.
3. For each injectable option: it is **applicable** if
   `target_major >= min_major` **AND** `target_tier ∈ editions`.
4. Apply the override/auto resolution from §1.3 and the dependencies (RESUMABLE⇒ONLINE, etc.).
5. Build the final T-SQL string, merged into a **single** `WITH (...)` clause.

No SQL Server business rule is hard-coded: everything lives in `ddl_compatibility.yaml`. A new
version (2027…) = one line of YAML, no recompilation.

---

## 7. Compatibility matrix — `ddl_compatibility.yaml`

Structured **by `min_version` + edition** (no longer one row per year). For each `command_type`,
each option declares the **minimum major version**, the allowed **editions**, and any
**dependencies** (`requires`).

```yaml
# ddl_compatibility.yaml
major_to_year:  { 13: 2016, 14: 2017, 15: 2019, 16: 2022, 17: 2025 }
azure_pseudo_major: 9999          # EngineEdition 5 / 8

commands:

  rebuild_index:                  # ALTER INDEX ... REBUILD
    online:               { min_major: 9,  editions: [enterprise, azure] }
    wait_at_low_priority: { min_major: 12, editions: [enterprise, azure], requires: [online] }   # 2014
    resumable:            { min_major: 14, editions: [enterprise, azure], requires: [online] }   # 2017
    sort_in_tempdb:       { min_major: 9,  editions: [enterprise, standard, azure] }
    data_compression:     { min_major: 10, editions: [enterprise, azure] }
    maxdop:               { min_major: 9,  editions: [enterprise, standard, azure] }

  create_index:                   # CREATE [UNIQUE] INDEX
    online:               { min_major: 9,  editions: [enterprise, azure] }
    resumable:            { min_major: 15, editions: [enterprise, azure], requires: [online] }   # 2019
    wait_at_low_priority: { min_major: 16, editions: [enterprise, azure], requires: [online] }   # 2022
    sort_in_tempdb:       { min_major: 9,  editions: [enterprise, standard, azure] }
    data_compression:     { min_major: 10, editions: [enterprise, azure] }
    maxdop:               { min_major: 9,  editions: [enterprise, standard, azure] }

  alter_column:                   # ALTER TABLE ALTER COLUMN
    online:               { min_major: 13, editions: [enterprise, azure] }                       # 2016
    # NB: WAIT_AT_LOW_PRIORITY is NOT supported with ONLINE ALTER COLUMN, on any version.

  add_column:                     # ALTER TABLE ADD
    # no ONLINE; speed = conditional metadata-only (see §1.4), not injectable

  add_constraint:                 # ALTER TABLE ADD CONSTRAINT (PK / UNIQUE)
    online:               { min_major: 13, editions: [enterprise, azure] }
    resumable:            { min_major: 16, editions: [enterprise, azure], requires: [online] }   # 2022

  drop_index:        {}
  drop_column:       {}
  drop_constraint:   {}
```

Associated Go types:

```go
type OptionRule struct {
    MinMajor int      `yaml:"min_major"`
    Editions []string `yaml:"editions"`
    Requires []string `yaml:"requires"`
}
type CommandRules map[string]OptionRule          // option -> rule
type Matrix struct {
    MajorToYear      map[int]int             `yaml:"major_to_year"`
    AzurePseudoMajor int                     `yaml:"azure_pseudo_major"`
    Commands         map[string]CommandRules `yaml:"commands"`
}
```

### Subtleties encoded in the matrix (factual reminder)

- **ONLINE**: `CREATE INDEX` / `ALTER INDEX REBUILD` since 2005 (Enterprise). `ALTER COLUMN ONLINE`
  since **SQL Server 2016** (not 2022). Online index = **Enterprise/Developer/Azure only**.
- **RESUMABLE**: `ALTER INDEX REBUILD` (2017) → `CREATE INDEX` (2019) → `ADD CONSTRAINT` PK/UNIQUE
  (2022). Requires `ONLINE = ON`.
- **WAIT_AT_LOW_PRIORITY**: *partition switch* + `REBUILD` (2014) → extended to ONLINE `CREATE INDEX`
  (2022). **Not supported with `ONLINE ALTER COLUMN`, on any version.**

---

## 8. Execution & monitoring

The monitoring thread polls the server on **decoupled intervals** (a single 60 s interval is too
coarse: 60 s of blocking in production = an incident):

- `blocking_poll_seconds` (default **10**) — blocking detection.
- `log_poll_seconds` (default **60**) — log pressure.
- `progress_poll_seconds` (default **30**) — DDL progress.

### 8.1 Transaction log

```sql
SELECT total_log_size_in_bytes, used_log_space_in_percent
FROM sys.dm_db_log_space_usage OPTION (RECOMPILE);
```

⚠️ `used_log_space_in_percent` is a percentage of the file's **current size** (which autogrows). The
threshold that matters is the **absolute bytes used vs the accepted ceiling** (`log_max_size` from
config, bounded by `max_size` read from `sys.database_files`). So we watch the **absolute first**,
the percentage second.

If we cross the ceiling, we trigger the **reaction hierarchy** (§9). Interpreting
`log_reuse_wait_desc` (scoped to the connection's database via `DB_NAME()`):

```sql
SELECT log_reuse_wait_desc FROM sys.databases WHERE database_id = DB_ID();
```

- `LOG_BACKUP` (FULL/BULK_LOGGED recovery) → a log backup is needed: **we wait** (it usually arrives
  every 5–15 min). We **never** trigger a log backup ourselves (it would break the managed chain).
- `ACTIVE_TRANSACTION` → that is our own DDL.
- `AVAILABILITY_REPLICA` / `REPLICATION` → log held by a secondary / by replication.
- Under **SIMPLE** recovery only, a `CHECKPOINT` between operations can help (config option
  `checkpoint_between_operations`); under FULL it truncates nothing → useless.

If the log does not drain after `log_drain_timeout_minutes`, we abort the operation and log the
observed `log_reuse_wait_desc`.

### 8.2 Blocking

```sql
SELECT
    r.session_id, r.status, r.command,
    s.login_name, s.host_name, s.program_name,
    DB_NAME(r.database_id) AS database_name,
    r.total_elapsed_time, r.wait_type, r.wait_time,
    r.blocking_session_id, r.open_transaction_count,
    SUBSTRING(qt.text, (r.statement_start_offset/2)+1,
        ((CASE r.statement_end_offset WHEN -1 THEN DATALENGTH(qt.text)
          ELSE r.statement_end_offset END - r.statement_start_offset)/2)+1) AS active_query,
    qt.text AS parent_query
FROM sys.dm_exec_requests r
INNER JOIN sys.dm_exec_sessions s ON r.session_id = s.session_id
OUTER APPLY sys.dm_exec_sql_text(r.sql_handle) qt
WHERE s.is_user_process = 1
  AND (s.status <> 'sleeping' OR r.open_transaction_count > 0)
ORDER BY r.cpu_time DESC
OPTION (RECOMPILE, MAXDOP 1);
```

The tool knows its own `@@SPID`. It distinguishes two situations:

- **Our DDL is blocked** (by a schema lock, for instance): we follow `r.blocking_session_id` on
  *our* session.
- **Our DDL blocks others**: we identify the sessions whose `blocking_session_id = DDL_SPID`. We must
  **walk the chain up to the *head blocker*** (the direct blocker is not always the root).

When our DDL blocks other sessions beyond `blocking_timeout_minutes`, we enter the reaction hierarchy
(§9). We **log the text of the blocked queries**. Sessions matching the manifest's
`ignore_blocked_sessions` (§1.3) are **excluded here**, before the timeout is even started, so an
ignored blocker never counts as pressure; the engine still writes them, with full detail, to the
advisory `<manifest>.blocked.yaml` capture when it does react to a non-ignored blocker.

### 8.3 Progress

```sql
SELECT percent_complete, estimated_completion_time, total_elapsed_time
FROM sys.dm_exec_requests WHERE session_id = @DDL_SPID;
```

Available for `REBUILD`/`ALTER`: shown in the TUI and logged periodically → the operator knows
whether we are at 5 % or 95 % before deciding to cancel.

---

## 9. Reaction hierarchy (pressure: blocking or log)

When the tool must relieve pressure, it picks the **least destructive available** mechanism.
RESUMABLE and WAIT_AT_LOW_PRIORITY are not at the same logical level: WALP handles **lock
acquisition** (the SCH-M switch at start/end), RESUMABLE handles **abandoning the long central
phase**. We combine them when possible.

**Decision based on the capabilities of the operation in progress:**

1. **A RESUMABLE operation in progress → pause then `RESUME`.** *Preferred strategy.* A resumable
   operation commits incrementally. **The pause is performed by aborting the running statement itself**
   — the engine cancels the execution context, which sends an *attention* that aborts the in-flight
   `REBUILD`/`CREATE`. For a resumable operation that abort **pauses** it (work preserved, no rollback).
   We do **not** issue a separate `ALTER INDEX … PAUSE` on another connection: that competes for the
   same index lock and makes the running statement return an error that is easily mistaken for a
   failure. The pause **keeps the work already done** *and* **relieves log pressure** (the log becomes
   truncatable again). We then wait until the pressure (blocking / log) drops and run
   `ALTER INDEX … RESUME` **on the same pinned execution connection** (its SPID is unchanged because an
   attention does not close the connection). Costs to know: ~10–15 % slower, more log overall, and the
   partial index **consumes data space** until it completes or is aborted.

2. **WAIT_AT_LOW_PRIORITY** (injected with ONLINE): lets SQL Server handle the **lock wait** without
   blocking readers/writers. We **always inject `ABORT_AFTER_WAIT = SELF`** (the DDL self-cancels if
   it waits too long for the lock — never the user queries). A **dangerous** config option, disabled
   by default, allows `ABORT_AFTER_WAIT = BLOCKERS` (kills the user blockers). During the *central*
   phase, if it is other user queries that are blocked by the DDL, we apply the configured delay then
   move to point 3.

3. **Statement abort then `KILL`** (last resort, non-resumable operation). The same abort gesture as a
   pause stops the statement; for a non-resumable operation it rolls back, and the operation is retried
   from the start. An explicit `KILL <DDL_SPID>` is only a fallback when the abort does not stop the
   statement within `kill_grace_seconds` — note that a `KILL` terminates the **pinned execution
   connection**, so it precludes a later `RESUME` (another reason it is last resort). See §10.

**ADR influence:** with *Accelerated Database Recovery* enabled, the `ROLLBACK` of a `KILL` is nearly
instantaneous → the cost/benefit shifts in favour of `KILL`, and the insistence on RESUMABLE can be
relaxed. The tool factors the ADR state into the strategy choice.

After a cancellation, we wait until there is no more blocking / the log has dropped, then we
**retry the same operation** up to `max_retry_attempts` times.

**Per-operation escape hatch — `options.ignore_blocking: true`.** The whole hierarchy above reacts
to *blocking* and *log* pressure. Setting `ignore_blocking` on one operation removes **blocking**
from its pressure inputs: that operation never pauses/cancels *because it blocks others* — it holds
its lock to completion and leaves the blocked sessions waiting. The **log** branch still applies (a
log over cap still stops it). This is the deliberate inverse of the default "be a good citizen"
posture, for the rare index that must go through regardless. See §1.3.

**Manifest-level escape hatch — `ignore_blocked_sessions:`.** The same blocking-removal, but
*targeted*: only sessions matching one of the listed matchers (§1.3) are removed from the blocking
inputs, so the operation keeps yielding to everyone else. The filter is applied in the blocking
detector (§8.2) — a matching blocked session is never counted — so it composes with the whole
hierarchy unchanged. The log branch still applies. The matcher is re-read from the manifest on each
blocking poll, so an entry added mid-run (by hand or by the TUI) takes effect *before* the next
abort, without restarting; the live exclusions are carried into the recovery manifest so a resumed
run remembers them. In `--tui`, selecting a blocked session and pressing `i` (then a criterion)
writes the rule into the running manifest via a structured atomic rewrite, which the live reload
then picks up.

---

## 10. Clean `KILL` strategy (cancel vs kill)

Do **not** use `context.WithCancel` alone. If the driver does not propagate the cancellation to the
server, the DDL keeps running on the SQL Server side. Therefore:

1. Cancellation via the Go context.
2. If the DDL is still running on the server after `kill_grace_seconds`, the monitoring thread issues
   an explicit `KILL <DDL_SPID>` **on its own connection**.
3. **Rollback tracking**: `KILL <DDL_SPID> WITH STATUSONLY` to estimate the **rollback %** and
   log/display it — otherwise the operator thinks it crashed. A 2nd `KILL` does nothing: we
   **monitor** the progress, we do not re-issue it.

The log reserves the space needed for its own rollback (no risk of exhaustion *during* the
rollback), but the rollback keeps generating non-truncatable log until it completes — hence the
preference for RESUMABLE when possible.

---

## 11. Crash recovery / orphaned operations

If the tool dies (or is killed), the manifest is left in `02.processing/` and the DDL **may still be
running** on the server (orphaned session). On restart, for each manifest present in
`02.processing/`:

1. Read the **state sidecar** `<name>.state.json` written before execution, containing the **session
   signature**: `SPID` + `login_time` (`sys.dm_exec_sessions`) + a **GUID** placed in `CONTEXT_INFO`
   (or a unique `Application Name` in the connection string), + the exact command and the start
   timestamp.
2. The SPID alone is **unreliable** (it gets reused). We correlate **SPID + login_time + CONTEXT_INFO
   GUID** to confirm that a live session is indeed *our* orphaned DDL.
3. Consult `sys.dm_exec_session_wait_stats` / the request state, and above all
   `sys.index_resumable_operations`:
   - **orphaned resumable** operation → **resume it** (`RESUME`) rather than restart from scratch;
   - identified non-resumable orphaned session → decide per config: wait for it to finish, or
     `KILL` + idempotent resume;
   - no trace → clean restart from the beginning (idempotent guards from §1.5).

Resumable operations **survive a crash of the tool and a restart of the server**: their state is
persistent on the engine side. At startup, the tool **always** queries
`sys.index_resumable_operations` to adopt any orphaned operation instead of creating a new one (risk
of double execution).

### 11.1 Interruption *while the tool is still running* (external `KILL` / server restart)

The session signature is stamped at run start: the tool writes a random 16-byte marker into
`CONTEXT_INFO` on the execution session and records its `0x…` literal in the sidecar, so a reused
SPID cannot be mistaken for our run.

If the **DDL session is killed externally**, or the **server restarts** while the tool is still up,
the execution statement returns a connection error. The tool does **not** blindly mark the manifest
failed: for a resumable operation it queries `sys.index_resumable_operations` for the target index on
the monitoring connection (which survives the loss of the execution session). If a **`PAUSED`**
operation is found, the killed statement actually *paused* the work — so the manifest is reported
**`INTERRUPTED`** and **left in `02.processing/` with its sidecar**, to be resumed by the next run's
recovery rather than discarded to `04.failed/`. Only a genuine error with no paused operation behind
it is a failure.

**Server restart (full reconnection).** If the *whole server* restarts, the monitoring pool is lost
too, so the resumable check itself fails. The tool therefore **retries the check while the server
comes back**, up to `reconnect_timeout_minutes` (Go's `database/sql` pool re-establishes connections
once the server is up). If a conclusive answer is obtained, it is used. If the server stays
unreachable for the whole window, the answer is **inconclusive** — and a resumable operation is then
**kept for recovery anyway** (reported `INTERRUPTED`), since the next run will classify it correctly
(`Resume` if a paused op exists, `Restart` otherwise). This guarantees resumable work is never lost to
a transient outage or a server restart.

### 11.2 Aborting orphaned resumable operations

A paused resumable operation is not free: it **holds data space** for the partial index and **blocks a
concurrent rebuild of the same index** (error 10637) until it is resumed or aborted. When the work is
no longer wanted, the **`abort-resumable`** subcommand inventories the connected database's resumable
operations (`sys.index_resumable_operations`, with their schema/table resolved) and cancels each with
`ALTER INDEX … ABORT`. It targets `PAUSED` operations by default (`--include-running` to also stop
running ones), and `--dry-run` previews without changing anything.

### 11.3 Continue-on-failure & recovery manifests (`on_failure: continue`)

By default a manifest is **fail-fast**: the first operation that fails (after the reaction
hierarchy is exhausted, e.g. a `KILL` because the offline rebuild kept blocking other sessions)
sends the whole manifest to `04.failed/` and the remaining operations are not attempted.

A manifest may instead set **`on_failure: continue`** at top level. Then a failed operation is
**quarantined** and the engine **continues** with the next operation. When the loop finishes, if any
operation failed:

- the original manifest is moved to `04.failed/` and its run is reported **`PARTIAL`** (its `.log`
  lists every operation, success and failure, with the per-operation reaction timestamps and
  durations — so *which* objects were locked and for how long is recorded);
- a re-runnable **recovery manifest** `<name>.recovery.yaml` is written next to it in `04.failed/`,
  containing **only the failed operations** with their original options. It carries
  `on_failure: continue` itself, so it round-trips; move it into `01.to_run/` to retry just those.

A `PARTIAL` run counts as a failed manifest for the exit code (§16). A recoverable interruption
(paused resumable, §11.1) still wins over continue mode — such an operation is left in
`02.processing/` for recovery, never quarantined. Note that a recovery manifest for an `ALL` rebuild
contains the **expanded** per-index rebuilds (§1.6), so only the indexes that actually failed are
retried.

---

## 12. TUI — incident console

The `--tui` flag opens a real-time console. Beyond the progress display (`percent_complete`,
estimated time, rollback % if a KILL is in progress), it enables **manual decisions**:

- **Live list of sessions blocked** by our DDL, with detail: `login_name`, `host_name`,
  `program_name`, `wait_type`, duration, **query text**.
- **Live wait categories** for the DDL session (from `sys.dm_exec_session_wait_stats`), grouped
  into meaningful buckets — Locking, Data I/O, Transaction log, Parallelism, Memory, CPU &
  scheduling, Page latch (tempdb), Sort & spill I/O, Availability Group, Backup — with a total, so
  the operator sees *what* is slowing the operation.
- Actions (with **explicit confirmation**, clearly distinguishing the targets):
  - `KILL` a specific **user blocker**;
  - `KILL` **our DDL**;
  - `PAUSE` (if the operation is resumable);
  - **extend** the wait timer;
  - **snapshot** the current state to the `.log`.

---

## 13. Log format

Each completed manifest produces `<name>.log` with a **dual rendering**: a structured **JSON** block
(machine) + a readable **human summary**. Expected fields:

- timestamps (start/end), total and per-operation duration;
- for each operation: the **final generated** command, **injected options + justification** (version,
  edition, matrix entry, override);
- batches/operations executed successfully (for resume);
- retries / cancels / pauses, with the **text of the blocked queries** at the moment of decision;
- per operation, the **waits that slowed it down**, categorized (Locking, Data I/O, Transaction log,
  Parallelism, Memory, CPU, tempdb page latch, …) with summed time and task counts, computed as the
  delta of `sys.dm_exec_session_wait_stats` across the operation (noise/idle waits dropped);
- final `percent_complete`, `log_reuse_wait_desc` if aborted due to the log;
- SQL errors recovered *gracefully* (number, severity, message).

The `<name>.state.json` sidecar (§11) is deleted at the end of a successful run.

---

## 14. Notifications

Webhook / Slack / e-mail (configurable) on the events: **cancel**, **fail**, **pause**, **log full**,
**abort**. In production, the team must be alerted live, not discover the `.log` the next day.

---

## 15. Run history

Optional persistence (SQLite or a dedicated table, destination in `config.yaml`) of each run:
duration, retries, pauses, blocking, injected options, result. Lets you analyse trends and estimate
upcoming operations.

---

## 16. Exit codes

For CI / SQL Agent integration:

| Code | Meaning                                         |
|------|-------------------------------------------------|
| 0    | All manifests processed successfully            |
| 1    | At least one manifest failed (`04.failed/`)     |
| 2    | Configuration error / invalid manifest          |
| 3    | Server connection error                         |
| 4    | Global preflight failure (unhealthy server state) |

---

## 17. Security & permissions

- **No plaintext credentials.** The connection string and the password come from a **`.env`** file
  (not versioned), not from the YAML. Supports **Windows / Azure AD authentication** and
  **`encrypt=true`** in the string. Never log the full string / the password.
- **Minimum permissions** of the service account, to document:
  - `VIEW SERVER STATE` (and `VIEW DATABASE STATE`) for the monitoring DMVs;
  - `ALTER ANY CONNECTION` (or `processadmin` / `sysadmin`) for the `KILL`;
  - `ALTER` on the targeted objects (tables / indexes).

---

## 18. Settings — `config.yaml`

```yaml
database:
  # secrets via .env: ${DB_PASSWORD}, etc. — never in plaintext here
  connection_string: "server=localhost;database=MYDB;encrypt=true;trustServerCertificate=true;app name=SqlGoPace"
  login_timeout_seconds: 15      # connection only — NOT a query timeout
  # Driver query timeout = 0 (infinite): no global timeout on a DDL statement.
  # Duration control is delegated to monitoring (blocking / log), never a fixed timer.

directories:
  to_run:     "./01.to_run"
  processing: "./02.processing"
  done:       "./03.done"
  failed:     "./04.failed"

monitoring:
  blocking_poll_seconds: 10
  log_poll_seconds: 60
  progress_poll_seconds: 30
  log_max_size_bytes: 53687091200   # 50 GB — absolute ceiling (bounded by the log files' max_size)
  log_max_percent: 80               # secondary
  blocking_timeout_minutes: 5       # delay before reacting when our DDL blocks other sessions
  log_drain_timeout_minutes: 30     # give up if the log does not drain
  max_retry_attempts: 3
  kill_grace_seconds: 30            # delay before an explicit KILL if the Go cancellation does not take effect
  reconnect_timeout_minutes: 2      # wait this long for the server to come back before classifying an interruption
  checkpoint_between_operations: false  # only has an effect under SIMPLE recovery model

preflight:
  require_data_free_space: true     # requires ≈ the object's size free in the filegroup
  check_tempdb: true
  ag_send_queue_warn: true          # warns but does not block (configurable)

options_override:
  online:               { force: null }   # true / false / null(auto)
  resumable:            { force: null }
  wait_at_low_priority: { force: null }
  maxdop:               { force: null }
  sort_in_tempdb:       { force: null }
  # code vs auto priority: here everything is generated by the tool (no raw SQL),
  # so only the override/operation/matrix resolution applies (see §1.3).
  allow_abort_blockers: false        # DANGEROUS: allows ABORT_AFTER_WAIT = BLOCKERS

notifications:
  webhook_url: ""                    # empty = disabled
  on_events: [cancel, fail, pause, log_full, abort]

history:
  enabled: true
  destination: "sqlite://./sqlgopace_history.db"

matrix_file: "./ddl_compatibility.yaml"
```

---

## 19. Reference SQL queries

```sql
-- ADR enabled?
SELECT is_accelerated_database_recovery_on FROM sys.databases WHERE database_id = DB_ID();

-- Recovery model
SELECT recovery_model_desc FROM sys.databases WHERE database_id = DB_ID();

-- Resumable operations in progress / paused (crash recovery)
SELECT object_id, index_id, name, state_desc, last_pause_time, percent_complete, page_count
FROM sys.index_resumable_operations;

-- AG replica state (send queue / log truncation)
SELECT database_id, is_local, synchronization_state_desc,
       log_send_queue_size, redo_queue_size
FROM sys.dm_hadr_database_replica_states;

-- Log files' size / ceiling
SELECT name, size, max_size, growth FROM sys.database_files WHERE type_desc = 'LOG';
```

(See also §8.1 / §8.2 / §8.3 for the monitoring queries.)

---

## 20. Technical constraints (recap)

- **Separate connections** for execution / monitoring (§3).
- **Explicit `KILL`** in addition to the Go cancellation (§10).
- Driver **`microsoft/go-mssqldb`**, query timeout = 0, `login_timeout` on the connection side only.
- **No raw SQL parsing**: all DDL is generated from the declarative YAML manifests.
