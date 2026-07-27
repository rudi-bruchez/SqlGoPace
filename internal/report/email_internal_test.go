package report

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
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

func TestSMTPSendHonorsCtxCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "220 stalling\r\n")
		select {} // never respond to EHLO -> client blocks on read until the conn is closed
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	done := make(chan error, 1)
	go func() {
		done <- smtpSend(ctx, EmailConfig{Host: host, Port: port, From: "a@x", To: []string{"b@y"}}, []byte("x"))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want error after ctx cancel, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("smtpSend did not return after ctx cancel — ctx not honored past dial")
	}
}
