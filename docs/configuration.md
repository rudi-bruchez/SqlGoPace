# Configuration

Two files drive a run. `config.yaml` holds policy, paths and connection details;
`.env` holds the secrets those details reference. Nothing else is read at startup except
the compatibility matrix and, for `plan`, the maintenance profile.

## Secrets

A password never appears in `config.yaml`. The connection string references environment
variables with `${VAR}`, expanded at load time from the process environment and from an
optional `.env` file in the working directory. A real environment variable always wins over
the file, so an operator can override one value for one command without editing anything,
and a missing `.env` is a silent no-op rather than an error.

```yaml
database:
  connection_string: "server=${DB_SERVER};database=${DB_NAME};user id=${DB_USER};password=${DB_PASSWORD};encrypt=true;trustServerCertificate=true;app name=SqlGoPace"
```

`.env` is gitignored, and `.env.example` is the committed template. The running version is
appended to `app name` automatically, so `sys.dm_exec_sessions.program_name` shows which
build is connected.

## Two different questions

This page answers "what happens if the key is absent". That is not the same as "what the
repository's `config.yaml` sets", and in three places they differ on purpose, because the
shipped file is deliberately more cautious than the bare default:

| Key | Default when absent | The shipped `config.yaml` |
|---|---|---|
| `preflight.require_data_free_space` | false | **true** |
| `history.enabled` | false | **true**, writing `./sqlgopace_history.db` |

If you copied that file, those are the values you have. It is the recommended starting
point, and `require_data_free_space: true` in particular can fail a first run's preflight,
which is the intended behaviour rather than a fault.

## Sections

| Section | Purpose |
|---|---|
| `database` | Connection string and login timeout. |
| `directories` | The four queue directories. |
| `monitoring` | Poll cadence, pressure ceilings, timeouts. |
| `preflight` | Which pre-run checks to perform. |
| `options_override` | Force a T-SQL option on or off globally. |
| `notifications` | Webhook and email, and the events that fire them. |
| `history` | Optional SQLite run history. |
| `shrink` | Tuning for the shrink driver. |
| `batch_dml` | Tuning for the batched-DML driver. |
| `kill_blockers` | Arms killing the sessions that block us. Off by default. |
| `kill_amplifying_maintenance` | Arms killing maintenance sessions we block. Off by default. |
| `matrix_file` | Path to `ddl_compatibility.yaml`, resolved relative to the config. |

## `database`

| Key | Default | Meaning |
|---|---|---|
| `connection_string` | required | Standard SQL Server connection string, with `${VAR}` references. |
| `login_timeout_seconds` | 15 | Connection timeout. This is **not** a query timeout. |

There is no query timeout anywhere in SqlGoPace, deliberately. A DDL statement's duration
is governed by the monitoring loop and the reaction hierarchy, never by a fixed timer: a
timer would abort a rebuild that was three hours in and about to finish.

## `directories`

```yaml
directories:
  to_run:     "./01.to_run"
  processing: "./02.processing"
  done:       "./03.done"
  failed:     "./04.failed"
```

All four are required. Relative paths resolve against the working directory, which matters
when a scheduler launches the binary from somewhere else than you do.

## `monitoring`

| Key | Default | Meaning |
|---|---|---|
| `blocking_poll_seconds` | required | How often to sample sessions and blocking. |
| `log_poll_seconds` | required | How often to sample transaction-log usage. |
| `progress_poll_seconds` | required | How often to read the operation's completion estimate. |
| `log_max_size_bytes` | required | Log-size ceiling that triggers the log reaction. |
| `log_max_percent` | required | Log-usage percentage ceiling, 1 to 100. |
| `blocking_timeout_minutes` | 1 | How long we may block another session before yielding. |
| `log_drain_timeout_minutes` | 30 | How long to wait for the log to drain before giving up cleanly. |
| `max_retry_attempts` | 0 | Retries after a recoverable failure. |
| `kill_grace_seconds` | 30 | Grace between cancelling a statement and the fallback `KILL`. |
| `reconnect_timeout_minutes` | 2 | How long to try to re-establish a lost monitoring connection. |
| `checkpoint_between_operations` | false | Issue a `CHECKPOINT` between operations. |

A poll interval of zero is rejected rather than accepted as a spin loop.

## `preflight`

| Key | Default | Meaning |
|---|---|---|
| `require_data_free_space` | false | Fail an index rebuild that does not have roughly its own size free in the database's data files. |

