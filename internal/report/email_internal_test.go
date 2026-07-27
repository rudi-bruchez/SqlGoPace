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
