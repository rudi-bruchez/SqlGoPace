package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// errManifestsFailed marks the error runEngine returns when the run itself went
// fine but some manifests failed. Each of those already sent its own "fail"
// event, so the run-level notification skips it rather than reporting twice.
var errManifestsFailed = errors.New("manifest(s) failed")

// runFailureTimeout bounds the whole run-level notification: it fires as the
// process is on its way out, so a stalled relay must not hold it there.
const runFailureTimeout = 30 * time.Second

// notifiers builds the configured notification channels: the webhook and the
// SMTP one, both filtered by the same notifications.on_events list. Each is a
// no-op when its own target is unset (empty webhook_url, empty email.host).
func notifiers(cfg *config.Config) []run.Notifier {
	return []run.Notifier{
		report.NewNotifier(cfg.Notifications.WebhookURL, cfg.Notifications.OnEvents),
		// The two EmailConfigs are field-identical by design, so converting rather
		// than copying field by field means a new SMTP setting cannot be silently
		// dropped here — it stops compiling instead.
		report.NewEmailNotifier(report.EmailConfig(cfg.Notifications.Email), cfg.Notifications.OnEvents),
	}
}

// notifyRunFailure reports err on every channel as the "run_failure" event. It
// covers what no manifest report can: the run stopped before, or independently
// of, any manifest — no connection, an unsupported target, crash recovery
// refused. Two errors stay silent: an operator who stopped the run themselves
// does not need to be told, and a manifest that failed already went out as
// "fail".
//
// Delivery never changes the run's outcome: failures are printed to out.
func notifyRunFailure(ctx context.Context, out io.Writer, ns []run.Notifier, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, errManifestsFailed) {
		return
	}
	// Whatever we report, delivery must not ride on the run's context: a hard
	// Ctrl+C cancels it, and a failure that cancel caused downstream still has to
	// reach someone.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runFailureTimeout)
	defer cancel()

	payload := map[string]any{"detail": err.Error()}
	for _, n := range ns {
		if nerr := n.Notify(ctx, "run_failure", payload); nerr != nil {
			fmt.Fprintf(out, "run_failure notification failed: %v\n", nerr)
		}
	}
}