A rebuild materializes the new index before dropping the old one, so it needs room for a
second copy of the object. The check sums the free space of the database's `ROWS` files and
compares it against the size of each `rebuild_index` / `rebuild_heap` target, read from
`sys.dm_db_partition_stats`. The peak requirement is the largest single rebuild rather than
their sum, because each one releases its temporary copy before the next begins.

Autogrowth counts as headroom. A file that can still grow is not out of room, so the check
adds `max_size − size` to the free space before deciding, and a file with no cap (it grows
until the disk fills) can never be proven short — those cases **warn** rather than pass
silently, because the growth itself is a blocking zero-fill unless instant file
initialization applies. Only a rebuild that cannot fit *and* cannot grow fails.

One limit remains: `create_index` is not checked, because the index does not exist yet and
there is nothing to size.

### The `file growth` check

Read on every run, independent of `require_data_free_space`, and **advisory only** — it
never fails a manifest. It warns when:

- **a data file grows by a percentage.** The increment scales with the file, so it gets
  larger exactly as the file gets harder to grow. Microsoft's guidance is to set a fixed
  number of megabytes instead. The warning names what one growth event would cost at the
  file's current size, which is the number that makes the setting feel real: 10% of a
  14 TB file is a 1.4 TB blocking allocation.
- **a data file has autogrowth disabled while the manifest contains a shrink.** The shrink
  hands back space the file will not be able to reclaim, so the reclaimed headroom is
  one-way. Reported only for a shrink, since a fixed-size file is a legitimate choice
  otherwise.

There is deliberately no warning for a growth increment that is merely "too large".
Microsoft's own guidance is inconsistent on the threshold — one page suggests roughly an
eighth of the file as a testing rule of thumb, another recommends no more than 100 MB for
large files — so the check reports the increment and leaves the judgement to the operator.

## `options_override`

Each option takes `{ force: true }`, `{ force: false }` or `{ force: null }` (auto, the
default). A forced value applies to every operation in every manifest, so it is the
bluntest instrument in the tool; per-operation `options:` are the precise one.

```yaml
options_override:
  online:               { force: null }
  resumable:            { force: null }
  wait_at_low_priority: { force: null }
  sort_in_tempdb:       { force: null }
  maxdop:               { force: null }   # 0..32767 when forced
  allow_abort_blockers: false             # resolves ABORT_AFTER_WAIT = BLOCKERS
  wait_max_duration_minutes: 1            # MAX_DURATION for WAIT_AT_LOW_PRIORITY
```

`allow_abort_blockers` is a kill capability in disguise: it makes
`WAIT_AT_LOW_PRIORITY` abort the sessions blocking us rather than ourselves, and it needs
`ALTER ANY CONNECTION`.

## `notifications`

Fires a webhook and/or an email for the events listed in `on_events`.

| Event | Fires when |
|---|---|
| `fail` | A manifest failed. |
| `incomplete` | A shrink stopped short of target, work preserved. |
| `interrupted` | Ctrl+C or a drain stopped the run. |
| `run_failure` | The run itself stopped for a reason outside any manifest. |
| `pause`, `cancel`, `abort` | The reaction hierarchy acted. |

`run_failure` reports what no manifest can: the server is unreachable, the edition is
unsupported, crash recovery could not reconcile the queue, an engine failed to start.
Without it those exits appear only on stderr and in the exit code, so an unattended run
fails silently. It never doubles up with the per-manifest events.

Two exits stay silent by construction: a killed process, since nothing is left running to
send anything, and a `config.yaml` that cannot be read, since the channel settings live in
it. Keep an external watchdog on the exit code.

```yaml
notifications:
  webhook_url: ""
  on_events: [fail, incomplete, interrupted, run_failure]
  email:
    host: "smtp.internal.example"   # empty disables email
    port: 25
    from: "sqlgopace@example.com"
    to: ["dba-team@example.com"]
    username: ""                     # empty means an anonymous relay
    password: "${SMTP_PASS}"         # only used when username is set
    starttls: false                  # opportunistic STARTTLS before auth
```

Note that a kill never reaches these channels. `on_events` fires for `pause`, `cancel` and
`abort`, the reaction kinds meaning the run yielded; a kill is a different kind of event
and is reported in the `.log`, the sidecar, stdout and the TUI instead. Do not assume a
configured webhook will tell you about a killed maintenance job.

## `history`

```yaml
history:
  enabled: false
  destination: ""     # SQLite file path
```

## `shrink`

