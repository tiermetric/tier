package cursoradmin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/store"
)

const (
	DefaultPollInterval = time.Hour
	DefaultSettleDays   = 1
	DefaultLookbackDays = 7
	// byoKindMarker was verified live in Cursor on 2026-10-21 at 18:12:29Z:
	// a BYO request had kind "User API Key", isChargeable=true,
	// isTokenBasedCall=false, and chargedCents covering only Cursor's fee.
	byoKindMarker = "api key"
)

// Store uses existing alias/roster and event-export reads. Email is looked up
// transiently, never used as a fallback developer or reconciliation identity.
type Store interface {
	DeveloperAliases(context.Context) (map[string]string, error)
	ListHierarchy(context.Context) ([]store.HierarchyRow, error)
	ListTokenEvents(context.Context, time.Time, time.Time, store.PageCursor, int) ([]store.TokenEvent, bool, error)
}

type Counts struct{ Events, Fallback, Uncosted, BYO, NonChargeable, Clamped int }
type Metrics interface {
	PollComplete(bool)
	EventsIngested(int)
	Counts(Counts)
	AuthFailure(int)
}
type PollerConfig struct {
	Client                   *Client
	Store                    Store
	Interval                 time.Duration
	SettleDays, LookbackDays int
	Logger                   *slog.Logger
	Metrics                  Metrics
	Now                      func() time.Time
	Settled                  collector.SettledFunc
	// Lost blocks sealing for uncosted days that may already be certified or
	// cannot be re-read, until the operator records the month as a gap.
	Lost collector.LostFunc
}
type Poller struct {
	PollerConfig
	// Retain unresolved days until a clean re-read or a successful loss record.
	uncostedByDay map[time.Time]int
	// Maximum boundary passed to Settled in this process. Zero means an earlier
	// process may already have certified any day in the current window.
	certifiedThrough time.Time
}

func NewPoller(cfg PollerConfig) *Poller {
	if cfg.Interval < DefaultPollInterval {
		cfg.Interval = DefaultPollInterval
	}
	if cfg.SettleDays <= 0 {
		cfg.SettleDays = DefaultSettleDays
	}
	if cfg.LookbackDays <= 0 {
		cfg.LookbackDays = DefaultLookbackDays
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Poller{PollerConfig: cfg, uncostedByDay: map[time.Time]int{}}
}
func (p *Poller) Name() string { return collector.SourceCursorAdmin }
func (p *Poller) Run(ctx context.Context, _ time.Time, ing collector.Ingester) error {
	if ing == nil {
		return fmt.Errorf("cursor-admin: Ingester is required")
	}
	p.runPass(ctx, ing)
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.runPass(ctx, ing)
		}
	}
}
func (p *Poller) runPass(ctx context.Context, ing collector.Ingester) {
	now := p.Now().UTC()
	counts, err := p.pollAt(ctx, ing, now)
	if ctx.Err() != nil {
		return
	}
	if p.Metrics != nil {
		p.Metrics.PollComplete(err == nil)
	}
	if err != nil {
		var status *StatusError
		if errors.As(err, &status) && (status.Status == 401 || status.Status == 403) {
			if p.Metrics != nil {
				p.Metrics.AuthFailure(status.Status)
			}
			p.Logger.Error("cursor-admin authorization failed", "status", status.Status, "hint", AuthHint)
		} else {
			p.Logger.Error("cursor-admin poll failed; will retry next tick", "err", logsafe.Err(err))
		}
		return
	}
	if p.Metrics != nil {
		p.Metrics.Counts(counts)
		p.Metrics.EventsIngested(counts.Events)
	}
	p.Logger.Info("cursor-admin poll complete", "events", counts.Events, "fallback", counts.Fallback, "uncosted", counts.Uncosted, "byo_skipped", counts.BYO, "nonchargeable_zero_cost_skipped", counts.NonChargeable, "clamped", counts.Clamped)
	start, end := p.windowAt(now)
	// Record late discoveries immediately: certification cannot be revoked.
	// Otherwise allow pricing to heal until the final read. A recorded loss
	// blocks the month until tierd seal --skip; failed or absent hooks keep pins.
	for day := range p.uncostedByDay {
		if p.Lost != nil && (p.certifiedThrough.IsZero() || day.Before(p.certifiedThrough) || !day.After(start)) {
			if err := p.Lost(ctx, day, day.AddDate(0, 0, 1)); err != nil {
				p.Logger.Error("cursor-admin could not record lost spend; day remains pinned", "day", day.Format("2006-01-02"), "err", logsafe.Err(err))
				continue
			}
			delete(p.uncostedByDay, day)
		}
	}
	// Settle only the clean prefix before the first still-pinned day, including
	// aged-out days whose loss must be retried before certification can pass them.
	if p.Settled != nil {
		through := end.AddDate(0, 0, 1)
		for day := range p.uncostedByDay {
			if day.Before(through) {
				through = day
			}
		}
		if start.Before(through) {
			p.Settled(ctx, start, through)
			if through.After(p.certifiedThrough) {
				p.certifiedThrough = through
			}
		}
	}
}
func (p *Poller) windowAt(now time.Time) (time.Time, time.Time) {
	today := now.UTC().Truncate(24 * time.Hour)
	return today.AddDate(0, 0, -p.LookbackDays), today.AddDate(0, 0, -p.SettleDays)
}

