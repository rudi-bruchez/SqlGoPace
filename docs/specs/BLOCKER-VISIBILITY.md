# Blocker visibility in the console

Status: designed 2026-09-16. Small, and deliberately so — the design work was finding out
where the latency actually came from.

## The symptom

From the field: *"blocked sessions take too long to appear in the console, especially after I
have already killed one process."*

## Where the latency comes from

Not from the monitoring sampler. `pumpSamples` (`internal/run/executor.go`) feeds the
*reaction* path and never reaches the console's blocked-sessions panel. Two other things do.

**The panel runs on the wrong key.** `feedConsole` is started with
`cfg.Monitoring.ProgressPoll()` (`cmd/sqlgopace/main.go:407`), so the blocker list is refreshed
every `progress_poll_seconds` — **30 s** by default — while the key that names this cadence,
`blocking_poll_seconds`, is 10 s and drives something else entirely. That is a wiring accident,
not a decision anybody recorded.

**And the panel withholds every blocker for a full minute.** `blockerGate.persistent`
(`cmd/sqlgopace/main.go:926`) records each blocker's first sighting and surfaces it only once
`now - firstSeen >= blocking_timeout` — **60 s** by default. Together: a session this run is
blocking is invisible for at least 60 s, and for up to 90 s.

**The "after a kill" half is the gate's reset.** When a session stops blocking, `persistent`
deletes its `firstSeen` entry. The next session in the blocking chain is a different SPID, so
its minute starts from zero. Every kill therefore buys another full minute of blindness —
exactly when an operator is working the chain and needs to see the next link.

## The decision

**Detection and display are decoupled from the reaction.** Show every blocker as soon as it is
seen; leave every kill timer exactly where it is.

This is safe because the two paths were already separate: `blockerGate` has exactly one
consumer, the console feed. No reaction, automatic or otherwise, reads it. Nothing about when
the engine cancels, pauses or kills changes here.

### Why the debounce was not a safety guard

It is tempting to read the minute as protection: the panel is also the roster from which an
operator arms a kill rule, and `x` kills a blocking session outright. Surfacing blockers
sooner makes that keystroke reachable sooner.

It is not protection. **The guard on that gesture is the gesture** — `x` opens
`modeKillConfirm` (`internal/tui/model.go`), and the whole ordering is measured by
`internal/tui/harm_audit_test.go`. The debounce never prevented a kill; it postponed the
information and let the operator kill the same session a minute later, having learned nothing
in between. Hiding data is not a guard, and this repository already settled that question the
other way in 0.24.0, when `x` was gated with a confirmation rather than by removing rows.

The evidence the debounce encoded implicitly is already rendered on the row: `wait=…` carries
how long the session has been waiting on us. The operator now reads it instead of waiting it
out.

## The change

1. **`blockerGate` is deleted.** Every session blocked by our SPID is sent to the console on
   the poll that observes it. `feedConsole`'s `blockingTimeout` parameter goes with it.
2. **The console feed gets two tickers**, matching the shape `pumpSamples` already has:
   blockers on `blocking_poll_seconds` (10 s), progress and the SPID observation on
   `progress_poll_seconds` (30 s). Moving the whole loop to the faster cadence would triple the
   progress reads for no gain.

A blocker is then visible within one `blocking_poll_seconds` — 10 s against 60–90 s — and no
kill, automatic or manual, happens one second earlier than before.

## The harm ledger is unchanged

No new `ActionKind`, no gesture weakened, no confirmation removed. What changes is the
freshness of what the operator sees, not the price of what they do. `harm_audit_test.go` ranks
actions against gestures and must still pass untouched; if it does not, this design is wrong
rather than the test.

## Migration note

Behaviour changes while no key changes value, which is the case that needs saying out loud.

An operator who raised `blocking_timeout_minutes` to avoid a noisy console was not configuring
the console: that key delayed **both** the display and the reaction, and only the second was
intended. After this change it governs the reaction alone. Anyone who set it for the console's
sake should revisit it — its real cost was a minute of blindness per kill.

## Left undone

- **The suspension tracker's resolution.** `suspensionTracker` accrues blocked time one
  observation per poll, so its totals are accurate to within one interval. Moving the blocker
  half to 10 s improves that incidentally; nothing was designed for it.
- **`progress_poll_seconds` as the panel's name.** The key keeps its current meaning. Renaming
  a public config key to match what it drives is a migration of its own and is not worth
  bundling here.
