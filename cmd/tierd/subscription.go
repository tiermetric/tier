package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/store"
)

// subscriptionFeeReconcileInterval is how often the reconciler re-checks that
// the CURRENT period's flat fees are posted (#113). A quiet pass is one indexed
// SELECT per configured route, so a short interval costs nothing; the interval
// really only bounds how long after a month rolls over the new period's fee
// appears in org_actual_spend.
const subscriptionFeeReconcileInterval = time.Hour

// subscriptionFeeStore is the slice of *store.DB the reconciler needs. Narrow
// by design ("accept interfaces, return structs"): it makes the reconciler
// testable without a database only where that is honest, and — more usefully —
// it states that this subsystem's ENTIRE authority over the DB is one
// idempotent delta post. Nothing here can read scores, write token events, or
// touch another source's rows.
type subscriptionFeeStore interface {
	// ReconcileSubscriptionFee converges a period's net to the configured fee —
	// used for the CURRENT period, where the live config is the truth.
	ReconcileSubscriptionFee(ctx context.Context, routePrefix, org, period string, feeMicro int64) (int64, error)
	// PostSubscriptionFeeIfUnposted posts a CLOSED period's fee only if that
	// period has no posting yet — used for the #155 catch-up, where converging
	// would let today's config silently restate what the org paid months ago.
	PostSubscriptionFeeIfUnposted(ctx context.Context, routePrefix, org, period string, feeMicro int64) (int64, error)
	// CountActiveSeats reports how many developers the posted fee can actually
	// reach. It is READ-ONLY and exists solely to make an unreachable posting
	// visible; nothing in this subsystem acts on the number beyond logging it.
	CountActiveSeats(ctx context.Context, org, period string) (int, error)
}

// checkSubscriptionCoverage enforces #113's two-artifacts-one-truth rule across
// the `subscriptions:` config block and the ACTIVE price table, in both
// directions but with deliberately different severities.
//
// FATAL — a configured route prefix matching no `billing_mode: subscription`
// entry. The fee would be posted into actual-paid for a route TIER prices as an
// ordinary metered (or guessed) model, so Spend Leverage silently gains
// dollars with no corresponding list-value tokens. That is a wrong NUMBER on a
// CFO-facing metric produced by a typo, and it is unrecoverable after the fact
// without an audit — so refuse to start.
//
// WARN — a subscription-mode table entry that no configured prefix covers. The
// direction matters: the tokens still price correctly (their comparable rate is
// in the table), only the FEE is missing, so the failure is an understated
// actual-paid rather than a fabricated one. It must not be fatal, because
// `tierd score`, `tierd demo`, and any read-only deployment legitimately run a
// table containing subscription rows with no `subscriptions:` block at all —
// making this fatal would break the zero-config CLI to catch an operator's
// omission.
//
// It returns an error rather than exiting so the caller keeps a single exit
// point and the gate is testable without a subprocess.
func checkSubscriptionCoverage(subs []config.Subscription, logger *slog.Logger) error {
	for _, s := range subs {
		// An EMPTY prefix would match every key in store.SubscriptionRoutesWith
		// Prefix, so it would pass this gate unconditionally AND suppress every
		// uncovered-route WARN below — a config that silently claims to cover
		// everything. config.validateSubscriptions already rejects it, but this
		// gate is called directly (by the smoke test, and by any future caller),
		// so it must not depend on someone else's validation to be safe.
		if s.RoutePrefix == "" {
			return fmt.Errorf("subscriptions: route_prefix is empty — an empty prefix would match every subscription route in the price table and silently claim to cover all of them")
		}
		matched := store.SubscriptionRoutesWithPrefix(s.RoutePrefix)
		if len(matched) == 0 {
			return fmt.Errorf("subscriptions: route_prefix %q matches no price-table entry with billing_mode: subscription "+
				"(the active table's subscription routes are %v) — the flat fee would inflate actual-paid for a route TIER "+
				"does not price as subscription. Add the route to your price table (or --prices override) with "+
				"billing_mode: subscription and a comparable list rate, or remove the subscriptions entry",
				s.RoutePrefix, store.SubscriptionRoutes())
		}
		logger.Info("subscription route covered",
			"route_prefix", s.RoutePrefix, "plan", s.Plan, "org", s.OrgKey(),
			"monthly_fee_usd", s.MonthlyFeeUSD, "active_since", s.ActiveSince,
			"price_table_entries", matched)
	}
	// Reverse direction: subscription-priced routes nobody pays a fee for.
	for _, route := range store.SubscriptionRoutes() {
		if subscriptionCovers(subs, route) {
			continue
		}
		logger.Warn("price-table route is billed by subscription but no monthly fee is configured for it "+
			"— its tokens are valued at the comparable list rate, but Spend Leverage's actual-paid side omits the fee, "+
			"so leverage will read HIGH. Add a subscriptions entry whose route_prefix covers it (#113)",
			"route", route)
	}
	return nil
}