type rowKey struct{ developer, model, basis string }
type total struct {
	usage    store.CostUsage
	cost     int64
	revision int
	mode     string
}

func (p *Poller) identities(ctx context.Context) (map[string]string, error) {
	aliases, err := p.Store.DeveloperAliases(ctx)
	if err != nil {
		return nil, fmt.Errorf("cursor-admin: alias read failed")
	}
	roster, err := p.Store.ListHierarchy(ctx)
	if err != nil {
		return nil, fmt.Errorf("cursor-admin: roster read failed")
	}
	members := map[string]bool{}
	for _, r := range roster {
		members[r.Developer] = true
	}
	out := map[string]string{}
	for email, dev := range aliases {
		folded := strings.ToLower(email)
		if previous, exists := out[folded]; exists && previous != dev {
			// Keep ambiguity sticky across any remaining aliases for this email.
			out[folded] = ""
		} else {
			out[folded] = dev
		}
	}
	for email, dev := range out {
		if !members[dev] || strings.Contains(dev, "@") {
			delete(out, email)
		}
	}
	// The members endpoint supplies no additional authoritative identity evidence:
	// aliases + local hierarchy are sufficient, so it is never needed here.
	return out, nil
}

func hasAutomationID(id string) bool {
	// Cursor sends the JSON string "null" for unset automation identity fields.
	return id != "" && !strings.EqualFold(strings.TrimSpace(id), "null")
}

func (p *Poller) aggregate(day time.Time, events []usageEvent, identities map[string]string, desired map[rowKey]total, counts *Counts) error {
	for _, e := range events {
		if strings.Contains(strings.ToLower(e.Kind), byoKindMarker) {
			counts.BYO++
			continue
		}
		ms, err := strconv.ParseInt(e.Timestamp, 10, 64)
		if err != nil {
			return fmt.Errorf("cursor-admin: invalid event timestamp")
		}
		ts := time.UnixMilli(ms).UTC()
		if ts.Before(day) || !ts.Before(day.AddDate(0, 0, 1)) {
			return fmt.Errorf("cursor-admin: event outside requested day")
		}
		u := store.CostUsage{}
		if e.IsTokenBasedCall {
			if e.TokenUsage == nil {
				return fmt.Errorf("cursor-admin: token event missing tokenUsage")
			}
			u = store.CostUsage{Input: e.TokenUsage.Input, Output: e.TokenUsage.Output, CacheRead: e.TokenUsage.CacheRead, CacheWrite5m: e.TokenUsage.CacheWrite}
		}
		// The API exposes one cache-write class; use the ordinary request 5m class,
		// as the JSONL/proxy legacy cache-write mapping does. No 1h evidence exists.
		clamp := func(v int) int {
			if v < 0 {
				counts.Clamped++
				return 0
			}
			return v
		}
		u.Input = clamp(u.Input)
		u.Output = clamp(u.Output)
		u.CacheRead = clamp(u.CacheRead)
		u.CacheWrite5m = clamp(u.CacheWrite5m)
		if !e.IsChargeable && u == (store.CostUsage{}) && e.ChargedCents == 0 {
			counts.NonChargeable++
			continue
		}
		model := store.NormalizeModel(e.Model)
		if model == "" {
			return fmt.Errorf("cursor-admin: event missing model")
		}
		basis := store.PricingTokenDerived
		var cost int64
		mode := store.BillingPerToken
		if e.IsTokenBasedCall && store.IsAuditedRate("", model) {
			cost, mode = store.ComputeCostHost("", model, u)
		} else {
			counts.Fallback++
			basis = store.PricingCallerAuthoritative
			cents := e.ChargedCents
			if e.IsTokenBasedCall {
				cents = e.TokenUsage.TotalCents
			}
			if math.IsNaN(cents) || math.IsInf(cents, 0) {
				return fmt.Errorf("cursor-admin: invalid vendor cents")
			}
			if cents < 0 {
				counts.Clamped++
				cents = 0
			}
			cost = store.DollarsToMicro(cents / 100)
			if cost == 0 {
				if !e.IsChargeable {
					counts.NonChargeable++
				} else {
					counts.Uncosted++
				}
				continue
			}
		}
		dev := collector.UnattributedIssueID
		if !hasAutomationID(e.ServiceAccountID) && !hasAutomationID(e.ServiceAccountName) && !hasAutomationID(e.CloudAgentID) && !hasAutomationID(e.AutomationID) && !e.IsHeadless {
			if canonical := identities[strings.ToLower(e.Email)]; canonical != "" {
				dev = canonical
			}
		}
		// Email dies here: only the canonical developer participates in any row/key.
		key := rowKey{dev, model, basis}
		a := desired[key]
		if cost > math.MaxInt64-a.cost {
			return fmt.Errorf("cursor-admin: daily cost overflow")
		}
		a.cost += cost
		a.mode = mode
		next := store.CostUsage{Input: a.usage.Input + u.Input, Output: a.usage.Output + u.Output, CacheRead: a.usage.CacheRead + u.CacheRead, CacheWrite5m: a.usage.CacheWrite5m + u.CacheWrite5m}
		if next.Input < a.usage.Input || next.Output < a.usage.Output || next.CacheRead < a.usage.CacheRead || next.CacheWrite5m < a.usage.CacheWrite5m {
			return fmt.Errorf("cursor-admin: daily token overflow")
		}
		a.usage = next
		desired[key] = a
	}
	return nil
}

