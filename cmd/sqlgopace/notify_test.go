package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// notifyCall is one delivery seen by recordingNotifier.
type notifyCall struct {
	event    string
	detail   string
	ctxAlive bool // whether the context the notifier was handed was still live
}

type recordingNotifier struct {
	calls []notifyCall
	err   error
}

func (r *recordingNotifier) Notify(ctx context.Context, event string, payload map[string]any) error {
	detail, _ := payload["detail"].(string)
	r.calls = append(r.calls, notifyCall{event, detail, ctx.Err() == nil})
	return r.err
}

func TestNotifyRunFailureEmitsRunFailureEvent(t *testing.T) {
	ns := []*recordingNotifier{{}, {}}

	notifyRunFailure(context.Background(), io.Discard, []run.Notifier{ns[0], ns[1]},
		errors.New("detect: login failed for user"))

	for i, n := range ns {
		if len(n.calls) != 1 {
			t.Errorf("notifier %d calls = %+v, want exactly one", i, n.calls)
			continue
		}
		if n.calls[0].event != "run_failure" || n.calls[0].detail != "detect: login failed for user" {
			t.Errorf("notifier %d call = %+v, want run_failure carrying the error text", i, n.calls[0])
		}
	}
}

// Nothing must reach a channel for these three: two are already reported by
// their own event, and a successful run reaches the helper as well because
// runEngine reports through a defer.
func TestNotifyRunFailureStaysSilent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		why  string
	}{
		{"operator interruption", fmt.Errorf("open: %w", context.Canceled), "the operator stopped the run themselves"},
		{"manifest failures", fmt.Errorf("3 %w", errManifestsFailed), "each manifest already sent its own fail event"},
		{"successful run", nil, "there is no failure to report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n recordingNotifier

			notifyRunFailure(context.Background(), io.Discard, []run.Notifier{&n}, tc.err)

			if len(n.calls) != 0 {
				t.Errorf("calls = %+v, want none: %s", n.calls, tc.why)
			}
		})
	}
}

// A hard Ctrl+C cancels the run context, so delivery must not ride on it.
func TestNotifyRunFailureDeliversOnCancelledContext(t *testing.T) {
	var n recordingNotifier
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	notifyRunFailure(ctx, io.Discard, []run.Notifier{&n}, errors.New("recovery: database unreachable"))

	if len(n.calls) != 1 {
		t.Fatalf("calls = %+v, want one run_failure", n.calls)
	}
	if !n.calls[0].ctxAlive {
		t.Error("notifier received a canceled context; delivery would fail before it starts")
	}
}

func TestNotifyRunFailureReportsDeliveryFailure(t *testing.T) {
	n := recordingNotifier{err: errors.New("smtp dial: connection refused")}
	var out bytes.Buffer

	notifyRunFailure(context.Background(), &out, []run.Notifier{&n}, errors.New("boom"))

	if !strings.Contains(out.String(), "smtp dial: connection refused") {
		t.Errorf("out = %q, want the delivery error reported", out.String())
	}
}

// notifiers wires both channels from config; the webhook one is exercised end to
// end here so the run-level event is known to reach a real target.
func TestNotifiersFireWebhookForRunFailure(t *testing.T) {
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode webhook body: %v", err)
		}
		got <- body
	}))
	defer srv.Close()

	var cfg config.Config
	cfg.Notifications.WebhookURL = srv.URL
	cfg.Notifications.OnEvents = []string{"run_failure"}

	notifyRunFailure(context.Background(), io.Discard, notifiers(&cfg), errors.New("unsupported engine edition 6"))

	select {
	case body := <-got:
		if body["event"] != "run_failure" || body["detail"] != "unsupported engine edition 6" {
			t.Errorf("webhook body = %v, want event run_failure with the error as detail", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook was not called")
	}
}

// An event the operator did not subscribe to must stay silent on every channel.
func TestNotifiersRespectOnEventsFilter(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called <- struct{}{} }))
	defer srv.Close()

	var cfg config.Config
	cfg.Notifications.WebhookURL = srv.URL
	cfg.Notifications.OnEvents = []string{"fail"} // run_failure not subscribed

	// Delivery is inline, so once this returns the webhook has either been hit or
	// never will be — there is no window left to wait out.
	notifyRunFailure(context.Background(), io.Discard, notifiers(&cfg), errors.New("boom"))

	if len(called) != 0 {
		t.Error("webhook called for an event missing from on_events")
	}
}
