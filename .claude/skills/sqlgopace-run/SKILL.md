---
name: sqlgopace-run
description: >-
  Run a SqlGoPace manifest against a real SQL Server and watch it, then read what came back.
  Use when the user wants to execute a queued manifest, is running one now, or is asking
  about a run that already happened: what a reaction in the log means, why an operation
  yielded or was killed, why a manifest landed in 04.failed, what to do with a
  .blocked.yaml / .contended.yaml / .amplifiers.yaml sidecar, whether a re-run will repeat
  work, or how to stop a run safely. Triggers include "run this manifest", "it's blocking",
  "the shrink stopped short", "what does this .log mean", "it failed preflight", "can I
  Ctrl+C this". For writing or reviewing the manifest itself, use sqlgopace-operator.
---

# Running a manifest, and reading what happened

Executing DDL mutates a production server. Treat every run as the user's decision, not
yours.

## Read these first

- [`docs/running.md`](../../../docs/running.md), for modes, flags, the queue, stopping a run,
  and what a re-run repeats after each kind of ending.
- [`docs/blocking-and-kills.md`](../../../docs/blocking-and-kills.md), for the reaction
  hierarchy and the three session-policy features.
- [`docs/permissions.md`](../../../docs/permissions.md), for when a failure is a missing grant.

## Before running anything

1. **Dry-run first, always.** `--dry-run --explain` renders the exact statements, takes no
   lock and touches nothing. Show the user the generated T-SQL and the decision lines, and
   let them confirm. If an option was dropped, the decision line says why, message number
   included.
2. **Check the grants match the manifest.** A `shrink` or `check_db` needs `db_owner`;
   `shrink_tempdb` needs `sysadmin`; batched DML needs `SELECT` plus `UPDATE`/`DELETE`; any
   kill feature needs `ALTER ANY CONNECTION`. `docs/permissions/99-verify.sql` reports what
   the login can actually do, using the same probes preflight uses.
3. **Ask about the window.** A long rebuild started at the wrong hour is the failure mode
   this tool exists to avoid. A `window:` block defers the manifest instead.

## Running it

```bash
sqlgopace --config config.yaml            # drain the queue, silently
sqlgopace --config config.yaml --tui      # the same run, watched
```

Prefer `--tui` when the user is present and the operation is significant. Its list is the
sessions the DDL is **holding up**, so `i` writes an `ignore_blocked_sessions` rule for the
selected one (hot-reloaded) and `x` kills it, after a confirmation naming its login, host and
open transactions — killing it frees nothing, it only discards that session's work. `b` opens
the blocker roster, the sessions that have blocked *us*, and is the only place a
`kill_blocking_sessions` rule can be armed from the console — arming confirms too, since
0.32.0, because the rule kills every *future* session matching the group, not the one on
screen; disarming does not ask. `k` kills the running DDL after a confirmation stating the
cost; on an edition where the operation is resumable that is a pause, which is why there is no
pause key. `d` drains. The full set is `i`, `x`, `b`, `k`, `d`, `enter`, `?`, `q` and the
arrows.

The three gestures that ask are ordered by what they cost: `x` ends one named session, `k`
ends our own operation, arming ends an unbounded set for the rest of the run.
`internal/tui/harm_audit_test.go` holds that ordering and fails if a key ever becomes cheaper
than a less harmful one — so **read the key table in
[`docs/running.md`](../../../docs/running.md) rather than this paragraph** when it matters.
This paragraph is a summary and has gone stale before.

**Stopping safely.** Without `--tui`, the first Ctrl+C drains: a resumable operation is
paused with its work preserved, a non-resumable one finishes, then the run stops. A second
Ctrl+C is a hard stop. Tell the user which of the two they are about to press. **Under
`--tui` the drain key is `d`**: bubbletea reads Ctrl+C as a keystroke, not a signal, so it
closes the console like `q` and the run keeps going in the background — it says so on the way
out. A drained manifest stays in `02.processing/` and the next run continues rather than
replaying.

## Reading the outcome

Every manifest leaves a `.log` beside it in `03.done/` or `04.failed/`. It records the
statement executed, every option decision with its reason, every reaction, and the timings.
Read it before speculating.

