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
