# EMAIL-NOTIFICATIONS — SMTP delivery of run events

> Source of truth for the intended behavior of SqlGoPace's email notifications.
> Design settled 2026-07-27. Adds a second delivery channel alongside the existing
> webhook `Notifier`, sharing the same `on_events` filter.

## 1. Goal and scope

Deliver run-event notifications by **email (SMTP)**, in addition to the existing webhook.
The two events that motivate this are already emitted by the engine:

- `fail` — a manifest stops on a hard error.
- `incomplete` — a shrink stops short of its target with work preserved (no-progress,
  self-wait timeout, log-drain timeout, ...).

Email reuses the **same `notifications.on_events` list** as the webhook; both channels fire
on exactly the same set of events. The email channel is a no-op unless an SMTP host is
configured, so nothing changes for users who do not set it.

### Non-goals

- No independent per-channel event filter (email shares the webhook's `on_events`).
- No per-manifest recipient override (recipients are a single global list).
- No HTML body — plain text only.
- No implicit-TLS (port 465) support in the first cut. `starttls` (opportunistic) covers the
  anonymous-relay and authenticated-587 cases. Implicit TLS is a ~15-line extension to add
  later if an instance's relay requires it.
- No attachments (the `.log` sidecar stays the file-based record).
- No new third-party dependency — standard library `net/smtp` only.

## 2. Configuration

A new `email` sub-block under `notifications`. `on_events` stays at the `notifications`
level and drives **both** the webhook and the email channel.

```yaml
notifications:
  webhook_url: ""
  on_events: [fail, incomplete, interrupted]
  email:
    host: "smtp.internal.example"   # empty → email disabled (no-op)
    port: 25                         # default 25 when absent/zero
    from: "sqlgopace@example.com"
    to: ["dba-team@example.com"]     # one or more recipients
    username: ""                     # empty → no auth (anonymous relay)
    password: "${SMTP_PASS}"         # via .env; used only when username is set
    starttls: false                  # opportunistic STARTTLS before auth
```

Config struct (`internal/config/config.go`):

```go
type NotificationsConfig struct {
    WebhookURL string      `yaml:"webhook_url"`
    OnEvents   []string    `yaml:"on_events"`
    Email      EmailConfig `yaml:"email"`
}

type EmailConfig struct {
    Host     string   `yaml:"host"`
    Port     int      `yaml:"port"`
    From     string   `yaml:"from"`
    To       []string `yaml:"to"`
    Username string   `yaml:"username"`
    Password string   `yaml:"password"`
    StartTLS bool     `yaml:"starttls"`
}
```

`password` is subject to the existing `${VAR}` env injection (never a literal credential in
`config.yaml`; secrets come from the gitignored `.env`).

### Validation (at config load)

Applied only when the channel is in use — an empty `host` disables it entirely:

- `host` non-empty ⇒ `from` required and `to` must have at least one entry.
- `username` non-empty ⇒ `password` required.
- `port` defaults to `25` when zero/absent.

`net/smtp` refuses PLAIN auth over an unencrypted, non-localhost connection. Therefore, when
`username` is set, `starttls` should be `true`; the send path returns the underlying
`net/smtp` error if auth is attempted without encryption rather than silently downgrading.
This is a runtime error surfaced in the run output, not a config-load rejection (the relay's
capabilities are not known at load time).

## 3. Component — `internal/report/email.go`

Mirrors the webhook `Notifier`: same `Notify(ctx, event, payload)` shape, same nil-safe /
disabled no-op behavior, same "log the error, never fail the run" contract.

```go
type EmailNotifier struct {
    cfg    EmailConfig
    events map[string]bool
    send   func(addr string, auth smtp.Auth, from string, to []string, msg []byte) error
    now    func() time.Time
}

func NewEmailNotifier(cfg EmailConfig, events []string) *EmailNotifier
func (n *EmailNotifier) Notify(ctx context.Context, event string, payload map[string]any) error
```

- **No-op** when `n == nil`, `cfg.Host == ""`, or `!n.events[event]`.
- **`send`** is a struct field so tests inject a fake; the production default performs the
  real SMTP conversation. **This is the only injected I/O** — everything else is pure.
- **`now`** is injectable so the `Date:` header is deterministic in tests.

### Message construction (pure)

`buildMessage(event string, payload map[string]any, now time.Time) []byte` is a pure
function returning an RFC 5322 message, unit-tested without a network:

- Headers: `From`, `To` (comma-joined), `Subject`, `Date`, `MIME-Version: 1.0`,
  `Content-Type: text/plain; charset=utf-8`.
- **Subject**: `[SqlGoPace] <EVENT>: <manifest>` — event upper-cased, e.g.
  `[SqlGoPace] FAIL: 020_shrink_proddb_data.yaml`. When `manifest` is absent from the
  payload the subject degrades to `[SqlGoPace] <EVENT>`.
- **Body**: one field per line — `Event`, `Manifest`, `Detail`, `Time` — pulling `manifest`
  and `detail` from the payload map (the same keys the engine already passes:
  `{"manifest": name, "detail": detail}`). Missing keys are omitted rather than printed empty.

### Production send

The default `send` opens the SMTP conversation with a bounded dial timeout (10s, matching the
webhook's `http.Client` timeout):

1. `net.DialTimeout("tcp", addr, 10s)` → `smtp.NewClient`.
2. If `starttls`, issue `STARTTLS` with a `tls.Config{ServerName: host}`.
3. If `username` set, `client.Auth(smtp.PlainAuth("", username, password, host))`.
4. `Mail(from)` → `Rcpt` per recipient → `Data` → write `msg` → `Quit`.

`Notify` honors `ctx` by running the send on a goroutine and selecting on `ctx.Done()` so a
cancelled run does not block on a stalled relay; on `ctx` cancellation it returns `ctx.Err()`
(logged, non-fatal). The 10s dial timeout is the backstop when there is no deadline on `ctx`.

## 4. Wiring — fan-out to multiple notifiers

Today the engine holds a single `notifier *report.Notifier`. Generalize to a slice behind a
small interface so both channels receive the same events.

```go
// internal/run — satisfied by *report.Notifier AND *report.EmailNotifier
type Notifier interface {
    Notify(ctx context.Context, event string, payload map[string]any) error
}
```

- `Engine.notifiers []Notifier`.
- `WithNotifier(n Notifier) EngineOption` **appends** (called once per channel).
- `Engine.notify(...)` loops over `notifiers`, logging each channel's error independently to
  `e.out` (`notify <channel> <manifest>: <err>`) and continuing — one channel failing never
  suppresses the other, and never fails the run.

`cmd/sqlgopace/main.go` builds both channels from the shared `on_events`:

```go
opts = append(opts,
    run.WithNotifier(report.NewNotifier(cfg.Notifications.WebhookURL, cfg.Notifications.OnEvents)),
    run.WithNotifier(report.NewEmailNotifier(cfg.Notifications.Email, cfg.Notifications.OnEvents)))
```

Both are non-nil; each no-ops internally when unconfigured (empty webhook URL / empty SMTP
host), so wiring both unconditionally is safe.

## 5. Error handling

- A send failure (dial, auth, relay rejection, `ctx` cancellation) is **logged and swallowed**
  — never propagated as a run failure. This matches the existing webhook contract.
- Config validation (section 2) catches obvious misconfiguration at load; relay-capability
  errors (e.g. auth over cleartext refused) surface at send time in the run output.

## 6. Testing (`make test`, no DB, no network)

- **`buildMessage`** (pure): subject format (with and without `manifest`), header presence,
  body lines, missing-key omission, deterministic `Date` via injected `now`.
- **`EmailNotifier.Notify`** with an injected fake `send`: correct `addr` (`host:port`),
  `from`, `to`, and a `msg` containing the subject and the `detail`; no-op when the event is
  disabled, when `host` is empty, and when the receiver is nil.
- **Engine fan-out**: two fake notifiers both receive an enabled event; one returning an error
  does not stop the other and does not fail the run.

## 7. Documentation & version

- `README.md` notifications section documents the `notifications.email` keys.
- `config.yaml` gains a commented `email:` block under `notifications:`.
- Bump `internal/version/VERSION` (minor).

## 8. Open question (does not block implementation)

The concrete SMTP infrastructure at the deployment site is not yet confirmed. The design
covers the three realistic cases via config (anonymous relay on 25, authenticated 587 with
STARTTLS); only implicit TLS on 465 is deferred (section 1, Non-goals) and is an additive
change if needed.