func (p *Poller) stored(ctx context.Context, day time.Time) (map[rowKey]total, error) {
	out := map[rowKey]total{}
	cursor := store.PageCursor{}
	for {
		rows, more, err := p.Store.ListTokenEvents(ctx, day, day.AddDate(0, 0, 1), cursor, 1000)
		if err != nil {
			return nil, fmt.Errorf("cursor-admin: stored day read failed")
		}
		for _, e := range rows {
			if e.Source != collector.SourceCursorAdmin {
				continue
			}
			if e.PricingBasis != store.PricingTokenDerived && e.PricingBasis != store.PricingCallerAuthoritative {
				return nil, fmt.Errorf("cursor-admin: unexpected stored pricing basis on %s; cannot safely number revisions", day.Format("2006-01-02"))
			}
			k := rowKey{e.Developer, store.NormalizeModel(e.Model), e.PricingBasis}
			a := out[k]
			a.cost += e.CostMicro
			a.mode = e.BillingMode
			a.revision++
			a.usage.Input += e.InputTok
			a.usage.Output += e.OutputTok
			a.usage.CacheRead += e.CacheRead
			a.usage.CacheWrite5m += e.CacheWrite5m
			a.usage.CacheWrite1h += e.CacheWrite1h
			out[k] = a
		}
		if !more {
			return out, nil
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("cursor-admin: empty stored page")
		}
		last := rows[len(rows)-1]
		cursor = store.PageCursor{TS: last.Timestamp, ID: last.ID}
	}
}