// subscriptionCovers reports whether any configured route prefix is a prefix of
// the given price-table key. It is the exact predicate
// store.SubscriptionRoutesWithPrefix applies, expressed from the other side, so
// the two directions of the coverage check cannot disagree about what "covers"
// means.
func subscriptionCovers(subs []config.Subscription, route string) bool {
	for _, s := range subs {
		for _, m := range store.SubscriptionRoutesWithPrefix(s.RoutePrefix) {
			if m == route {
				return true
			}
		}
	}
	return false
}

// subscriptionPeriods returns the periods one subscription's startup pass must
// reconcile, given the current period (#155).
//
// Without active_since it is the current period ALONE — today's behaviour, kept
// as the default so no deployment silently starts backfilling history on
// upgrade. With active_since it is every period from there through now, which
// is what closes the real gap: a tierd offline across a month boundary never
// posted that month's fee, so its Spend Leverage denominator was understated by
// the whole amount the org actually paid.
//
// Re-walking the range is free — an already-posted period is skipped by its own
// source-scoped rows — so this is a catch-up, not a rewrite. (There is no ledger
// TABLE; see the header of internal/store/subscription.go for why the `source`
// column replaced it.)
func subscriptionPeriods(s config.Subscription, current string) ([]string, error) {
	if s.ActiveSince == "" {
		return []string{current}, nil
	}
	return store.PeriodRange(s.ActiveSince, current)
}

// runSubscriptionFeeReconciler keeps org_actual_spend carrying each configured
// subscription's flat monthly fee (#113), catching up missed periods on startup
// (#155), until ctx is cancelled.
//
// The two passes are deliberately DIFFERENT, and the asymmetry is the design:
//
//   - the STARTUP pass covers active_since..current, because that is the only
//     moment at which periods can be missing — the process was not running for
//     them. It is the one-shot catch-up.
//   - each TICK covers the CURRENT period only. A steady-state server has
//     nothing to catch up, so re-walking history hourly would issue N
//     transactions per tick forever to learn what it already knows. The tick
//     exists solely so a server running across a month boundary posts the new
//     month without a restart.
//
// Every pass is idempotent (store.ReconcileSubscriptionFee posts only the
// difference), so restarts, ticks, and a mid-month fee edit all converge rather
// than accumulate. Errors are logged and retried on the next tick — never fatal
// to serve.
func runSubscriptionFeeReconciler(ctx context.Context, db subscriptionFeeStore, subs []config.Subscription, logger *slog.Logger, interval time.Duration) {
	reconcileSubscriptionFeesOnce(ctx, db, subs, logger, store.CurrentPeriod(time.Now()), true)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Re-read the clock EVERY tick, not once at start: the tick's entire
			// reason to exist is that the current period changes underneath a
			// long-running server at the month boundary.
			reconcileSubscriptionFeesOnce(ctx, db, subs, logger, store.CurrentPeriod(time.Now()), false)
		}
	}
}