| What you see | What it means |
|---|---|
| `done:` | Success. Nothing to do. |
| `failed: … preflight failed` | It never touched the object. Read the named check: a missing grant, a missing object, a pressure ceiling already exceeded, too little free data space for a rebuild (`preflight.require_data_free_space`, on by default since 0.31.0 — it was off when the key was absent), a filter that spares no row (`confirm_full_table`), or a `key_range` table whose clustered key is not single-column, integer and unique. |
| `failed: operation N (…): …` | The statement itself failed. The message is SQL Server's, quoted verbatim. |
| `incomplete` | A shrink stopped short of target, or (since 0.25.0) a batched DML ended on log pressure, blocking or its `self_wait_timeout_minutes` budget. Committed work is preserved, so it is not a failure — but it lands in `04.failed/`, labelled INCOMPLETE and counted apart. Re-run to continue. |
| `interrupted` | Ctrl+C, a drain, or a closing window. The manifest is resumable where it stopped. |
| `PARTIAL` | `on_failure: continue`: some operations were quarantined. A `.recovery.yaml` holds only those. |
| `pause` / `cancel` / `abort` in the log | The reaction hierarchy acted. The detail line says what triggered it. |
| `hold:` | The operation deliberately held its lock through a session an `ignore_blocked_sessions` rule named. |
| `warn: execution connection re-pinned: SPID a -> b` | The statement before it left the pinned connection unusable, so the run continued on a new server session (0.33.0). Session ids before and after that line refer to different sessions. Not an error on its own; read the failure just above it. Only DDL operations emit it — a shrink or a batched DML is repaired the same way and stays silent. |
| `is still running the abandoned statement after 2m0s` | The run cancelled an operation, could not confirm SQL Server stopped it, and the session would not clear. It refuses to run more DDL beside it, so the rest of the manifest fails the same way. This is a server-side rollback still in progress: check `sys.dm_exec_requests` for that session, let it finish, then re-run. Do not "fix" it by re-running immediately. |
| `warn: stopped killing blockers matching rule …` | The repeat-offender cap hit. The blocker is returning faster than it can be cleared; that is an operator problem, not a tuning one. |

**"It exits immediately saying another run holds the directory."** Not an error to work
around. Since 0.22.0 a run takes an OS lock on `02.processing/` and names the process holding
it; two runs sharing a queue used to requeue each other's in-flight manifests. Wait, or give
the second schedule its own `directories.processing`.

## What to do with a sidecar

None of these are read back by the tool. Acting on one is always a deliberate step, and
copying an entry into a manifest is a decision to put to the user.

- **`.blocked.yaml`** lists sessions *we* blocked. Its suggested entries belong to
  `ignore_blocked_sessions`, never to `kill_blocking_sessions`. Before proposing any of
  them, check `open_transactions`: a read-only `SELECT` at zero costs nothing to keep
  waiting; an `INSERT` with open transactions is a different conversation.
- **`.contended.yaml`** lists objects a shrink could not get past, tagged by how that was
  established. Feed it back with `sqlgopace plan --confirmed <path>` to reorganize exactly
  those, then shrink again. That loop is what makes a stubborn shrink converge.
- **`.amplifiers.yaml`** lists maintenance sessions killed for amplifying our block into an
  outage, with a ready `sp_update_job` statement per job. SqlGoPace never disables a job
  itself; propose the statement, do not run it.
- **`.heaps.yaml`** lists heaps a shrink cannot benefit from, because reorganize cannot
  compact a heap's in-row data. They need a rebuild in a window.

## Diagnosing the common ones

**"It keeps yielding and never finishes."** One trivial session is tripping the blocking
timer. Read `.blocked.yaml`, identify it, and propose an `ignore_blocked_sessions` rule with
`options.max_block_minutes` as a backstop. Do not reach for the killer first.

**"It says the table does not exist, but it does."** Almost always permissions: without
rights on the object, `OBJECT_ID(...)` returns NULL.

**"The shrink stopped short."** Expected, and reported as `incomplete` rather than failed.
Read `.contended.yaml` for what pinned the file end. Bringing a very large tempdb down live
is often impossible without a restart.

**"A kill did nothing."** Either the login lacks `ALTER ANY CONNECTION`, in which case
preflight warned rather than failed, or the rule is in the wrong direction and never
matched. Check the direction before the grant: it is the more common error.

**"Will a re-run redo the work?"** Depends on how it ended, and the three cases differ. See
the table in [`docs/running.md`](../../../docs/running.md); do not answer from memory.

## Rules for the assistant

- **Never run a manifest the user has not seen rendered.** Dry-run, show, confirm, then run.
- **Never invent a fix by editing generated SQL.** There is no path for that: change the
  manifest and re-render.
- **Never propose a kill rule as a first response to blocking.** The yield is the designed
  behaviour and the automatic killer ships disarmed for a reason. `kill_blockers.enabled` is
  that arm and nothing else: the console's `x` and `k` kill by hand whatever it says. Establish
  the direction from a sidecar first.
- **Quote the log rather than paraphrasing it.** SQL Server's message and its number are the
  thing the user can search for.
