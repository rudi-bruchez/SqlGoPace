# Blocker roster with pre-armed auto-kill — design

Date: 2026-07-22
Status: approved design, pending implementation plan

## Problem

When a long DDL/shrink runs, other sessions periodically block it — the head
blockers that stall progress (e.g. an idle-in-transaction SPID that starved a
shrink for days). Today the console shows these only as a read-only summary line
("suspended N×, … by SPID …"); there is no way to see the full roster of who has
blocked us this run, and no way to say "if this login/host blocks me again, kill
it." The only kill affordances (`x` / `X`) act on the *other* population — the
victims our DDL blocks — and only on a session that is live right now.

## Goal

A key that opens a roster of **every session that has blocked our DDL this run**,
grouped by **login** or **host**, from which the operator can arm (and un-arm)
auto-kill rules keyed on that login/host. Arming appends a `kill_blocked_sessions`
rule to the running manifest; the engine hot-reloads it and the already-armed
`BlockerKiller` terminates any present-or-future session from that group on the
next blocking poll. Nothing is killed at arm time — recurrences are handled by
the killer.

## Decisions (settled)

- **Scope:** in-memory, this run only. No cross-run persistence, no new storage.
- **Population:** sessions that blocked *us* (the suspension-history / self-block
  population), not the victims our DDL blocks. This is coherent end to end: the
  config-armed `BlockerKiller` already kills the session blocking us, so a
  login/host rule armed here matches what actually gets enforced.
- **Grouping:** one roster, a key toggles the grouping between `login_name` and
  `host_name`. A row is a distinct login (or host) with aggregate episode count
  and total blocked time. Selecting a row arms a rule keyed on that attribute.
- **Arm timing:** arming only appends the rule. If `kill_blockers` is enabled in
  config, the armed killer kills present-or-future matches on the next poll; if
  disabled, the rule is inert and the roster warns.
- **Un-arm:** included in v1. `space` is a true on/off toggle; un-arming removes
  the matching kill rule from the manifest.
- **Open key:** `b` (blockers). Closes with `b` / `esc` / `q`.

## Data source — reuse the suspension history

The roster is a grouped, actionable rendering of the existing
`m.suspension.Blockers`, already tracked per blocking SPID (login, episode count,
total blocked time). No new accumulator. Two capture gaps to fill so host
grouping works:

- `mssql.SelfBlock` gains a `Host string` field, populated from the blocking
  session's `s.Host` in `FindSelfBlock` (alongside the existing `Login`/`Program`).
- `run` suspension tracking (in `cmd/sqlgopace/main.go`): `blockerAgg` records
  `host`; `suspensionTracker.observe` accepts the blocker's host; `snapshot`
  emits it. `feedConsole` passes `sb.Host` into `observe`.
- `tui.SuspensionBlocker` (the message struct) gains `Host string`.

Blockers whose login/host is unknown (the blocking session was not in the
snapshot) aggregate into a single non-armable "(unknown)" row — shown for
completeness, but `space` is a no-op on it (no criterion value to match).

## TUI model & key handling

New `Model` fields:

- `rosterOpen bool` — the roster modal is showing.
- `rosterCursor int` — selection within the grouped rows.
- `rosterByHost bool` — grouping key: false = login, true = host.
- `armed map[string]bool` — set of armed keys (`"login_name=APP01"`), the local
  echo that drives the `✓` marker and the toggle direction. Roster-scoped: it
  reflects roster actions, not kill rules that pre-existed in config/`X`.
- `killerArmed bool` — whether `kill_blockers` is enabled in config, for the
  warning. Set on construction from `main.go` (New gains the flag, or a setter).

Key routing mirrors `inCriterionMode`: a new `inRoster()` predicate makes
`handleKey` delegate to `handleRosterKey` while `rosterOpen`.

- `b` (normal mode) → open the roster. `b` / `esc` / `q` (roster mode) → close.
  **`q` closes the roster; it does not quit the app while the roster is open.**