// reconcileSubscriptionFeesOnce runs ONE reconcile pass over every configured
// subscription. backfill selects the startup semantics (active_since..current)
// over the steady-state ones (current period only) — see
// runSubscriptionFeeReconciler for why the two differ.
//
// current — the billing period to treat as "now" — is a PARAMETER rather than a
// `time.Now()` read inside, so the caller and the pass can never disagree about
// which month this is. Reading the clock here would mean a test computing its
// expected period from a second, independent `time.Now()`, which is a race
// across a UTC month rollover and, worse, makes the month-boundary behaviour
// itself unreachable by a test. The seam costs one argument and buys both.
//
// It never returns an error: a failed post is logged and left for the next
// tick, because a fee-bookkeeping lag is recoverable and taking down serve for
// it is not. Extracted from the loop so the two semantics are testable without
// driving a ticker.
func reconcileSubscriptionFeesOnce(ctx context.Context, db subscriptionFeeStore, subs []config.Subscription, logger *slog.Logger, current string, backfill bool) {
	for _, s := range subs {
		periods := []string{current}
		if backfill {
			p, err := subscriptionPeriods(s, current)
			if err != nil {
				// Unreachable with a config that passed validateSubscriptions
				// (active_since is period-shaped and <= now there). Logged rather
				// than swallowed so a future caller that skips validation gets a
				// visible complaint instead of a silently empty backfill.
				//
				// It falls through to the CURRENT period rather than skipping the
				// route: a bad active_since is a reason not to trust the HISTORY, not
				// a reason to stop billing this month. Skipping outright would turn a
				// typo in an optional backfill hint into silently unposted current
				// spend — a strictly worse failure than the one being reported.
				logger.Error("subscription fee backfill range rejected; falling back to the current period only",
					"route_prefix", s.RoutePrefix, "active_since", s.ActiveSince, "current", current, "err", err)
				p = []string{current}
			}
			periods = p
		}
		feeMicro := store.DollarsToMicro(s.MonthlyFeeUSD)
		// seatChecked bounds the seat lookup below to ONE extra SELECT per
		// configured route per pass. A 240-period catch-up must not issue 240 of
		// them, and it does not need to: a wrong org key has no seats in ANY
		// period, so the first period actually posted is a sufficient probe.
		seatChecked := false
		for _, period := range periods {
			// Stop cleanly mid-catch-up. Without this, a SIGTERM during a long
			// backfill would let the loop run to completion against a cancelled ctx,
			// where every ReconcileSubscriptionFee fails fast and logs an ERROR —
			// up to store.MaxPeriodRange of them, which reads like a fault at exactly
			// the moment an operator is looking for one. (Named as the STORE's bound,
			// not config.maxBackfillPeriods: that one is unexported and unreachable
			// from this file, and this loop's period list came from store.PeriodRange.
			// TestBackfillBoundMatchesStore pins the two equal.)
			if ctx.Err() != nil {
				return
			}
			// CURRENT period converges to the configured fee (a mid-month raise
			// posts the difference); a CLOSED period is only ever FILLED IN. The
			// split is the whole reason there are two store methods: converging a
			// closed period would let an August config edit restate June's
			// actual-paid, moving every historical Spend Leverage figure with no
			// record that anything changed. #155 asked for "active and UNPOSTED".
			post := db.ReconcileSubscriptionFee
			if period != current {
				post = db.PostSubscriptionFeeIfUnposted
			}
			delta, err := post(ctx, s.RoutePrefix, s.OrgKey(), period, feeMicro)
			if err != nil {
				logger.Error("subscription fee reconcile failed",
					"route_prefix", s.RoutePrefix, "org", s.OrgKey(), "period", period, "err", err)
				continue
			}
			if delta == 0 {
				continue // already posted for this period — the idempotent case
			}
			// The money moved. Now check that it can REACH anyone (#113 review Y4).
			//
			// config.Subscription.OrgKey() falls back to the route prefix when `org:`
			// is absent, and nothing validates that key against the DB — the startup
			// coverage gate runs before store.Open, correctly, so it structurally
			// cannot. An org with no seats allocates to nobody: ActualPaidUSD stays 0
			// and Spend Leverage renders "—", while this very line logs a successful
			// post every month with nothing connecting the two. That is the mirror of
			// the uncovered-route case that already WARNs, and it understates rather
			// than fabricates, so it warns rather than refusing to start.
			//
			// The probe is always the CURRENT period, never the period just posted.
			//
			// ⚠️ NOT because old periods have no seats — a FIRST enrollment is
			// backdated to "0000-01" (store.go, UpsertHierarchy), so a founding
			// developer really does hold a seat 15 years back. An earlier draft of
			// this comment claimed otherwise and was wrong; measured,
			// CountActiveSeats(acme, 2011-01) == 1 after a single enrollment.
			//
			// The real false-positive sources are org SWITCHES (the new org's
			// period_start is the switch month — past invoices stay with the old
			// org) and DEPARTURES (period_end set). Both give legitimate zero-seat
			// months inside a long catch-up, and warning on those is how a real
			// signal becomes one operators mute. The current period is the one where
			// a correctly-configured org always has seats and a typo'd key never
			// does — so detection of the failure this targets is unimpaired.
			//
			// The cost of this choice, stated plainly: a backfill covering months
			// before anyone was in that org goes unwarned.
			if !seatChecked {
				seatChecked = true
				switch seats, err := db.CountActiveSeats(ctx, s.OrgKey(), current); {
				case err != nil:
					logger.Error("subscription fee seat check failed; cannot tell whether this org's fee reaches any developer",
						"route_prefix", s.RoutePrefix, "org", s.OrgKey(), "period", current, "err", err)
				case seats == 0:
					logger.Warn("subscription fee posted to an org with NO active developer seats — the money is recorded but will NOT reach Spend Leverage, "+
						"which allocates org spend only through period_membership. Check the `org:` key (it defaults to the route_prefix when omitted) "+
						"and that developers are mapped to it via POST /api/v1/hierarchy (#113)",
						"route_prefix", s.RoutePrefix, "org", s.OrgKey(), "period", current)
				}
			}
			// One line per period ACTUALLY posted (#155): after a catch-up an
			// operator must be able to read off exactly which months were caught up,
			// and a line per period EXAMINED would bury that in no-ops.
			//
			// The BACKFILL case is split out at WARN, and the severity is the honest
			// part. Config carries ONE fee with no per-period history, so a filled-in
			// closed month is filled at TODAY'S configured fee — not at whatever the
			// org actually paid that month. This is the adoption path, not an edge
			// case: an operator upgrading sets active_since to the month the plan
			// started and their CURRENT fee, and a plan that went $100 → $200 has
			// every pre-raise month posted at $200. Understating or overstating a
			// closed month's actual-paid moves its Spend Leverage silently, so the
			// one moment it happens is the one moment to say so.
			if backfill && period != current {
				logger.Warn("subscription fee BACKFILLED into a closed period at the CURRENTLY configured fee "+
					"— config carries no per-period fee history, so if this plan's price has changed since, this month's "+
					"actual-paid (and its Spend Leverage) is now wrong by the difference. Correct it with "+
					"POST /api/v1/org_actual_spend for the affected period (#155)",
					"route_prefix", s.RoutePrefix, "org", s.OrgKey(), "period", period,
					"posted_usd", store.MicroToDollars(delta),
					"source", store.SubscriptionSpendSource(s.RoutePrefix))
				continue
			}
			logger.Info("subscription fee posted to org_actual_spend",
				"route_prefix", s.RoutePrefix, "org", s.OrgKey(), "period", period,
				"delta_usd", store.MicroToDollars(delta),
				"source", store.SubscriptionSpendSource(s.RoutePrefix),
				// Constant, and kept deliberately: both lines carry the same
				// route/org/period keys, so a log consumer that filtered on
				// backfill=false would silently start matching nothing if the field
				// disappeared when the backfill case moved to its own WARN.
				"backfill", false)
		}
	}
}
