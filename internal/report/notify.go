package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"time"
)

// Notifier posts JSON payloads to a webhook for enabled events. A nil-safe no-op
// when the URL is empty or the event is not enabled.
type Notifier struct {
	url    string
	events map[string]bool
	client *http.Client
}

// NewNotifier builds a Notifier for the given webhook URL and enabled events.
func NewNotifier(url string, events []string) *Notifier {
	set := make(map[string]bool, len(events))
	for _, e := range events {
		set[e] = true
	}
	return &Notifier{url: url, events: set, client: &http.Client{Timeout: 10 * time.Second}}
}

// Notify posts a JSON body {"event": event, ...payload} when event is enabled.
func (n *Notifier) Notify(ctx context.Context, event string, payload map[string]any) error {
	if n == nil || n.url == "" || !n.events[event] {
		return nil
	}

	body := map[string]any{"event": event}
	maps.Copy(body, payload)
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build notification: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("notification returned status %d", resp.StatusCode)
	}
	return nil
}
