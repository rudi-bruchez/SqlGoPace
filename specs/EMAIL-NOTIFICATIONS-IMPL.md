# Email Notifications Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add native SMTP email as a second notification channel alongside the existing webhook, driven by the same `notifications.on_events` filter.

**Architecture:** A new `report.EmailNotifier` mirrors the webhook `report.Notifier` (same `Notify(ctx, event, payload)` shape, same nil-safe / disabled no-op, same "log and swallow" error contract). All SMTP I/O sits behind one injected `send` function so message construction and event filtering are pure and unit-tested without a network. The engine fans out to a slice of notifiers behind a new `run.Notifier` interface satisfied by both concrete types.

**Tech Stack:** Go standard library only — `net/smtp`, `crypto/tls`, `net`. No new dependency.

**Design spec:** `specs/EMAIL-NOTIFICATIONS.md` (source of truth).

**Branch:** `feat/email-notifications` (already created; the spec is committed there).

## Global Constraints

- Go 1.26.4; standard library only — **no new module dependency** (`net/smtp` covers it).
- English only — all code, comments, identifiers, file names, committed docs.
- Idiomatic Go, KISS — no layers/interfaces/options the current need doesn't justify.
- US spelling in comments/identifiers.
- `internal/report` must **not** import `internal/config` (report stays a low-level, config-free package, like the webhook `Notifier`). `cmd` maps `config.EmailConfig` → `report.EmailConfig` field by field, exactly as it maps `config.ShrinkConfig` → `run.ShrinkTuning`.
- A send failure must **never** fail the run — log to the engine's output writer and continue.
- Verify with `make test` (unit, `-race`, no DB), `make vet`, `make lint` (golangci-lint v2).
- `${VAR}` secrets come from the gitignored `.env`; env expansion already runs on the whole YAML (`os.ExpandEnv`, `internal/config/config.go:274`), so `password: "${SMTP_PASS}"` needs no special handling.
- Windows: stop any running `bin/sqlgopace.exe` before `make build` (the binary is locked while running).
- Commit after each task (frequent commits).

---

### Task 1: Config — `EmailConfig` struct, defaults, validation

