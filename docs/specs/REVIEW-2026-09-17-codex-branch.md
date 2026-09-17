# Second external reader's report — codex on `feat/tui-server-load`, 2026-09-17

A second `codex` run, this one a general code review of the branch rather than a harm review, made
after 0.40.0 landed. The reader's own report was written in French at the repository root; the
repository is English-only, so that file was removed and this is its record. What follows is **not**
the reader's text: it is each finding restated in English with the verdict of the verification this
session ran against the code, so a later reader can tell a checked claim from a hypothesis.

Numbering is the reader's own (F-01 … F-12), kept so the two documents can be compared.

The reader ran `go test ./...`, `go test -race ./...`, `go vet ./...` and `gofmt -l .` and reported
them clean. It did not run the integration suite: no DSN was available to it, and the rules of
engagement forbade reaching a real instance. So nothing it says about DMV, `KILL`, rollback or
shrink behaviour on a live server was verified by running.

---

## Confirmed by reading the code

### F-03 — the tempdb shrink arms killers a documentation page promises it never uses

`docs/shrink.md:129` carries a section headed **"It never kills a blocker"**, whose first sentence
is "Live sessions blocking a tempdb shrink are always waited out, never killed: they are legitimate
application queries." `cmd/sqlgopace/main.go` attaches both `blockerKiller` and `victimKiller` to
the tempdb sampler, with a comment arguing the opposite — that a tempdb shrink must be able to kill
a blocker like any other operation.

With `kill_blocking_sessions` armed globally, a tempdb shrink therefore kills application sessions
that a page of the operator documentation promises it will not touch. This is the class the harm
review exists to find: the documentation is *more* reassuring than the code, so the operator who
read the page is the one who gets hurt.

### F-03, second half — the tempdb shrink watches the wrong database's log

The same wiring passes `conn` (the user database) as the probe and `tempdbConn` only as the session.
The in-code comment justifies it with "DMV reads are instance-wide, so probing stays on conn". That
is true of `sys.dm_exec_requests` and `sys.dm_exec_sessions`, which is why session detection works.
It is false of both log reads: `logSpaceSQL` selects from `sys.dm_db_log_space_usage`, which reports
the **connected** database, and `logReuseWaitSQL` filters `WHERE database_id = DB_ID()`. During a
`shrink_tempdb`, log monitoring therefore watches the user database's log — reacting to pressure
that has nothing to do with the operation, and blind to the pressure that has.

The comment is what hid this: it is right about the DMVs it was written for and was never revisited
when the log reads joined the same sampler.

### F-01 — the fallback KILL verifies only the session number

`Conn.Kill` issues a bare `KILL <spid>`. The three runners call it after `kill_grace` expires with
whatever `SPID()` returns at that moment.

The codebase already holds the check that is missing: `stopOrphan` reads `SessionIdentity` and
refuses to act unless the session still exists, is still active, and its `login_time` matches the
one recorded when the session was pinned. The fallback path does not reuse it. The window is real
rather than theoretical precisely because `SPID()` must be re-read on every use — the execution
session *is* replaced when a canceled statement poisons the connection, and a server that has
reclaimed a session number is free to hand it to somebody else.

### F-04 — the plan fingerprint does not cover what an operation does

`planFingerprint` hashes `CommandType` and `opTarget` per operation and nothing else, so editing
`set`, `set_raw`, `where`, `where_raw`, compression, partition or options between an interruption
and a resume leaves the fingerprint unchanged and the cursor believed.

Second half, also confirmed: `reconcileResumePlan` compares the fingerprint only under
`if resumeFrom > 0`, while `updateSidecar` rebinds it unconditionally at the end of the function. A
run interrupted during the first operation therefore keeps its `key_range` watermark, adopts the
edited plan's fingerprint without comparison, and resumes behind a watermark recorded against
different SQL.

### F-07 — releasing the queue lock removes the file, which reopens the race

`QueueLock.Release` unlocks, closes, then `os.Remove`s the lock file, with a comment calling the
removal cosmetic. On Unix that is the classic flock/unlink race: a second process can open and lock
the old inode inside the window, the first process then unlinks the inode it still holds, and a
third process creates a fresh file of the same name and takes an independent lock on it. Two runs
then believe they own the queue, and recovery in one can requeue the other's in-flight work.

The removal being cosmetic is exactly the argument for not doing it.

### F-02, the part 0.41.0 does not cover — a failed reuse-wait read discards a known over-cap

`ServerSampler.Log` reads `LogSpace`, computes `overCap`, and when over cap reads `LogReuseWait` to
attribute it. If that second read fails it returns `LogSample{}` and the error — throwing away the
threshold breach, which was already known, along with the attribution, which was not.

The rest of F-02 (both polls on one goroutine; a failing or hanging poll leaving the supervisor on
stale state) is fixed in 0.41.0.

## Superseded or declined

### F-06 — half fixed, half deliberate

The reader is right that the unchunked shrink phases react to `max_block_minutes` and to nothing
else, and that a manifest which omits it had no yield at all. That half is fixed in 0.41.0: an
absent key now resolves to two minutes. That those phases do not react to log pressure or to
`blocking_timeout_minutes` is a documented decision (`SHRINK.md` §9), not an oversight: there is no
chunk boundary to pause at, and aborting stays the operator's call.

### F-08 — the declared trust boundary, not a defect

`set_raw`, `where_raw`, `type` and `data_compression` reach generated SQL without a complete
allowlist. The reader states itself that `SECURITY.md` declares this: write access to `01.to_run/`
is equivalent to the SQL login's own power. An allowlist for `type` and `data_compression` would
still narrow the surface and is worth doing on its own merits; it is not a discovered vulnerability.

### F-05, F-09 … F-12 — not verified this pass

F-05 (`key_range` is at-least-once across a crash, because the range commits before its watermark is
saved, so triggers and audit rows can fire twice) is plausible and, if confirmed, is a documentation
fix: name the guarantee rather than imply idempotence from the literal-`set` restriction.

F-09 (the three runners duplicate the execute/sample/react/cancel mechanism), F-10 (`updateSidecar`
returns silently when the sidecar cannot be read), F-11 (two near-identical alarm latches) and F-12
(the feature surface is wide for a beta) are maintainability opinions rather than defects. F-10 is
the one of the four with a harm argument and deserves a look when the resume path is next touched.

## What it confirms the project gets right

Identifiers go through `quoteIdent` with `]` escaped; DMV queries use `sql.Named` rather than
concatenation; automatic `KILL`, amplifier kills and `ABORT_AFTER_WAIT = BLOCKERS` are all off by
default; manifest and state writes are atomic replacements; errors are wrapped with `%w` and `Rows`
walks check `rows.Err()`. It also confirms the 0.40.0 connection-string fix: the shipped template
enables TLS without `trustServerCertificate=true`, and the documentation explains the exception and
its cost.