- `↑` / `↓` → move `rosterCursor` within the current grouping's rows.
- `g` → toggle `rosterByHost`; the cursor resets to 0 (row set changes).
- `space` / `enter` → toggle-arm the selected group. On a group with key
  `k = "<criterion>=<value>"`: if `armed[k]` → emit `ActionDisarmKillRule` and
  `delete(armed, k)`; else → emit `ActionArmKillRule` and `armed[k] = true`.
  No-op on the "(unknown)" row.

Grouping (computed in the view / a helper): fold `m.suspension.Blockers` by the
active key, summing `Count` and `TotalMS`, preserving first-seen order for stable
rendering.

## Action & dispatch

Two new `ActionKind`s carrying `Criterion` (`login_name` | `host_name`) + `Value`
(SPID unused — a recurrence has a new SPID):

- `ActionArmKillRule` → `armKillRule`: `ddl.KilledSessionFor(criterion, value, 0)`
  → `ddl.AppendKilledSession(path)` → echo a `LogMsg`. Exactly `killBlockerAuto`
  **minus** the `conn.Kill`. `AppendKilledSession` de-dups, so re-arming is a
  harmless no-op.
- `ActionDisarmKillRule` → `disarmKillRule`: `ddl.KilledSessionFor(...)` →
  `ddl.RemoveKilledSession(path, rule)` → echo a `LogMsg`.

New manifest edit `ddl.RemoveKilledSession(path string, s KilledSession) error`:
load the manifest, drop every field-equal (`sameKilledSession`) entry from
`KillBlockedSessions`, write back atomically (temp + rename), mirroring
`AppendKilledSession`. Removing a rule that isn't present is a no-op. (Note: if a
config-supplied rule happened to match the same login/host, disarm removes it
too — that is the operator's explicit intent when toggling the group off.)

## Config-disabled warning

When `killerArmed` is false, the roster footer shows:
"kill_blockers disabled in config — armed rules won't fire until it's enabled."
Arming still writes the rule (inert until the feature is enabled later).
`killerArmed` is derived from `cfg.KillBlockers.Enabled` where the model/program
is constructed in `main.go`.

## View

While `rosterOpen`, `View()` returns a full alt-screen modal instead of the
dashboard (simplest given the fixed alt-screen height):

- Title: `blockers that stalled this run — grouped by login|host`.
- One row per group: `<value>   N×   <total-blocked>   [✓ armed]`, selected row
  highlighted; the "(unknown)" row last.
- Empty state: `(no blockers yet)`.
- Footer: `[↑/↓] select  [space] arm/disarm  [g] group by host|login  [b/esc/q] close`
  plus the config warning when `killerArmed` is false.

The normal-mode footer gains `[b] blockers` in its shortcut line.

## Out of scope (v1 / YAGNI)

- Cross-run persistence (decided: in-memory).
- Immediate kill of live matches at arm time (decided: the killer handles it).
- Reconciling `armed` against kill rules that pre-existed in config or were added
  via `X` (the `✓` reflects roster actions only).

## Tests

Pure/unit, no DB:

- `mssql.FindSelfBlock` populates `Host` from the blocking session.
- `suspensionTracker` captures host per blocker; `snapshot` emits it in
  `SuspensionBlocker.Host`.
- Model: `b` opens / `q` closes without quitting; `g` toggles grouping and resets
  the cursor; grouping aggregates count and total by login and by host; `space`
  on an unarmed group emits `ActionArmKillRule` with the right criterion/value and
  sets `✓`; `space` again emits `ActionDisarmKillRule` and clears `✓`; `space` on
  the "(unknown)" row is a no-op.
- `armKillRule` appends a kill rule and issues **no** `conn.Kill`; `disarmKillRule`
  removes it.
- `ddl.KilledSessionFor("host_name", v, 0)` yields a host rule;
  `ddl.RemoveKilledSession` drops a field-equal rule and is a no-op when absent.
