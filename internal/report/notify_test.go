package report_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

func TestNotifierPostsEnabledEvent(t *testing.T) {
	var gotEvent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if e, ok := payload["event"].(string); ok {
			gotEvent = e
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := report.NewNotifier(srv.URL, []string{"fail"})
	if err := n.Notify(context.Background(), "fail", map[string]any{"manifest": "010_a.yaml"}); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	if gotEvent != "fail" {
		t.Errorf("server received event %q, want fail", gotEvent)
	}
}

func TestNotifierSkipsDisabledEvent(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()

	n := report.NewNotifier(srv.URL, []string{"fail"})
	if err := n.Notify(context.Background(), "done", nil); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	if hit {
		t.Errorf("server was hit for a disabled event")
	}
}

func TestNotifierNoURL(t *testing.T) {
	n := report.NewNotifier("", []string{"fail"})
	if err := n.Notify(context.Background(), "fail", nil); err != nil {
		t.Errorf("Notify() with empty URL = %v, want nil (no-op)", err)
	}
}
