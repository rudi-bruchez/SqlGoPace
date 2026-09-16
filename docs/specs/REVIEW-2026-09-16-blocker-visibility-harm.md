# Harm review — blocker visibility (0.36.0–0.38.0)

Date: 2026-09-16. Tree at `7c7e33a`, version 0.38.0.
Scope: the three changes since the 0.34.0 whole-system review
([REVIEW-2026-09-16-harm.md](REVIEW-2026-09-16-harm.md)) — operation durations on the console
row (0.36.0), the manifest name in the panel title (0.37.0), and blocker visibility
(0.38.0, [BLOCKER-VISIBILITY.md](BLOCKER-VISIBILITY.md)).

A historical record. It is not updated as items are fixed.

## What ran, and what did not

| | Result |
|---|---|
| `govulncheck ./...` | 0 vulnerabilities reachable from this code. 1 in an imported package and 18 in required modules, none called. |
| `gosec ./...` | 16 findings, all pre-existing and none in the files this work touched: 5× G301 (directory permissions), 7× G304 (file opened from a variable path), 4× G306 (write permissions). Same set as the 0.34.0 review. |
| `semgrep --config auto` | **Did not run.** Its rulesets are fetched from `semgrep.dev` and this machine sits behind a TLS-intercepting proxy: `CERTIFICATE_VERIFY_FAILED … unable to get local issuer certificate`. Neither `--config auto` nor `--config p/golang` can reach the registry. Not a clean result — an absent result. |
| Independent readers | One reader (`agy`) on a clone with credentials parked. `opencode` and `kimi` are not installed on this machine, so the panel is one, not three. **A panel of one that returns nothing has told us about its own question, not about this code.** |

## Findings, worst first

### 1. SEVERE — a kill rule's `after_seconds` silently depends on `blocking_poll_seconds`, and 0.38.0 gives operators a reason to lower it

**Where.** `internal/run/kill.go:167` (`sincePoll`), `internal/run/recidivism.go:80`
(`accrueSince`), `internal/run/recidivism.go:90` (`forgetMarksExcept`),
`docs/blocking-and-kills.md:171`.

**What goes wrong.** A manifest's `kill_blocking_sessions` rule reads as a wall-clock promise:

```yaml
kill_blocking_sessions:
  - login_name: "^svc_dashboard$"
    after_seconds: 30
```

It is not one. Blocking debt is banked poll by poll, and **an episode's first observation
banks nothing** — deliberately, because the blocker's true start is unknown. So every episode
discards up to one `blocking_poll_seconds` of the time it actually blocked. With the shipped
`blocking_poll_seconds: 10`, a rule written as 30 s fires somewhere between 30 and 40 seconds
of real blocking for a continuous offender, and for a bursty one — a dashboard that blocks in
four-second stabs, a connection pool retrying — it may **never** fire, because
`forgetMarksExcept` clears the poll mark of an identity absent from a poll, making its next
sighting a first sight again, banking zero.

Change `blocking_poll_seconds` from 10 to 2 and every kill rule in every manifest tightens,
by up to 8 seconds per episode, with no rule edited and nothing said. For the bursty offender
the change is qualitative rather than marginal: what was effectively immune starts
accumulating debt and gets killed.

**Who it happens to.** An operator who lowers `blocking_poll_seconds`. Before 0.38.0 few had
a reason to. **0.38.0 supplies one**: this release documents that key as also driving the
console's blocked-sessions panel (`docs/configuration.md:95`, `docs/running.md`), so an
operator who wants a fresher console now knows exactly which number to reduce — and nothing
tells them it also tightens what gets killed. The hazard is pre-existing; this release raised
the probability of it being triggered.

**Evidence.** `sincePoll` returns zero when `lastPoll.IsZero()`, and `resetEpisode` zeroes
`lastPoll`. `accrueSince` banks nothing on a bucket's first observation. `forgetMarksExcept`
documents the intent plainly: *"its next observation is a first sight again"*.
`docs/blocking-and-kills.md:171` states the first-poll rule but never connects it to the poll
interval, and `docs/configuration.md` describes `blocking_poll_seconds` as a sampling cadence
with no mention of kill timing.

**Smallest fix — documentation, not code.** The mechanism is sound; the promise is what
misleads. One sentence in `docs/blocking-and-kills.md` beside the existing first-poll
paragraph: *the effective delay is `after_seconds` plus up to one `blocking_poll_seconds` per
episode, so lowering the poll interval makes every kill rule fire sooner, and a blocker that
comes and goes may never accumulate its delay at a coarse interval.* And a matching clause on
the `blocking_poll_seconds` row in `docs/configuration.md`. Both are two lines and neither
changes behaviour.

### 2. MODERATE — the roster's episode counts changed meaning in 0.38.0, and they are what an operator arms a kill rule from

**Where.** `cmd/sqlgopace/main.go:956` (`suspensionTracker.observe`), the console feed's new
cadence in the same file, `internal/tui/view.go` (`rosterGroups`).