// reconcile compares the UNION of desired and stored keys for the whole day.
// Missing keys target zero, so alias changes reverse old unattributed spend.
// One serve goroutine owns this source's read/append loop, as with the org
// pollers. Revision counts survive restarts and partial ingest failures.
func (p *Poller) reconcile(ctx context.Context, day time.Time, desired map[rowKey]total, counts *Counts) ([]collector.TokenEvent, error) {
	old, err := p.stored(ctx, day)
	if err != nil {
		return nil, err
	}
	keys := map[rowKey]bool{}
	for k := range desired {
		keys[k] = true
	}
	for k := range old {
		keys[k] = true
	}
	ordered := make([]rowKey, 0, len(keys))
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
		return a.basis < b.basis
	})
	var out []collector.TokenEvent
	for _, k := range ordered {
		want, have := desired[k], old[k]
		// Clamp targets before subtraction, never the signed delta. A correction
		// can be negative while the resulting net must stay nonnegative in every class.
		clamp := func(v int) int {
			if v < 0 {
				counts.Clamped++
				return 0
			}
			return v
		}
		want.usage.Input = clamp(want.usage.Input)
		want.usage.Output = clamp(want.usage.Output)
		want.usage.CacheRead = clamp(want.usage.CacheRead)
		want.usage.CacheWrite5m = clamp(want.usage.CacheWrite5m)
		want.usage.CacheWrite1h = clamp(want.usage.CacheWrite1h)
		if want.cost < 0 {
			counts.Clamped++
			want.cost = 0
		}
		mode := want.mode
		if mode == "" {
			mode = have.mode
		}
		if mode == "" {
			mode = store.BillingPerToken
		}
		delta := total{cost: want.cost - have.cost, usage: store.CostUsage{Input: want.usage.Input - have.usage.Input, Output: want.usage.Output - have.usage.Output, CacheRead: want.usage.CacheRead - have.usage.CacheRead, CacheWrite5m: want.usage.CacheWrite5m - have.usage.CacheWrite5m, CacheWrite1h: want.usage.CacheWrite1h - have.usage.CacheWrite1h}}
		if delta.cost == 0 && delta.usage == (store.CostUsage{}) {
			continue
		}
		if delta.cost > store.MaxTokenEventCostMicro*10000 {
			return nil, fmt.Errorf("cursor-admin: daily correction exceeded row budget")
		}
		// The store caps a positive cost per insert. Split a large correction into
		// sequence rows so the daily SUM remains exact instead of silently clipping.
		for {
			cost := min(delta.cost, store.MaxTokenEventCostMicro)
			have.revision++
			out = append(out, collector.TokenEvent{Developer: k.developer, IssueID: collector.UnattributedIssueID, Model: k.model, PricingBasis: k.basis,
				InputTok: delta.usage.Input, OutputTok: delta.usage.Output, CacheRead: delta.usage.CacheRead, CacheWrite5m: delta.usage.CacheWrite5m, CacheWrite1h: delta.usage.CacheWrite1h, CostMicro: cost,
				Source: collector.SourceCursorAdmin, Fidelity: collector.FidelityDaily, Timestamp: day, BillingMode: mode,
				IdempotencyKey: collector.IdempotencyKey(collector.SourceCursorAdmin, day.Format("2006-01-02"), k.developer, k.model, k.basis, strconv.Itoa(have.revision))})
			delta.cost -= cost
			delta.usage = store.CostUsage{}
			if delta.cost == 0 {
				break
			}
		}
	}
	return out, nil
}

func (p *Poller) collect(ctx context.Context, ing collector.Ingester, now time.Time) ([]collector.TokenEvent, Counts, error) {
	var counts Counts
	ids, err := p.identities(ctx)
	if err != nil {
		return nil, counts, err
	}
	start, end := p.windowAt(now)
	var out []collector.TokenEvent
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		desired := map[rowKey]total{}
		beforeUncosted := counts.Uncosted
		err := p.Client.fetchDay(ctx, day, func(events []usageEvent) error { return p.aggregate(day, events, ids, desired, &counts) })
		if n := counts.Uncosted - beforeUncosted; n > 0 {
			p.uncostedByDay[day] = n
		} else if err == nil {
			delete(p.uncostedByDay, day)
		}
		if err != nil {
			return nil, counts, err
		}
		events, err := p.reconcile(ctx, day, desired, &counts)
		if err != nil {
			return nil, counts, err
		}
		for _, e := range events {
			if ing != nil {
				if err := ing.Ingest(ctx, e); err != nil {
					return nil, counts, fmt.Errorf("cursor-admin: delta ingest failed")
				}
				counts.Events++
			}
		}
		out = append(out, events...)
	}
	return out, counts, nil
}
func (p *Poller) pollOnce(ctx context.Context, ing collector.Ingester) (Counts, error) {
	return p.pollAt(ctx, ing, p.Now().UTC())
}
func (p *Poller) pollAt(ctx context.Context, ing collector.Ingester, now time.Time) (Counts, error) {
	_, counts, err := p.collect(ctx, ing, now)
	return counts, err
}
func (p *Poller) Collect(ctx context.Context, _ time.Time) ([]collector.TokenEvent, error) {
	events, _, err := p.collect(ctx, nil, p.Now().UTC())
	return events, err
}
