package copilotbilling

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

const DefaultPollInterval = 6 * time.Hour
const DefaultSettleDays = 2
const DefaultLookbackDays = 7

// Store uses the existing paginated export for day-wide reconciliation. There
// is no vendor seat inventory: only the operator's hierarchy and alias map.
type Store interface {
	ListHierarchy(context.Context) ([]store.HierarchyRow, error)
	DeveloperAliases(context.Context) (map[string]string, error)
	ListTokenEvents(context.Context, time.Time, time.Time, store.PageCursor, int) ([]store.TokenEvent, bool, error)
}
type Metrics interface {
	PollComplete(bool)
	EventsIngested(int)
	Clamps(int)
	RemainderSkipped()
	HTTPError(int)
}
type PollerConfig struct {
	Client                   *Client
	Store                    Store
	Interval                 time.Duration
	SettleDays, LookbackDays int
	Logger                   *slog.Logger
	Metrics                  Metrics
	Now                      func() time.Time
	// Settled is called only after a complete day has been durably reconciled.
	Settled collector.SettledFunc
}
type Poller struct{ cfg PollerConfig }

func NewPoller(cfg PollerConfig) *Poller {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultPollInterval
	}
	if cfg.SettleDays == 0 {
		cfg.SettleDays = DefaultSettleDays
	}
	if cfg.LookbackDays == 0 {
		cfg.LookbackDays = DefaultLookbackDays
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Poller{cfg: cfg}
}
func (p *Poller) Name() string { return collector.SourceCopilotBilling }
func (p *Poller) Run(ctx context.Context, _ time.Time, ing collector.Ingester) error {
	if ing == nil || p.cfg.Store == nil || p.cfg.Client == nil {
		return fmt.Errorf("copilot-billing: client, store and ingester are required")
	}
	p.runPass(ctx, ing)
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.runPass(ctx, ing)
		}
	}
}
func (p *Poller) runPass(ctx context.Context, ing collector.Ingester) {
	err := p.pollOnce(ctx, ing)
	if ctx.Err() != nil {
		return
	}
	if p.cfg.Metrics != nil {
		p.cfg.Metrics.PollComplete(err == nil)
	}
	if err != nil {
		p.cfg.Logger.Error("copilot-billing poll failed; will retry next tick", "err", err)
	}
}
func (p *Poller) pollOnce(ctx context.Context, ing collector.Ingester) error {
	return p.walk(ctx, func(events []collector.TokenEvent) error {
		for _, ev := range events {
			if err := ing.Ingest(ctx, ev); err != nil {
				return fmt.Errorf("copilot-billing: ingest delta failed")
			}
			if p.cfg.Metrics != nil {
				p.cfg.Metrics.EventsIngested(1)
			}
		}
		return nil
	}, p.cfg.Settled)
}

// Collect returns the same deltas as Run without mutating the store. Serve is
// the sole writer for this source, as with the existing org pollers.
func (p *Poller) Collect(ctx context.Context, _ time.Time) ([]collector.TokenEvent, error) {
	var out []collector.TokenEvent
	err := p.walk(ctx, func(events []collector.TokenEvent) error { out = append(out, events...); return nil }, nil)
	return out, err
}