**What goes wrong.** `observe` derives episodes from sampling: a new episode is counted when a
poll sees us blocked after a poll that did not, or when the blocking SPID changes between two
polls. 0.38.0 moved that observation from `progress_poll_seconds` (30 s) to
`blocking_poll_seconds` (10 s). For identical server behaviour the roster now reports **more
episodes and a higher per-login count**: gaps that fell between two 30-second polls are now
seen, and blocks entirely contained in a 30-second gap, previously invisible, are now counted.

The roster is where a kill rule is armed, which `CLAUDE.md` names the most destructive gesture
in the console and `harm_audit_test.go` ranks as `harmEndManyOngoing` — the highest. An
operator carrying a mental baseline from an earlier version ("this login shows two episodes on
a normal run") reads the higher post-upgrade numbers as a worsening peer rather than as a
sampling change, and arms a rule on that reading.

**Who it happens to.** Anyone upgrading who uses the roster, on the default path. No
configuration is involved.

**Bounded by.** These numbers are console-only — `SuspensionMsg` does not reach
`internal/report`, so no `.log` or history row carries them and no cross-run comparison is
built on them.

**Smallest fix — documentation.** The CHANGELOG's 0.38.0 entry names the cadence change but
not its effect on the counts. One sentence saying episode counts and per-login counts are
sampled, are finer from 0.38.0 on, and are not comparable with earlier runs.

### 3. MODERATE — a blocker too brief to matter is now listed and killable, which is the case the deleted gate was written for

**Where.** `cmd/sqlgopace/main.go` (`blockersOf`, replacing `blockerGate`),
`internal/tui/model.go:672` (`x` → `modeKillConfirm`).

**What goes wrong.** The removed `blockerGate` carried its own rationale: *"so a fleeting block
(e.g. a page-reclaim latch during a shrink) never prompts the operator"*. That case is real and
0.38.0 deliberately surfaces it. A session blocked on us for three seconds now appears in the
panel, and `x` kills it — which, as the code comment at `model.go:668` says, **frees nothing**
and rolls back whatever that session had open.

The design argument in `BLOCKER-VISIBILITY.md` holds — the guard on `x` is `modeKillConfirm`,
the harm ledger passes untouched, and hiding a row was never a guard. But the argument covers
the *gesture*, not the *judgment*: the console now presents rows it never presented, and offers
no cue distinguishing a three-second latch from a ten-minute transaction.

**Bounded by.** The row already renders `wait=` — the evidence is on screen, and it is what the
debounce encoded implicitly. The confirmation prompt is unchanged.

**Who it happens to.** An operator watching a shrink, on the default path.

**Smallest fix — documentation, and it is already half done.** `docs/running.md` gained a
sentence in this release saying nothing is filtered for being recent and that the wait is the
evidence to judge by. Worth one more clause naming the shrink latch specifically, since that is
the known transient and an operator who kills it has paid a rollback for nothing.

### Not findings

- **The 0.36.0 and 0.37.0 changes.** Operation duration on a finished row and the manifest
  name in the panel title are render-only: both read values already computed and already
  written to the `.log`, neither reaches a decision path. Nothing to report.
- **gosec's 16 hits.** Opened; all pre-existing, all in path handling and directory creation
  that the 0.34.0 review already triaged. Nothing this work touched.

## What the software gets right

The separation that made finding 1 possible to reason about is real and deliberate: the
reaction path samples independently of the console, `blockerGate` had exactly one consumer, and
the split is documented where it matters. The destructive gestures are ranked in a test that
walks the type rather than a diff, and that test is why finding 3 could be dismissed as a
gesture question and kept as a judgment question. `sincePoll`, `accrueSince` and
`forgetMarksExcept` each carry a comment stating what they deliberately do *not* do — which is
how a reviewer reconstructs an intended semantics instead of guessing at one.

## Is it responsible to ship 0.38.0 as it stands?

**Yes, with one documentation change first.** Nothing here is a code defect, and no finding
makes the tool more dangerous than 0.34.0 was. In order:

1. **Finding 1's two sentences** (`docs/blocking-and-kills.md`, `docs/configuration.md`). Do
   this before the tag. This release is what turns a latent coupling into one an operator is
   invited to trip, and the cost of saying so is two lines.
2. **Finding 2's sentence** in the CHANGELOG's 0.38.0 entry. Before the tag, same paragraph as
   the migration note already there.
3. **Finding 3's clause** in `docs/running.md`. Can wait for the next docs pass.

## The shortest honest warning the README should carry

> `blocking_poll_seconds` is not only a sampling rate. Every kill rule's delay is measured
> over polls, and each blocking episode discards its first one — so lowering this value makes
> every `kill_blocking_sessions` rule fire sooner, and raising it can stop an intermittent
> blocker from ever reaching its delay. Change it deliberately, and re-read your kill rules
> when you do.
