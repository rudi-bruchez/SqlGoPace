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

- [`docs/running.md`](../../../docs/running.md) — modes, flags, the queue, stopping a run,
  and what a re-run repeats after each kind of ending.
- [`docs/blocking-and-kills.md`](../../../docs/blocking-and-kills.md) — the reaction
  hierarchy and the three session-policy features.
- [`docs/permissions.md`](../../../docs/permissions.md) — when a failure is a missing grant.

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

Prefer `--tui` when the user is present and the operation is significant: it shows the
sessions being blocked, the ones blocking, and offers `x` kill a blocker, `i` ignore one,
`k` kill the DDL, `p` pause, `d` drain.

**Stopping safely.** The first Ctrl+C drains: a resumable operation is paused with its work
preserved, a non-resumable one finishes, then the run stops. A second Ctrl+C is a hard stop.
Tell the user which of the two they are about to press. A drained manifest stays in
`02.processing/` and the next run continues rather than replaying.

## Reading the outcome

Every manifest leaves a `.log` beside it in `03.done/` or `04.failed/`. It records the
statement executed, every option decision with its reason, every reaction, and the timings.
Read it before speculating.

| What you see | What it means |
|---|---|
| `done:` | Success. Nothing to do. |
| `failed: … preflight failed` | It never touched the object. Read the named check: a missing grant, a missing object, or a pressure ceiling already exceeded. |
| `failed: operation N (…): …` | The statement itself failed. The message is SQL Server's, quoted verbatim. |
| `incomplete` | A shrink stopped short of target with its work preserved. Not a failure; re-run to continue. |
| `interrupted` | Ctrl+C, a drain, or a closing window. The manifest is resumable where it stopped. |
| `PARTIAL` | `on_failure: continue`: some operations were quarantined. A `.recovery.yaml` holds only those. |
| `pause` / `cancel` / `abort` in the log | The reaction hierarchy acted. The detail line says what triggered it. |
| `hold:` | The operation deliberately held its lock through a session an `ignore_blocked_sessions` rule named. |
| `warn: stopped killing blockers matching rule …` | The repeat-offender cap hit. The blocker is returning faster than it can be cleared; that is an operator problem, not a tuning one. |

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
  behaviour and the killers are off by default for a reason. Establish the direction from a
  sidecar first.
- **Quote the log rather than paraphrasing it.** SQL Server's message and its number are the
  thing the user can search for.
