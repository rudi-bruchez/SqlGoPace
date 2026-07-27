package report

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
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