Entirely optional; every key defaults. These are global because they depend on the
instance's storage and SLA rather than on any manifest, and they are starting points and
bounds that the driver's dynamic calibration varies. An operator usually touches them only
for atypical storage.

```yaml
shrink:
  # The three initial_step_*_mb keys are the previous, tiered model. They are consulted
  # only when target_chunks is zero or less, and the shipped config.yaml still carries them.
  initial_step_small_mb:     100  # reclaim under 5 GB
  initial_step_medium_mb:    250  # 5 to 50 GB
  initial_step_large_mb:     500  # over 50 GB
  target_chunks:            1000  # aim to finish in about this many chunks
  max_step_pct_of_file:        5  # per-file ceiling, as a percent of the file (0 disables)
  min_step_mb:                50  # floor: below this, per-loop overhead dominates
  max_step_mb:              8192  # absolute ceiling: do not saturate I/O in one move
  max_chunk_seconds:         300  # ceiling on chunk duration: stops growth, never shrinks a step
  max_no_progress:             3  # consecutive no-gain chunks before stopping cleanly
  no_progress_before_flush:    2  # no-progress events before the tempdb cache flush
  no_progress_backoff_seconds:      30   # wait before retrying a stalled chunk, doubling
  no_progress_backoff_max_seconds: 300   # backoff ceiling
  self_wait_timeout_minutes:         5   # max wait while blocked before a clean stop
  log_reuse_wait_timeout_minutes:   30   # max wait for a scheduled BACKUP LOG
```

`target_chunks` is what sizes the first chunk: the driver divides the space to reclaim by
it. The three `initial_step_*_mb` keys are the previous, tiered model and are consulted
only when `target_chunks` is set to zero or less.

From there the step is adjusted between chunks: halved when the chunk cost the server too
much (WRITELOG or PAGEIOLATCH_EX past their thresholds, or the supervisor stopping the chunk
because it was blocking others), grown by a quarter after any chunk that was not, held once a
chunk reaches `max_chunk_seconds`.

`max_chunk_seconds` is a **ceiling, not a target**. It stops the step growing; it never shrinks
one. A chunk longer than it is not a problem to correct — `DBCC SHRINKFILE` restarts its
end-of-file page walk on every call, so small chunks pay that fixed cost over and over and are
the *slower* option on a large reclaim. Lower it only if you have a reason to want short chunks;
note that reactions, live `percent_complete`, and the `max_block_minutes` cap all apply *during*
a chunk, so a long one is neither blind nor unstoppable.

> **Renamed in v0.17.0.** This key used to be `shrink.target_batch_seconds`, defaulting to 5,
> and it gated growth rather than capping it — which pinned the step near `min_step_mb` on any
> large shrink. The name changed with the meaning, so a config still carrying
> `target_batch_seconds` under `shrink:` now **fails to load**, naming the key. That is
> deliberate: defaults only fill absent keys, so silently accepting the old one would have kept
> the old behaviour with nothing said. Rename it to `max_chunk_seconds` and reconsider the
> value — 5 was right for a target, it is far too low for a ceiling. The identically named
> `batch_dml.target_batch_seconds` is unaffected and keeps its meaning.

## `batch_dml`

Also optional, same reasoning.

| Key | Default | Meaning |
|---|---|---|
| `initial_small_rows` | 1000 | First batch on a table estimated under 100k rows. |
| `initial_medium_rows` | 5000 | 100k to 1M rows. |
| `initial_large_rows` | 20000 | Over 1M rows. |
| `min_rows` | 100 | Batch floor. |
| `max_rows` | 100000 | Batch ceiling. |
| `escalation_cap_rows` | 4000 | Tighter ceiling when RCSI is off, to avoid lock escalation to a table lock. |
| `target_batch_seconds` | 5 | Ideal per-batch duration. |
| `self_wait_timeout_minutes` | 5 | Max cumulative wait while blocked before a clean stop. |

## `kill_blockers` and `kill_amplifying_maintenance`

Both are off by default and both need `ALTER ANY CONNECTION`. They kill in opposite
directions and are covered together in
[`blocking-and-kills.md`](blocking-and-kills.md), which is the page to read before
enabling either.

```yaml
kill_blockers:
  enabled: false
  default_after_seconds: 60

kill_amplifying_maintenance:
  enabled: false
  min_blocked_behind: 1
  after_seconds: 60
  commands: []          # empty uses the built-in allow-list; a non-empty list REPLACES it
```
