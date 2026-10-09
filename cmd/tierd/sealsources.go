package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/logsafe"
)

// subscriptionFeeSource is the subscription-fee reconciler's source_watermark name.
const subscriptionFeeSource = "subscription-fees"

// sealGatedSources is every pulled source serve starts under these settings
// (#913-D9 ruling C′): serve registers each in source_watermark before starting
// any, and a month is sealed only once each has settled past its end. The JSONL
// watcher, webhooks, `tierd ship` and /costs pushes are not pulled, and only
// report_grace covers them.
func sealGatedSources(admin *anthropicAdminSettings, oai *openAIUsageSettings, subs []config.Subscription,
	codex *codexRolloutSettings, oc *opencodeSettingsT, muse *museSettingsT) []string {
	var out []string
	for _, s := range []struct {
		on   bool
		name string
	}{
		{admin != nil, collector.SourceAnthropicAdmin},
		{oai != nil, collector.SourceOpenAIUsage},
		{len(subs) > 0, subscriptionFeeSource},
		{codex != nil, collector.SourceCodexRollout},
		{oc != nil, collector.SourceOpencode},
		{muse != nil, collector.SourceMuse},
	} {
		if s.on {
			out = append(out, s.name)
		}
	}
	return out
}

// sourceWatermarks is what serve's sources write to source_watermark.
type sourceWatermarks interface {
	RegisterSources(ctx context.Context, sources []string) (retired []string, err error)
	AdvanceSourceWatermark(ctx context.Context, source string, from, through time.Time) error
	RecordSourceLoss(ctx context.Context, source string, from, through time.Time) error
}

// sealSettled is the collector.SettledFunc serve gives source: it advances the
// source's watermark, and a failed write leaves it where it was, so the seal
// gate waits for the source's next successful pass.
func sealSettled(db sourceWatermarks, source string, logger *slog.Logger) collector.SettledFunc {
	return func(ctx context.Context, from, through time.Time) {
		if err := db.AdvanceSourceWatermark(ctx, source, from, through); err != nil && ctx.Err() == nil {
			logger.Warn("could not record how far a source has settled; sealing waits for its next successful pass (#913)",
				"source", source, "err", logsafe.Err(err))
		}
	}
}

// sealLost is the collector.LostFunc serve gives source: it records the span in
// the store, so no restart forgets it.
func sealLost(db sourceWatermarks, source string) collector.LostFunc {
	return func(ctx context.Context, from, through time.Time) error {
		return db.RecordSourceLoss(ctx, source, from, through)
	}
}

// registerSealSources registers sources and logs them: serve starts no source
// until this succeeds, so no started source is ever missing its row.
func registerSealSources(ctx context.Context, db sourceWatermarks, sources []string, logger *slog.Logger) error {
	retired, err := db.RegisterSources(ctx, sources)
	if err != nil {
		return err
	}
	if len(retired) > 0 {
		logger.Warn("retired pulled sources this serve does not start: sealing no longer waits on them; "+
			"if one is still in use, restore its config and restart serve (#913)", "retired", retired)
	}
	logger.Info("registered the pulled sources: in team/division mode a month is sealed only once each has settled "+
		"past its end; the session watcher, webhooks and pushed costs are covered by report_grace only (#913)", "sources", sources)
	return nil
}