var githubLogin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// rosterLogins builds ephemeral request candidates from roster IDs and aliases
// whose canonical target is rostered. Non-login aliases (email, OS paths) are
// not GitHub request candidates. No response can expand this allowlist.
func (p *Poller) rosterLogins(ctx context.Context) (map[string]string, error) {
	rows, err := p.cfg.Store.ListHierarchy(ctx)
	if err != nil {
		return nil, fmt.Errorf("copilot-billing: read hierarchy failed")
	}
	aliases, err := p.cfg.Store.DeveloperAliases(ctx)
	if err != nil {
		return nil, fmt.Errorf("copilot-billing: read aliases failed")
	}
	canonical := func(s string) string {
		if v := aliases[s]; v != "" {
			return v
		}
		return s
	}
	roster := map[string]bool{}
	for _, r := range rows {
		roster[canonical(r.Developer)] = true
	}
	out := map[string]string{}
	add := func(login, dev string) error {
		if dev == collector.UnattributedIssueID || !roster[dev] || !githubLogin.MatchString(login) || strings.HasSuffix(login, "-") || strings.Contains(login, "--") {
			return nil
		}
		login = strings.ToLower(login)
		if old, ok := out[login]; ok && old != dev {
			return fmt.Errorf("copilot-billing: ambiguous case-folded roster alias")
		}
		out[login] = dev
		return nil
	}
	for _, r := range rows {
		if err := add(r.Developer, canonical(r.Developer)); err != nil {
			return nil, err
		}
	}
	for a, dev := range aliases {
		if err := add(a, dev); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type spendKey struct{ developer, model, product string }
type modelProduct struct{ model, product string }

func aggregate(rep usageReport) (map[modelProduct]float64, error) {
	out := map[modelProduct]float64{}
	for _, item := range rep.UsageItems {
		if item.Model == "" || item.Product == "" || item.GrossAmount == nil || math.IsNaN(*item.GrossAmount) || math.IsInf(*item.GrossAmount, 0) {
			return nil, fmt.Errorf("copilot-billing: usage item missing model, product or gross amount")
		}
		k := modelProduct{item.Model, item.Product}
		out[k] += *item.GrossAmount
		// Bound before float->int conversion; never silently saturate real spend.
		if math.Abs(out[k]) >= float64(math.MaxInt64)/1e6 {
			return nil, fmt.Errorf("copilot-billing: gross amount out of range")
		}
	}
	return out, nil
}

func (p *Poller) walk(ctx context.Context, consume func([]collector.TokenEvent) error, settled collector.SettledFunc) error {
	logins, err := p.rosterLogins(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(logins))
	for login := range logins {
		names = append(names, login)
	}
	sort.Strings(names)
	today := p.cfg.Now().UTC().Truncate(24 * time.Hour)
	var firstErr error
	events, skipped, clamps := 0, 0, 0
	defer func() { p.recordClamps(clamps) }()
	for day := today.AddDate(0, 0, -p.cfg.LookbackDays); !day.After(today.AddDate(0, 0, -p.cfg.SettleDays)); day = day.AddDate(0, 0, 1) {
		if err := ctx.Err(); err != nil {
			return err
		}
		desired := map[spendKey]float64{}
		mapped := map[modelProduct]int64{}
		complete := true
		for _, login := range names {
			rep, status, err := p.cfg.Client.usage(ctx, day, login, p.cfg.Client.cfg.MaxRetries)
			if err != nil {
				if p.cfg.Metrics != nil && status != 0 {
					p.cfg.Metrics.HTTPError(status)
				}
				// Non-members have no mapped spend; the org total still carries it.
				if _, ok := err.(usageNotFoundError); ok {
					continue
				}
				p.cfg.Logger.Error("copilot-billing: per-user response unusable; day not reconciled", "err", err)
				if firstErr == nil {
					firstErr = err
				}
				complete = false
				break
			}
			sums, err := aggregate(rep)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				complete = false
				break
			}
			for k, v := range sums {
				desired[spendKey{logins[login], k.model, k.product}] += v
			}
		}
		// An incomplete user walk cannot remove previously captured users. Retry
		// the whole day next tick; never reconcile a partial mapped baseline.
		if !complete {
			skipped++
			if p.cfg.Metrics != nil {
				p.cfg.Metrics.RemainderSkipped()
			}
			continue
		}
		for k, v := range desired {
			if v < 0 {
				v = 0
				desired[k] = v
				clamps++
			}
			mapped[modelProduct{k.model, k.product}] += int64(math.Round(v * 1e6))
		}
		rep, _, orgErr := p.cfg.Client.usage(ctx, day, "", p.cfg.Client.cfg.MaxRetries)
		var org map[modelProduct]float64
		if orgErr == nil {
			org, orgErr = aggregate(rep)
		}
		if orgErr != nil {
			p.httpError(orgErr)
			skipped++
			if p.cfg.Metrics != nil {
				p.cfg.Metrics.RemainderSkipped()
			}
			p.cfg.Logger.Error("copilot-billing: org remainder skipped; org-level response unusable", "err", orgErr, "days", 1)
			if firstErr == nil {
				firstErr = orgErr
			}
			continue
		} else {
			// Include mapped-only keys too: an absent org key is an explicit zero,
			// and mapped spend beyond that zero must count as a clamp.
			for k := range mapped {
				if _, ok := org[k]; !ok {
					org[k] = 0
				}
			}
			for k, v := range org {
				rem := int64(math.Round(v*1e6)) - mapped[k]
				if rem < 0 {
					rem = 0
					clamps++
				}
				desired[spendKey{collector.UnattributedIssueID, k.model, k.product}] = float64(rem) / 1e6
			}
		}
		deltas, n, err := p.reconcileDay(ctx, day, desired, orgErr == nil)
		clamps += n
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
		if err := consume(deltas); err != nil {
			return err
		}
		if err == nil && orgErr == nil && settled != nil && ctx.Err() == nil {
			settled(ctx, day, day.AddDate(0, 0, 1))
		}
		events += len(deltas)
	}
	p.cfg.Logger.Info("copilot-billing poll complete", "events", events, "remainder_skipped_days", skipped)
	return firstErr
}
func (p *Poller) httpError(err error) {
	if status := statusOf(err); status != 0 && p.cfg.Metrics != nil {
		p.cfg.Metrics.HTTPError(status)
	}
}
func (p *Poller) recordClamps(n int) {
	if n > 0 {
		if p.cfg.Metrics != nil {
			p.cfg.Metrics.Clamps(n)
		}
		p.cfg.Logger.Warn("copilot-billing: spend clamped to zero", "clamps", n)
	}
}

// Product has no token_events column. Keep it in the idempotency key rather
// than pretending it is a model or session. Length-delimited hashing comes
// from collector.IdempotencyKey; the product is base64url for lossless reads.
// Only source/org/day/canonical developer/model/product/sequence enter this key;
// amounts and vendor logins never do. The sequence survives process restarts.
func deltaKey(org string, day time.Time, k spendKey, seq int) string {
	return collector.IdempotencyKey(collector.SourceCopilotBilling, org, day.Format("2006-01-02"), k.developer, k.model, k.product) + "/" + base64.RawURLEncoding.EncodeToString([]byte(org)) + "/" + base64.RawURLEncoding.EncodeToString([]byte(k.product)) + "/" + strconv.Itoa(seq)
}
func parseDeltaKey(org string, day time.Time, e store.TokenEvent) (spendKey, int, error) {
	parts := strings.Split(e.IdempotencyKey, "/")
	if len(parts) != 4 || parts[1] != base64.RawURLEncoding.EncodeToString([]byte(org)) {
		return spendKey{}, 0, fmt.Errorf("copilot-billing: invalid stored delta identity")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return spendKey{}, 0, fmt.Errorf("copilot-billing: invalid stored product identity")
	}
	seq, err := strconv.Atoi(parts[3])
	if err != nil || seq < 1 {
		return spendKey{}, 0, fmt.Errorf("copilot-billing: invalid stored revision")
	}
	k := spendKey{e.Developer, e.Model, string(raw)}
	if deltaKey(org, day, k, seq) != e.IdempotencyKey {
		return spendKey{}, 0, fmt.Errorf("copilot-billing: mismatched stored delta identity")
	}
	return k, seq, nil
}

// reconcileDay compares the UNION of old and new keys, including vanished
// keys. A sole source writer plus durable sequences makes partial ingest and
// restart safe: each successfully written key resumes from its stored net.
// A failed org response preserves previous unattributed totals for that day.
func (p *Poller) reconcileDay(ctx context.Context, day time.Time, desired map[spendKey]float64, remainder bool) ([]collector.TokenEvent, int, error) {
	type baseline struct {
		net int64
		seq int
	}
	old := map[spendKey]baseline{}
	cursor := store.PageCursor{TS: day.Add(-time.Nanosecond)}
	for {
		rows, more, err := p.cfg.Store.ListTokenEvents(ctx, day, day.AddDate(0, 0, 1), cursor, store.MaxExportPageSize)
		if err != nil {
			return nil, 0, fmt.Errorf("copilot-billing: read stored day failed")
		}
		for _, e := range rows {
			if e.Source != collector.SourceCopilotBilling {
				continue
			}
			// Another organization's rows cannot become this org's baseline.
			org := p.cfg.Client.cfg.Org
			parts := strings.Split(e.IdempotencyKey, "/")
			if len(parts) == 4 && parts[1] != base64.RawURLEncoding.EncodeToString([]byte(org)) {
				continue
			}
			k, seq, err := parseDeltaKey(org, day, e)
			if err != nil {
				return nil, 0, err
			}
			b := old[k]
			b.net += e.CostMicro
			b.seq = max(b.seq, seq)
			old[k] = b
		}
		if !more {
			break
		}
		if len(rows) == 0 {
			return nil, 0, fmt.Errorf("copilot-billing: empty export page with continuation")
		}
		e := rows[len(rows)-1]
		cursor = store.PageCursor{TS: e.Timestamp, ID: e.ID}
	}
	keys := map[spendKey]bool{}
	for k := range old {
		keys[k] = true
	}
	for k := range desired {
		keys[k] = true
	}
	ordered := make([]spendKey, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.developer != b.developer {
			return a.developer < b.developer
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return a.product < b.product
	})
	var out []collector.TokenEvent
	var reconcileErr error
	clamps := 0
	for _, k := range ordered {
		if k.developer == collector.UnattributedIssueID && !remainder {
			continue
		}
		gross := desired[k]
		if math.IsNaN(gross) || math.IsInf(gross, 0) || math.Abs(gross) >= float64(math.MaxInt64)/1e6 {
			return nil, clamps, fmt.Errorf("copilot-billing: gross amount out of range")
		}
		if gross < 0 {
			gross = 0
			clamps++
		}
		target := int64(math.Round(gross * 1e6))
		b := old[k]
		if b.net < 0 {
			clamps++
		} // repair a pre-existing negative total to >=0
		delta := target - b.net
		if delta == 0 {
			continue
		}
		// The shared store caps positive rows at $10k. Skip only this key and
		// leave the day incomplete; no extra split protocol.
		if delta > store.MaxTokenEventCostMicro {
			reconcileErr = fmt.Errorf("copilot-billing: delta exceeds store row cost ceiling")
			p.cfg.Logger.Error("copilot-billing: oversized key skipped; day incomplete", "err", reconcileErr)
			continue
		}
		out = append(out, collector.TokenEvent{
			Developer: k.developer, IssueID: collector.UnattributedIssueID, Model: k.model,
			CostMicro: delta, Source: collector.SourceCopilotBilling, Fidelity: collector.FidelityDaily,
			PricingBasis: store.PricingCallerAuthoritative, Timestamp: day,
			IdempotencyKey: deltaKey(p.cfg.Client.cfg.Org, day, k, b.seq+1),
			// Like /api/v1/costs imports: no serving-host evidence, and per_token
			// is the storage default for imported dollars, not measured token billing.
			Host: store.HostUnknown, BillingMode: store.BillingPerToken,
		})
	}
	return out, clamps, reconcileErr
}
