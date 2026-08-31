# Functional spec — remote TUI (server / client mode)

> **Status: DRAFT — iteration to design/implement.** Records the need and the intended design.
> Created on 2026-06-17, following the `030_compress_exampledb_indexes.yaml` trial (run launched in
> the background, non-TUI) where we wanted to follow the state live **from another session**.

## 1. Goal

Allow **following a run's live state and acting on it from another process**:

- the instance executing the run opens a **server on a port** and broadcasts its state
  (progress, blocked sessions, waits, status);
- **another execution of the tool in client mode** connects to that port and **displays the
  TUI** (incident console) + the state, and can send **actions** (kill DDL, kill a
  blocker, pause/extend).

Use case: a long maintenance run is executing on a jump host / in the background; a DBA
wants to open the TUI from their machine without relaunching or interrupting the run.

## 2. Why this is tractable: the decoupling already exists

The TUI communicates with the engine **only through messages** — that is already the protocol:

- **State (server → TUI)**: `ProgressMsg`, `BlockersMsg`, `WaitsMsg`, `StatusMsg`, `LogMsg`
  (`internal/tui/model.go:66-84`).
- **Actions (TUI → server)**: a `chan Action` (`internal/tui/model.go:123,129`), routed by
  `dispatchActions` (`cmd/sqlgopace/main.go:438,514-515`).

Today `runWithTUI` (`cmd/sqlgopace/main.go:431-456`) wires all of that **in-process**:

```
feedConsole (poll DB) ──Msg──▶ tui.Program ──Action──▶ dispatchActions (→ SQL server)
   main.go:459-488                  (Bubble Tea)              main.go:514
```

Server/client mode **does not change the TUI**: it replaces that in-process wiring with a
**network transport**. The Bubble Tea model (rendering, keys) is reused **as is** on the
client side. That is what keeps the effort moderate rather than huge.

## 3. Proposed design

### 3.1 Overview

```
            SERVER instance (executes the run)                     CLIENT instance
  ┌───────────────────────────────────────────┐        ┌──────────────────────────┐
  feedConsole/engine ──Msg──▶  broadcast HUB   │  SSE   │  reader ──Msg──▶ tui.Model │
  (state already produced)    │  (last snapshot,        │        │  (Bubble Tea, unchanged) │
  dispatchActions ◀──Action── │   fan-out to N clients) │◀──POST── writer ◀──Action── tui │
  (→ SQL server)              └───────────── :port ──┘        └──────────────────────────┘
```

- **Server**: the executing instance already owns the SQL connection and the SPID, and already
  produces the `Msg` values (via `feedConsole` / the future step-sink from `progress-tui.md`). We add a **hub**
  that (a) keeps the **last snapshot** of each message type, (b) **fans out** to the
  connected clients, (c) receives the clients' `Action`s and pushes them into the existing
  `chan Action`.
- **Client**: a new mode (`--connect host:port`) that opens the **same TUI**, but feeds
  its `tui.Program` from the **network stream** instead of `feedConsole`, and sends `Action`s
  to `POST /action` instead of the local `dispatchActions`.

### 3.2 Recommended transport: HTTP SSE + POST

- **State**: `GET /state` as **Server-Sent Events** (a stream of JSON-encoded `Msg`). Upsides:
  simple, debuggable (`curl`), traverses proxies, and **opens the way to a future web
  dashboard for free** (same stream).
- **Actions**: `POST /action` (JSON body = one `Action`).
- **Snapshot on join**: on SSE connection, the hub first sends back the **last snapshot** of
  each message (otherwise a late client only sees deltas and gets an empty screen).
- Alternatives rejected for v1: WebSocket (bidirectional but heavier); TCP + JSON
  lines (the simplest but not debuggable from a browser/curl). Worth keeping in mind if SSE gets in the way.

### 3.3 Serialization

`Msg`/`Action` are plain structs → direct JSON. Define a small envelope type
`{ "type": "progress|blockers|waits|status|log", "data": {…} }` on the server side; on the client side,
decode into the matching `tea.Msg`. The types live in `internal/tui` (or a new
`internal/tui/wire`) so they remain the single source of the protocol.