**Files:**
- Modify: `internal/config/config.go` (add `EmailConfig`, extend `NotificationsConfig`, port default in `applyDefaults`, validation in `validate`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.EmailConfig{Host string; Port int; From string; To []string; Username string; Password string; StartTLS bool}`, exposed as `NotificationsConfig.Email EmailConfig` (yaml `email`). Port defaults to 25 when the channel is enabled.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestEmailConfigParsesAndDefaultsPort(t *testing.T) {
	t.Setenv("SMTP_PASS", "s3cret")
	yaml := validYAML + "notifications:\n" +
		"  on_events: [fail, incomplete]\n" +
		"  email:\n" +
		"    host: smtp.internal\n" +
		"    from: sqlgopace@example.com\n" +
		"    to: [dba@example.com]\n" +
		"    password: \"${SMTP_PASS}\"\n"
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	e := cfg.Notifications.Email
	if e.Host != "smtp.internal" || e.Port != 25 || e.From != "sqlgopace@example.com" ||
		len(e.To) != 1 || e.To[0] != "dba@example.com" || e.Password != "s3cret" {
		t.Errorf("email config = %+v, want host/port(25 default)/from/to/expanded password", e)
	}
}

func TestEmailConfigRequiresFromAndTo(t *testing.T) {
	yaml := validYAML + "notifications:\n  email:\n    host: smtp.internal\n"
	if _, err := config.Parse([]byte(yaml)); err == nil {
		t.Fatal("Parse() want error when email.host set without from/to")
	}
}

func TestEmailConfigRequiresPasswordWhenUsername(t *testing.T) {
	yaml := validYAML + "notifications:\n  email:\n" +
		"    host: smtp.internal\n    from: a@x\n    to: [b@y]\n    username: u\n"
	if _, err := config.Parse([]byte(yaml)); err == nil {
		t.Fatal("Parse() want error when username set without password")
	}
}
```

Confirm the constructor used in these tests matches the package's real entry point — if `config.Parse` is named differently (e.g. `config.Load`/`ParseBytes`), read `config_test.go`'s existing helpers (`validYAML`, the parse call) and match them. Reuse the existing `validYAML` fixture.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config -run TestEmailConfig -v`
Expected: FAIL — `Email` field does not exist / no validation.

- [ ] **Step 3: Add the struct and wire defaults + validation**

In `internal/config/config.go`, extend `NotificationsConfig`:

```go
// NotificationsConfig holds webhook and email settings. on_events is shared:
// it filters both channels.
type NotificationsConfig struct {
	WebhookURL string      `yaml:"webhook_url"`
	OnEvents   []string    `yaml:"on_events"`
	Email      EmailConfig `yaml:"email"`
}

// EmailConfig holds SMTP delivery settings. An empty Host disables the email
// channel (no-op). Port defaults to 25. Username/Password are optional (empty =
// anonymous relay); Password is injected from ${VAR} like every other secret.
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

In `(*Config).applyDefaults()` (after the shrink/batch defaults, around line 322), add:

```go
	if c.Notifications.Email.Host != "" && c.Notifications.Email.Port <= 0 {
		c.Notifications.Email.Port = 25
	}
```

In `(*Config).validate()` (before the final `return nil`, around line 397), add:

```go
	if e := c.Notifications.Email; e.Host != "" {
		if strings.TrimSpace(e.From) == "" || len(e.To) == 0 {
			return fmt.Errorf("notifications.email.from and at least one to are required when host is set: %w", ErrInvalidConfig)
		}
		if e.Username != "" && e.Password == "" {
			return fmt.Errorf("notifications.email.password is required when username is set: %w", ErrInvalidConfig)
		}
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config -run TestEmailConfig -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add notifications.email (SMTP) config, defaults, validation"
```

---

### Task 2: `report.EmailConfig` + pure `buildMessage`

**Files:**
- Create: `internal/report/email.go`
- Create: `internal/report/email_internal_test.go` (package `report` — exercises the unexported `buildMessage`)

**Interfaces:**
- Produces: `report.EmailConfig{Host string; Port int; From string; To []string; Username string; Password string; StartTLS bool}` (report-local carrier, distinct from `config.EmailConfig`); `buildMessage(cfg EmailConfig, event string, payload map[string]any, now time.Time) []byte`.

- [ ] **Step 1: Write the failing test**

Create `internal/report/email_internal_test.go`:

```go
package report

import (
	"strings"
	"testing"
	"time"
)

func TestBuildMessage(t *testing.T) {
	cfg := EmailConfig{From: "sqlgopace@example.com", To: []string{"a@x", "b@y"}}
	now := time.Date(2026, 7, 27, 15, 4, 5, 0, time.UTC)
	msg := string(buildMessage(cfg, "fail", map[string]any{
		"manifest": "020_shrink.yaml", "detail": "boom",
	}, now))

	for _, want := range []string{
		"From: sqlgopace@example.com\r\n",
		"To: a@x, b@y\r\n",
		"Subject: [SqlGoPace] FAIL: 020_shrink.yaml\r\n",
		"MIME-Version: 1.0\r\n",
		"Event: fail\r\n",
		"Manifest: 020_shrink.yaml\r\n",
		"Detail: boom\r\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q in:\n%s", want, msg)
		}
	}
}

func TestBuildMessageSubjectWithoutManifest(t *testing.T) {
	msg := string(buildMessage(EmailConfig{From: "a@x", To: []string{"b@y"}}, "interrupted",
		map[string]any{}, time.Unix(0, 0).UTC()))
	if !strings.Contains(msg, "Subject: [SqlGoPace] INTERRUPTED\r\n") {
		t.Errorf("subject should degrade without manifest:\n%s", msg)
	}
	if strings.Contains(msg, "Manifest:") || strings.Contains(msg, "Detail:") {
		t.Errorf("absent payload keys should be omitted:\n%s", msg)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/report -run TestBuildMessage -v`
Expected: FAIL — `buildMessage` / `EmailConfig` undefined.

- [ ] **Step 3: Write the struct and pure builder**

Create `internal/report/email.go`:

```go
package report

import (
	"fmt"
	"strings"
	"time"
)

// EmailConfig holds SMTP delivery settings for EmailNotifier. It is report-local
// (mirrors config.EmailConfig) so this package stays free of internal/config, the
// same way run.ShrinkTuning mirrors config.ShrinkConfig. cmd maps between them.
type EmailConfig struct {
	Host     string
	Port     int
	From     string
	To       []string
	Username string
	Password string
	StartTLS bool
}

// buildMessage renders an RFC 5322 plain-text message for one run event. Pure:
// no I/O, deterministic given now. manifest/detail come from the same payload
// keys the engine already passes ({"manifest": ..., "detail": ...}); absent keys
// are omitted rather than printed empty.
func buildMessage(cfg EmailConfig, event string, payload map[string]any, now time.Time) []byte {
	manifest, _ := payload["manifest"].(string)
	detail, _ := payload["detail"].(string)

	subject := "[SqlGoPace] " + strings.ToUpper(event)
	if manifest != "" {
		subject += ": " + manifest
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(cfg.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n") // header/body separator

	fmt.Fprintf(&b, "Event: %s\r\n", event)
	if manifest != "" {
		fmt.Fprintf(&b, "Manifest: %s\r\n", manifest)
	}
	if detail != "" {
		fmt.Fprintf(&b, "Detail: %s\r\n", detail)
	}
	fmt.Fprintf(&b, "Time: %s\r\n", now.Format(time.RFC3339))
	return []byte(b.String())
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/report -run TestBuildMessage -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**

```bash
git add internal/report/email.go internal/report/email_internal_test.go
git commit -m "feat(report): add report.EmailConfig and pure buildMessage"
```

---

### Task 3: `EmailNotifier` + `Notify` (event filtering, injected send)

**Files:**
- Modify: `internal/report/email.go` (add `EmailNotifier`, `NewEmailNotifier`, `Notify`)
- Modify: `internal/report/email_internal_test.go` (add filtering/recipient tests)

**Interfaces:**
- Consumes: `report.EmailConfig`, `buildMessage` (Task 2).
- Produces: `NewEmailNotifier(cfg EmailConfig, events []string) *EmailNotifier`; method `(*EmailNotifier).Notify(ctx context.Context, event string, payload map[string]any) error`; unexported field `send func(ctx context.Context, cfg EmailConfig, msg []byte) error` (default set in Task 4; tests override it directly).

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/email_internal_test.go`:

```go
import "context"  // add to the existing import block

func TestEmailNotifierFiltersEvents(t *testing.T) {
	var calls int
	n := NewEmailNotifier(EmailConfig{Host: "smtp.internal", From: "a@x", To: []string{"b@y"}},
		[]string{"fail"})
	n.send = func(context.Context, EmailConfig, []byte) error { calls++; return nil }

	if err := n.Notify(context.Background(), "incomplete", nil); err != nil || calls != 0 {
		t.Fatalf("disabled event: err=%v calls=%d, want nil/0", err, calls)
	}
	if err := n.Notify(context.Background(), "fail", map[string]any{"manifest": "m.yaml"}); err != nil || calls != 1 {
		t.Fatalf("enabled event: err=%v calls=%d, want nil/1", err, calls)
	}
}

func TestEmailNotifierNoOpWithoutHost(t *testing.T) {
	var calls int
	n := NewEmailNotifier(EmailConfig{From: "a@x", To: []string{"b@y"}}, []string{"fail"}) // Host ""
	n.send = func(context.Context, EmailConfig, []byte) error { calls++; return nil }
	if err := n.Notify(context.Background(), "fail", nil); err != nil || calls != 0 {
		t.Fatalf("empty host: err=%v calls=%d, want nil/0 (no-op)", err, calls)
	}
	var nilN *EmailNotifier
	if err := nilN.Notify(context.Background(), "fail", nil); err != nil {
		t.Fatalf("nil receiver Notify = %v, want nil", err)
	}
}

func TestEmailNotifierPassesRecipientsAndBody(t *testing.T) {
	var gotCfg EmailConfig
	var gotMsg string
	n := NewEmailNotifier(EmailConfig{Host: "smtp.internal", Port: 25, From: "a@x", To: []string{"b@y", "c@z"}},
		[]string{"fail"})
	n.send = func(_ context.Context, cfg EmailConfig, msg []byte) error {
		gotCfg, gotMsg = cfg, string(msg)
		return nil
	}
	if err := n.Notify(context.Background(), "fail", map[string]any{"manifest": "m.yaml", "detail": "d"}); err != nil {
		t.Fatalf("Notify = %v", err)
	}
	if len(gotCfg.To) != 2 {
		t.Errorf("recipients = %v, want 2", gotCfg.To)
	}
	if !strings.Contains(gotMsg, "Subject: [SqlGoPace] FAIL: m.yaml") || !strings.Contains(gotMsg, "Detail: d") {
		t.Errorf("message missing subject/detail:\n%s", gotMsg)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/report -run TestEmailNotifier -v`
Expected: FAIL — `NewEmailNotifier` / `EmailNotifier` undefined.

- [ ] **Step 3: Add the notifier**

Append to `internal/report/email.go` (and add `"context"` to the import block):

```go
// EmailNotifier sends run-event emails over SMTP for enabled events. It mirrors
// Notifier: a nil-safe no-op when the receiver is nil, the host is empty, or the
// event is not enabled. All SMTP I/O is behind send so the filtering and message
// construction stay pure and testable; the production send is set by
// NewEmailNotifier (see smtpSend).
type EmailNotifier struct {
	cfg    EmailConfig
	events map[string]bool
	send   func(ctx context.Context, cfg EmailConfig, msg []byte) error
	now    func() time.Time
}

// NewEmailNotifier builds an EmailNotifier for the given SMTP settings and enabled
// events. It is a no-op at Notify time when cfg.Host is empty.
func NewEmailNotifier(cfg EmailConfig, events []string) *EmailNotifier {
	set := make(map[string]bool, len(events))
	for _, e := range events {
		set[e] = true
	}
	return &EmailNotifier{cfg: cfg, events: set, send: smtpSend, now: time.Now}
}

// Notify emails the event to the configured recipients when it is enabled. A
// disabled event, an empty host, or a nil receiver is a no-op. A send failure is
// returned (the engine logs it and continues — a notification never fails a run).
func (n *EmailNotifier) Notify(ctx context.Context, event string, payload map[string]any) error {
	if n == nil || n.cfg.Host == "" || !n.events[event] {
		return nil
	}
	return n.send(ctx, n.cfg, buildMessage(n.cfg, event, payload, n.now()))
}
```

Note: `smtpSend` is defined in Task 4. Until then the package will not compile — that is expected; Task 3 and Task 4 land together conceptually. If implementing strictly one task at a time, temporarily set `send: nil` here and a guard, or implement Task 4's `smtpSend` stub first. Recommended: do Task 4's `smtpSend` immediately after Step 3 so the package compiles, then run both tasks' tests.

- [ ] **Step 4: Run tests to verify they pass**

(After Task 4's `smtpSend` exists so the package compiles.)
Run: `go test ./internal/report -run TestEmailNotifier -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit** (may be combined with Task 4's commit)

```bash
git add internal/report/email.go internal/report/email_internal_test.go
git commit -m "feat(report): add EmailNotifier with event filtering and injected send"
```

---

### Task 4: Production `smtpSend` (net/smtp) + loopback test

**Files:**
- Modify: `internal/report/email.go` (add `smtpSend`)
- Modify: `internal/report/email_internal_test.go` (add loopback SMTP stub + test)

**Interfaces:**
- Produces: `smtpSend(ctx context.Context, cfg EmailConfig, msg []byte) error` — the default `send` wired by `NewEmailNotifier`.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/email_internal_test.go` (add imports `bufio`, `net`, `strconv`):

```go
type capturedMail struct {
	from  string
	rcpts []string
	body  string
}

// fakeSMTPServer is a minimal loopback SMTP stub: enough of the protocol for
// net/smtp's client to send one plaintext message (no STARTTLS, no auth).
func fakeSMTPServer(t *testing.T) (addr string, got *capturedMail) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got = &capturedMail{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 localhost ready\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				fmt.Fprint(conn, "250 localhost\r\n")
			case strings.HasPrefix(cmd, "MAIL FROM"):
				got.from = cmd
				fmt.Fprint(conn, "250 OK\r\n")
			case strings.HasPrefix(cmd, "RCPT TO"):
				got.rcpts = append(got.rcpts, cmd)
				fmt.Fprint(conn, "250 OK\r\n")
			case cmd == "DATA":
				fmt.Fprint(conn, "354 end with .\r\n")
				var body strings.Builder
				for {
					dl, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimRight(dl, "\r\n") == "." {
						break
					}
					body.WriteString(dl)
				}
				got.body = body.String()
				fmt.Fprint(conn, "250 OK\r\n")
			case cmd == "QUIT":
				fmt.Fprint(conn, "221 bye\r\n")
				return
			default:
				fmt.Fprint(conn, "250 OK\r\n")
			}
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	return ln.Addr().String(), got
}

func TestSMTPSendPlaintext(t *testing.T) {
	addr, got := fakeSMTPServer(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	cfg := EmailConfig{Host: host, Port: port, From: "a@x", To: []string{"b@y", "c@z"}}

	if err := smtpSend(context.Background(), cfg, []byte("Subject: t\r\n\r\nhello world\r\n")); err != nil {
		t.Fatalf("smtpSend: %v", err)
	}
	if !strings.Contains(got.from, "a@x") {
		t.Errorf("MAIL FROM = %q, want a@x", got.from)
	}
	if len(got.rcpts) != 2 {
		t.Errorf("RCPT count = %d, want 2 (%v)", len(got.rcpts), got.rcpts)
	}
	if !strings.Contains(got.body, "hello world") {
		t.Errorf("body = %q, want to contain 'hello world'", got.body)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/report -run TestSMTPSendPlaintext -v`
Expected: FAIL — `smtpSend` undefined.

- [ ] **Step 3: Implement `smtpSend`**

Append to `internal/report/email.go` (add imports `crypto/tls`, `net`, `net/smtp`, `strconv`):

```go
// smtpSend performs the SMTP conversation for one message: dial (bounded by a
// 10s timeout and honoring ctx during connect), optional STARTTLS, optional PLAIN
// auth, then MAIL/RCPT/DATA/QUIT. It is the production default for EmailNotifier.send.
func smtpSend(ctx context.Context, cfg EmailConfig, msg []byte) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()

	if cfg.StartTLS {
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, to := range cfg.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return c.Quit()
}
```

- [ ] **Step 4: Run the report package tests**

Run: `go test ./internal/report -v`
Expected: PASS — `TestSMTPSendPlaintext`, all `TestEmailNotifier*`, `TestBuildMessage*`, and the existing `TestNotifier*`.

Note: `smtpSend` covers the plaintext/no-auth path under test (the confirmed anonymous-relay case). The STARTTLS and PLAIN-auth branches are simple conditional glue, exercised manually against a real relay (stubbing TLS + auth in-process is disproportionate). State this limitation in the commit body.

- [ ] **Step 5: Commit**

```bash
git add internal/report/email.go internal/report/email_internal_test.go
git commit -m "feat(report): implement smtpSend (net/smtp) with loopback plaintext test

STARTTLS/auth branches are conditional glue verified against a real relay,
not the in-process stub (plaintext/no-auth path is unit-tested)."
```

---

### Task 5: Engine fan-out — `run.Notifier` interface + notifier slice

**Files:**
- Modify: `internal/run/engine.go` (add `Notifier` interface, change `notifier` field to `notifiers []Notifier`, `WithNotifier` appends, `notify` loops)
- Create: `internal/run/engine_notify_internal_test.go` (package `run`)

**Interfaces:**
- Consumes: nothing new (both `*report.Notifier` and `*report.EmailNotifier` already satisfy the interface structurally).
- Produces: `run.Notifier` interface `{ Notify(ctx context.Context, event string, payload map[string]any) error }`; `WithNotifier(n Notifier) EngineOption` now **appends**.

- [ ] **Step 1: Write the failing test**

Create `internal/run/engine_notify_internal_test.go`:

```go
package run

import (
	"context"
	"errors"
	"io"
	"testing"
)

type notifierFunc func(context.Context, string, map[string]any) error

func (f notifierFunc) Notify(ctx context.Context, ev string, p map[string]any) error {
	return f(ctx, ev, p)
}

func TestEngineFansOutToAllNotifiers(t *testing.T) {
	var a, b []string
	fa := notifierFunc(func(_ context.Context, ev string, _ map[string]any) error { a = append(a, ev); return nil })
	fb := notifierFunc(func(_ context.Context, ev string, _ map[string]any) error { b = append(b, ev); return errors.New("boom") })

	e := &Engine{out: io.Discard}
	WithNotifier(fa)(e)
	WithNotifier(fb)(e)

	e.notify(context.Background(), "fail", "m.yaml", "detail") // must not panic despite fb's error

	if len(a) != 1 || a[0] != "fail" || len(b) != 1 || b[0] != "fail" {
		t.Errorf("fan-out: a=%v b=%v, want both [fail]", a, b)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/run -run TestEngineFansOut -v`
Expected: FAIL — `WithNotifier` takes `*report.Notifier`, not the interface / field is not a slice.

- [ ] **Step 3: Make the changes**

In `internal/run/engine.go`:

Change the field (line 161) from `notifier *report.Notifier` to:

```go
	notifiers        []Notifier
```

Add the interface near `EngineOption` (around line 191):

```go
// Notifier delivers a run event to an external channel. Both *report.Notifier
// (webhook) and *report.EmailNotifier (email) satisfy it; the engine fans out to
// every wired notifier.
type Notifier interface {
	Notify(ctx context.Context, event string, payload map[string]any) error
}
```

Change `WithNotifier` (lines 222-223) to append:

```go
// WithNotifier adds a notification channel (webhook or email). May be called once
// per channel; all wired notifiers receive every enabled event.
func WithNotifier(n Notifier) EngineOption { return func(e *Engine) { e.notifiers = append(e.notifiers, n) } }
```

Replace the `notify` body (lines 1202-1209) with:

```go
func (e *Engine) notify(ctx context.Context, event, name, detail string) {
	payload := map[string]any{"manifest": name, "detail": detail}
	for _, n := range e.notifiers {
		if err := n.Notify(ctx, event, payload); err != nil {
			fmt.Fprintf(e.out, "notify %s: %v\n", name, err)
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/run -run TestEngineFansOut -v`
Then the whole package (the `WithNotifier` signature change is source-compatible — `*report.Notifier` still satisfies `Notifier` — but confirm no caller broke):
Run: `go build ./... && go test ./internal/run`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/run/engine.go internal/run/engine_notify_internal_test.go
git commit -m "refactor(run): fan out notifications to a slice of run.Notifier"
```

---

### Task 6: Wire the email channel in `cmd/sqlgopace/main.go`

**Files:**
- Modify: `cmd/sqlgopace/main.go:466` (build both notifiers from the shared `on_events`)

**Interfaces:**
- Consumes: `report.NewEmailNotifier`, `report.EmailConfig` (Tasks 2-4), `run.WithNotifier` (Task 5), `config.EmailConfig` (Task 1).

- [ ] **Step 1: Make the change**

Replace the single-notifier line at `cmd/sqlgopace/main.go:466`:

```go
		opts = append(opts, run.WithNotifier(report.NewNotifier(cfg.Notifications.WebhookURL, cfg.Notifications.OnEvents)))
```

with both channels (webhook + email), mapping `config.EmailConfig` → `report.EmailConfig` field by field:

```go
		opts = append(opts,
			run.WithNotifier(report.NewNotifier(cfg.Notifications.WebhookURL, cfg.Notifications.OnEvents)),
			run.WithNotifier(report.NewEmailNotifier(report.EmailConfig{
				Host:     cfg.Notifications.Email.Host,
				Port:     cfg.Notifications.Email.Port,
				From:     cfg.Notifications.Email.From,
				To:       cfg.Notifications.Email.To,
				Username: cfg.Notifications.Email.Username,
				Password: cfg.Notifications.Email.Password,
				StartTLS: cfg.Notifications.Email.StartTLS,
			}, cfg.Notifications.OnEvents)))
```

Both are unconditional: each no-ops internally when unconfigured (empty webhook URL / empty SMTP host), so an unset `email:` block changes nothing.

- [ ] **Step 2: Build and run the full suite**

Run: `go build ./... && go test -race ./... && go vet ./...`
Expected: build OK, all tests PASS, vet clean.

- [ ] **Step 3: Commit**

```bash
git add cmd/sqlgopace/main.go
git commit -m "feat(cmd): wire the SMTP email notifier alongside the webhook"
```

---

### Task 7: Docs, config example, version bump

**Files:**
- Modify: `README.md` (notifications section)
- Modify: `config.yaml` (commented `email:` block under `notifications:`)
- Modify: `internal/version/VERSION`

**Interfaces:** none (documentation and metadata).

- [ ] **Step 1: Document the config in `README.md`**

Find the notifications entry in the config reference table and the surrounding prose (search for `webhook_url` / `on_events`), and add the `email` sub-block. Example prose to adapt to the existing style:

```markdown
Email notifications (optional) share the same `on_events` filter as the webhook.
Leave `notifications.email.host` empty to disable them.

```yaml
notifications:
  webhook_url: ""
  on_events: [fail, incomplete, interrupted]
  email:
    host: "smtp.internal.example"   # empty → email disabled
    port: 25                         # default 25
    from: "sqlgopace@example.com"
    to: ["dba-team@example.com"]
    username: ""                     # empty → anonymous relay (no auth)
    password: "${SMTP_PASS}"         # from .env; only used when username is set
    starttls: false                  # opportunistic STARTTLS before auth
```
```

Also list the events: `fail` (hard error), `incomplete` (shrink stopped short, work preserved), `interrupted` (Ctrl+C/drain), plus the reaction events (`pause`/`resume`/`kill`/`abort`).

- [ ] **Step 2: Add the commented block to `config.yaml`**

Under the existing `notifications:` block, add:

```yaml
  # Optional email channel. Shares on_events with the webhook. Empty host = disabled.
  # email:
  #   host: "smtp.internal.example"
  #   port: 25
  #   from: "sqlgopace@example.com"
  #   to: ["dba-team@example.com"]
  #   username: ""            # empty = anonymous relay
  #   password: "${SMTP_PASS}"
  #   starttls: false
```

If `config.yaml` has no `notifications:` block yet, add one with `webhook_url: ""`, `on_events: [fail, incomplete, interrupted]`, and the commented `email:` block.

- [ ] **Step 3: Bump the version**

Set `internal/version/VERSION` to the next minor. The working tree already holds `0.5.0` (the uncommitted avg-per-chunk change); if that ships in the same release, keep `0.5.0`, otherwise set `0.6.0`. Decide based on how the working tree is committed.

- [ ] **Step 4: Verify the build carries the version**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add README.md config.yaml internal/version/VERSION
git commit -m "docs: document email notifications; bump version"
```

---

## Self-Review

**Spec coverage (against `specs/EMAIL-NOTIFICATIONS.md`):**
- §2 Config (struct, defaults, validation) → Task 1. ✅
- §2 `${VAR}` password → covered by existing global `os.ExpandEnv` (Global Constraints); Task 1 test asserts expansion. ✅
- §3 Component (`EmailConfig`, pure `buildMessage`, `EmailNotifier`, injected `send`, production `smtpSend`, subject/body format, no-op rules) → Tasks 2, 3, 4. ✅
- §4 Wiring (`run.Notifier` interface, `notifiers` slice, `WithNotifier` append, `notify` loop, main.go builds both) → Tasks 5, 6. ✅
- §5 Error handling (log-and-swallow, one channel's failure doesn't stop the other) → Task 5 `notify` loop + fan-out test. ✅
- §6 Testing (buildMessage pure, Notify filtering with fake send, fan-out) → Tasks 2-5. ✅
- §7 Docs/config/version → Task 7. ✅
- §1 Non-goals (no per-channel filter, no per-manifest recipients, plain text, no implicit-TLS, no attachments, no new dep) → respected throughout. ✅

**Placeholder scan:** No `TODO`/`TBD`/"add error handling"; every code step has real code. The one intentional partial-compile note (Task 3 depends on Task 4's `smtpSend`) is called out explicitly with a resolution. ✅

**Type consistency:** `report.EmailConfig` field set is identical in Tasks 2/3/4/6. `send`/`smtpSend` signature `func(ctx context.Context, cfg EmailConfig, msg []byte) error` is identical in Tasks 3 and 4. `buildMessage(cfg, event, payload, now)` identical in Tasks 2 and 3. `run.Notifier` method signature matches `report.*Notifier.Notify` (`ctx, string, map[string]any`). ✅
