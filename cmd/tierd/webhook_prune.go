package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/tiermetric/tier/internal/logsafe"
)

// webhookPruneInterval is how often a running serve re-applies the webhook
// payload retention bounds (#846). store.Open prunes once at boot; without this
// a server that never restarts keeps raw bodies, which can hold contributor
// names and emails, past the ~90-day bound docs/privacy.md promises.
const webhookPruneInterval = 24 * time.Hour

// webhookPayloadPruner is the one store method the prune loop needs, so a test
// can drive the error path with a fake.
type webhookPayloadPruner interface {
	PruneWebhookPayloads(ctx context.Context) (int64, error)
}

// runWebhookPayloadPruner prunes every interval until ctx is cancelled. It does
// NOT prune on entry: store.Open has just done that. interval is a parameter so
// a test can run the loop at millisecond cadence.
//
// A failed prune is logged and retried at the next tick; it never stops serve,
// because a retention pass that lags a day is recoverable and a dead server is
// not. An error caused by the shutdown cancel itself is not logged.
func runWebhookPayloadPruner(ctx context.Context, st webhookPayloadPruner, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.PruneWebhookPayloads(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logger.Error("webhook payload prune failed; retrying at the next interval",
					"err", logsafe.Err(err), "retry_in", interval)
				continue
			}
			logger.Info("webhook payload prune", "rows_pruned", n)
		}
	}
}