### 3.4 Modes & flags

- `sqlgopace -config config.yaml --serve :7070`: executes the run **and** serves the state (local TUI
  optional on top, or no local TUI).
- `sqlgopace --connect host:7070`: **executes no DDL**; only opens the TUI fed by
  the remote stream. Requires neither `config.yaml` nor a SQL connection on the client side.

## 4. Security — the real cost of the feature

A port that accepts actions is a port that can **`KILL` a DDL or a SQL session** remotely.
Structuring decisions:

1. **Bind `127.0.0.1` by default.** Network exposure (`--serve 0.0.0.0:…`) = explicit opt-in,
   with a warning.
2. **Read-only clients by default.** State is broadcast to everyone; **actions** are a
   separate capability, refused unless the client presents a **token** (`--token` / `Authorization`
   header).
3. **TLS** as soon as we go beyond localhost (otherwise token and state travel in clear over the network).
4. **No execution on the client side**: `--connect` mode never touches a database; it only
   displays and relays actions to the server, which remains the only one talking to SQL Server.

This is where most of the effort goes, **not** into the rendering.

## 5. Inherent limits (to accept)

- **Ephemeral server.** The run is *one-shot* (the engine processes the queue then exits): the server
  only lives **for the duration of the operation**. So it has to be **launched with `--serve` from the start** —
  you cannot "attach" a server to a run already launched in bare mode (the same limit as the current
  TUI, cf. `docs/specs/progress-tui.md §5`). This feature **solves** that gap, provided you
  start in server mode.
- **Client reconnection.** A client can (re)connect at any time; it receives the current
  snapshot. If the server stops (end of run), the client must show that cleanly and exit.
- **A single producer.** The server is the executing instance; clients are passive
  (display + relayed actions). No multi-server.

## 6. Links with the other iterations

- **`progress-tui.md`**: the "operation i/N" *step-sink* + timer goes **through the same hub**,
  so remote clients also see the manifest progress. Design the two together:
  the hub is the convergence point for messages (server poll + step events).
- **`crash-resumable.md`**: not directly related, but a remote client makes following a long
  maintenance run (and its possible metadata skips) far more practical.

## 7. Effort estimate

**Moderate** (a few days), dominated by the **transport + the security model**, not by the TUI:

| Lot | Size |
|---|---|
| Broadcast hub (snapshot + fan-out) on the server side | small |
| SSE transport (`GET /state`) + `POST /action` | small-moderate |
| Client mode `--connect` (reuses `tui.Model`, network reader/writer) | small |
| `Msg`/`Action` serialization + envelope type | small |
| Security: localhost bind, read-only, token, (TLS) | **moderate — the bulk** |
| Flags/config, tests, docs | moderate |

The Bubble Tea model and the message protocol **already exist**: that is the main saving.

## 8. Open questions

- **SSE vs WebSocket** for v1? (SSE + POST recommended; WS if we want a single bidi channel.)
- **Scope of remote actions**: do we allow `KILL` remotely, or only
  pause/extend, with KILL reserved for the local TUI? (security)
- **Discovery**: fixed port via flag, or written into a sidecar (`02.processing/…`) so a local
  `--connect` can find it without typing it in?
- **Several runs in parallel** (multi-database, §17): one port per run, or a hub multiplexing
  several runs under a single port?
- **Web reuse**: do we freeze the SSE format so a web dashboard can be plugged in later
  without breaking it?

## 9. Code references (as of 2026-06-17)

| Topic | Location |
|---|---|
| State message types (protocol) | `internal/tui/model.go:66-84` |
| Action channel TUI → server | `internal/tui/model.go:123,129` ; `internal/tui/program.go:25` |
| In-process TUI wiring (to be replaced by the network) | `cmd/sqlgopace/main.go:431-456` |
| State production by server poll | `feedConsole` (`cmd/sqlgopace/main.go:459-488`) |
| Action routing to SQL Server | `dispatchActions` (`cmd/sqlgopace/main.go:514-515`) |
| Limit "no attaching to an already-running run" | `docs/specs/progress-tui.md §5` |
| Step-sink to converge into the hub | `docs/specs/progress-tui.md §3.0` |
