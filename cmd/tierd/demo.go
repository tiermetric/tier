// tierd demo (#383): seed an obviously-SYNTHETIC dataset and serve the dashboard
// so someone with no Claude Code history can preview a populated TIER in a single
// command. The data is unmistakable — demo-* developers, DEMO-* issues, an
// "ACME (DEMO)" org — and the console prints a SYNTHETIC banner. It must never be
// mistaken for real scores: it is not a knob on real data, it is a throwaway
// SQLite file, recreated each run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// runDemo seeds a synthetic database and hands off to runServe. It returns the
// serve exit code; the seed is deterministic and the DB is recreated each run.
func runDemo(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultListenAddr, "loopback listen address for the demo dashboard")
	fs.Func("db", "refused: demo creates its own private synthetic database", func(string) error {
		return errors.New("demo: --db is refused; demo creates its own private synthetic database")
	})
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "demo: unexpected positional arguments; use --addr only (--db is refused)")
		return 1
	}
	// The legacy environment-default parsers exit on malformed input. Run them
	// before allocating private storage, so they cannot bypass its cleanup.
	// Demo accepts no config file; serve's remaining setup failures return.
	_ = addSealServeFlags(flag.NewFlagSet("demo defaults", flag.ContinueOnError))
	for _, name := range serveBoolEnvNames {
		_ = envBool(name)
	}
	// Register before allocating/seeding so signals during startup also unwind
	// cleanup. runServe joins its workers and closes SQLite before we remove it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if ctx.Err() != nil {
		return 0
	}
	// No flag, environment database setting or config supplies this path. MkdirTemp creates a fresh
	// random directory with mode 0700; concurrent demos cannot share a database.
	// This replaces the old inspection allow-list, including its pricing audit
	// owner checks and correction/rate bookkeeping exemptions: no pre-existing
	// table, even an orphaned pricing observation audit, is read or reused.
	dir, err := os.MkdirTemp("", "tierd-demo-")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo: create private database directory:", err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			_, _ = fmt.Fprintln(stderr, "demo: remove private database directory:", err)
		}
	}()
	dbPath := filepath.Join(dir, "demo.db")

	db, err := store.Open(dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "demo: open db: %v\n", err)
		return 1
	}
	if err := seedDemo(ctx, db); err != nil {
		_ = db.Close()
		_, _ = fmt.Fprintf(stderr, "demo: seed synthetic data: %v\n", err)
		return 1
	}
	_ = db.Close() // runServe re-opens the same private file.
	if ctx.Err() != nil {
		return 0
	}

	_, _ = fmt.Fprintln(stderr, "================================================================")
	_, _ = fmt.Fprintln(stderr, " TIER DEMO  —  SYNTHETIC DATA, NOT REAL SCORES")
	_, _ = fmt.Fprintln(stderr, " Developers (demo-*), issues (DEMO-*), and spend are invented.")
	if loopbackBind(*addr) {
		// #476: bound to loopback, so it is reachable only from this machine.
		// Point the operator at the exact command to expose the synthetic board to
		// another machine — a non-loopback bind is permitted for `demo` alone.
		_, _ = fmt.Fprintln(stderr, " Dashboard: http://"+*addr+"/")
		_, _ = fmt.Fprintln(stderr, " To view from another machine: tierd demo --addr 0.0.0.0:"+addrPort(*addr))
	} else {
		// Non-loopback: reachable from other machines on this network.
		_, _ = fmt.Fprintln(stderr, " Dashboard (reachable on this network): http://"+*addr+"/")
	}
	_, _ = fmt.Fprintln(stderr, " Ctrl-C to stop.")
	_, _ = fmt.Fprintln(stderr, "================================================================")

	// Developer aggregation so every synthetic developer's own TIER is visible.
	// --read-only (#429): a demo board is inherently a read-only showcase — mount
	// only the read + health routes so the write/ingest/admin surface, the webhook,
	// and the proxies are structurally absent (404). This is also the mode the
	// public community demo (demo.tiermetric.org) runs in.
	//
	// syntheticDemo (#476): the ONLY way to set the internal bit that lets
	// validateBind accept a non-loopback bind without an --api-token. It is safe
	// here because the data is invented and --read-only makes every mutation route
	// structurally absent; it is unreachable from any serve flag/env, so a serve
	// over REAL data is still refused on a non-loopback bind.
	return runServeWithOptions(
		[]string{"--db", dbPath, "--aggregation", "developer", "--addr", *addr, "--read-only"},
		serveOptions{syntheticDemo: true, shutdown: ctx.Done()},
	)
}

// loopbackBind reports whether addr binds only the loopback interface — the
// "localhost" hostname or a loopback IP literal. A hostname other than localhost
// and any unparseable/zoned address are treated as NON-loopback, matching
// validateBind's fail-closed tokenless policy. Used only to choose the demo
// banner text; validateBind keeps its own copy of this logic for its distinct
// error messages.
func loopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// addrPort returns the port component of addr for the demo banner hint, falling
// back to the default demo port when addr has no parseable port.
func addrPort(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return "8080"
}

// demoDeveloper bundles one synthetic developer's cost events, outcomes, and the
// flat-subscription invoice total finance recorded for them — the actual-paid
// figure behind the Spend Leverage tile (list value ÷ what you paid).
type demoDeveloper struct {
	name          string
	team          string
	actualPaidUSD float64 // flat subscription cost for the period; feeds Spend Leverage
	events        []store.TokenEvent
	outcomes      []store.Outcome
}

// seedDemo inserts the synthetic org hierarchy, cost events, outcomes, and a
// per-developer actual_spend invoice. Idempotent: re-running is safe (token
// events carry idempotency keys, outcomes dedup on the merge-commit SHA, and the
// actual_spend row is guarded by an ActualSpendAllWindow read so it is skipped
// when one already exists for the developer).
func seedDemo(ctx context.Context, db *store.DB) error {
	const org, division = "ACME (DEMO)", "Product"
	devs := demoData()

	// Stamp recent timestamps so the synthetic rows land inside the dashboard's
	// default look-back window (a zero time would be filtered out). Attribution is
	// issue-scoped and time-bounded: a cost event only counts toward an outcome
	// when it falls in [merge - store.AttributableWindow, merge] (#136). So each
	// issue's cost must be stamped BEFORE that issue's merge — otherwise every
	// outcome reads as zero-token and its developer is unranked. Spread merges
	// across recent days for a realistic board; lead each issue's cost by a day.
	//
	// .UTC() is load-bearing, not cosmetic (#723). modernc.org/sqlite stores a
	// time.Time as TEXT in Go's own layout INCLUDING the zone suffix
	// ("2006-01-02 15:04:05.999999999 -0700 MST"), so `ORDER BY ts` is a binary
	// string sort over that text. Rows written with different UTC offsets then
	// sort by WALL CLOCK, not by instant: "2026-04-01 08:00:00 -0400 EDT"
	// (= 12:00Z) sorts BEFORE "2026-04-01 10:00:00 +0000 UTC" (= 10:00Z) — the
	// later instant first. A demo-seeded database is internally single-zone and
	// so self-consistent, but an install east or west of UTC that seeds a demo
	// and then ingests real (UTC) webhook rows is exactly the mixed-zone table
	// that voids #711's (ts, id) total order and, with it, #716's digest. This
	// was the tree's ONLY writer that did not normalize; every other one does
	// (internal/webhook, internal/api, cmd/tierd/backfill, every collector).
	//
	// It is placed BEFORE the AddDate arithmetic below, which is deliberate and
	// does more than fix the rendering: AddDate is WALL-CLOCK arithmetic, so
	// normalizing first also puts the seeder's day boundaries on UTC. The
	// synthetic dataset is then identical on every host instead of shifting with
	// the operator's zone.
	//
	// Pinned by TestSeedDemo_StampsUTCTimestamps; the mechanism itself is
	// pinned by TestTimestampZone_MixedOffsetsBreakSQLOrder (store).
	now := time.Now().UTC()
	base := 7 // freshest merge sits ~7 days back, leaving room for the cost lead
	for di := range devs {
		d := &devs[di]
		mergeAt := make(map[string]time.Time, len(d.outcomes))
		for oi := range d.outcomes {
			ts := now.AddDate(0, 0, -(base + oi*3))
			d.outcomes[oi].Timestamp = ts
			created := ts.Add(-time.Duration(12+di*6+oi) * time.Hour)
			d.outcomes[oi].CreatedAt = &created
			d.outcomes[oi].MergedAt = &ts
			mergeAt[d.outcomes[oi].IssueID] = ts
		}
		for ei := range d.events {
			if ts, ok := mergeAt[d.events[ei].IssueID]; ok {
				d.events[ei].Timestamp = ts.AddDate(0, 0, -1) // inside the 14-day window, before merge
			} else {
				d.events[ei].Timestamp = now.AddDate(0, 0, -base)
			}
		}
		base += 2 // stagger developers so the board isn't all one date
	}

	hier := make([]store.HierarchyRow, 0, len(devs))
	for _, d := range devs {
		hier = append(hier, store.HierarchyRow{Developer: d.name, Team: d.team, Division: division, Org: org})
	}
	// Dated membership (#886) places each event under the team its developer held
	// at the event's time, and the synthetic history is dated days before this
	// seed runs. So on a fresh demo store the teams are baselined first, at the
	// same instant the upgrade baseline uses; UpsertHierarchies then finds those
	// open rows and appends nothing. A re-seed finds the store non-empty and is
	// refused the baseline, which leaves the rows the first seed wrote.
	if err := db.SeedDemoMembershipBaseline(ctx, hier); err != nil && !errors.Is(err, store.ErrDemoBaselineStoreNotEmpty) {
		return fmt.Errorf("membership baseline: %w", err)
	}
	if err := db.UpsertHierarchies(ctx, hier, "demo:seed"); err != nil {
		return fmt.Errorf("hierarchy: %w", err)
	}

	var events []store.TokenEvent
	for _, d := range devs {
		events = append(events, d.events...)
	}
	if err := db.InsertTokenEvents(ctx, events); err != nil {
		return fmt.Errorf("token events: %w", err)
	}
	for _, d := range devs {
		for _, o := range d.outcomes {
			if _, err := db.InsertOutcome(ctx, o); err != nil {
				return fmt.Errorf("outcome %s: %w", o.IssueID, err)
			}
		}
	}

	// Seed a per-developer flat-subscription invoice so the Spend Leverage tile
	// (list value ÷ actual paid) renders a real number instead of "—". The
	// actual_spend ledger is append-and-SUM by design, so guard against
	// double-counting on a re-seed: only insert for developers with nothing
	// recorded yet. This keeps seedDemo idempotent alongside the token-event and
	// outcome dedup above.
	paid, err := db.ActualSpendAllWindow(ctx, now.AddDate(0, 0, -60), time.Time{})
	if err != nil {
		return fmt.Errorf("read actual spend: %w", err)
	}
	period := now.UTC().Format("2006-01")
	for _, d := range devs {
		if d.actualPaidUSD <= 0 || paid[d.name] > 0 {
			continue
		}
		if err := db.InsertActualSpend(ctx, store.ActualSpend{
			Developer:       d.name,
			Period:          period,
			ActualPaidMicro: int64(math.Round(d.actualPaidUSD * 1_000_000)),
			Timestamp:       now.AddDate(0, 0, -1),
		}); err != nil {
			return fmt.Errorf("actual spend %s: %w", d.name, err)
		}
	}
	return nil
}

// demoSHA returns a deterministic, unique 40-hex merge-commit SHA for the nth
// synthetic outcome.
func demoSHA(n int) string { return fmt.Sprintf("%040x", n) }

func demoEvent(dev, issue, model string, usd float64, key string) store.TokenEvent {
	return store.TokenEvent{
		Developer:      dev,
		IssueID:        issue,
		Model:          model,
		InputTok:       int(math.Round(usd * 40000)),
		OutputTok:      int(math.Round(usd * 8000)),
		CostMicro:      int64(math.Round(usd * 1_000_000)),
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: key,
	}
}

func demoOutcome(dev, issue string, pr int, weight, quality float64, sha string) store.Outcome {
	return store.Outcome{
		Developer:      dev,
		IssueID:        issue,
		PRNumber:       pr,
		Weight:         weight,
		WeightSource:   "label",
		Quality:        quality,
		MergeCommitSHA: sha,
	}
}

// demoData is a deliberately-varied cast so the demo dashboard shows a realistic
// spread of TIER scores: an efficient developer, two in the middle, and a
// high-spend/low-yield one whose latest change was reverted.
func demoData() []demoDeveloper {
	return []demoDeveloper{
		{name: "demo-ada", team: "Payments", actualPaidUSD: 20.00, // Pro-tier plan
			events: []store.TokenEvent{
				demoEvent("demo-ada", "DEMO-11", "claude-sonnet-5", 12.00, "demo-ada-1"),
				demoEvent("demo-ada", "DEMO-12", "claude-sonnet-5", 9.50, "demo-ada-2"),
				demoEvent("demo-ada", "DEMO-13", "claude-haiku-4-5", 4.20, "demo-ada-3"),
			},
			outcomes: []store.Outcome{
				demoOutcome("demo-ada", "DEMO-11", 101, 5, 1.0, demoSHA(1)),
				demoOutcome("demo-ada", "DEMO-12", 102, 3, 1.0, demoSHA(2)),
				demoOutcome("demo-ada", "DEMO-13", 103, 8, 1.0, demoSHA(3)),
			}},
		{name: "demo-grace", team: "Payments", actualPaidUSD: 50.00, // Max-tier plan
			events: []store.TokenEvent{
				demoEvent("demo-grace", "DEMO-21", "claude-opus-4-8", 28.00, "demo-grace-1"),
				demoEvent("demo-grace", "DEMO-22", "claude-sonnet-5", 16.40, "demo-grace-2"),
				demoEvent("demo-grace", "DEMO-23", "claude-sonnet-5", 11.00, "demo-grace-3"),
			},
			outcomes: []store.Outcome{
				demoOutcome("demo-grace", "DEMO-21", 121, 5, 1.0, demoSHA(4)),
				demoOutcome("demo-grace", "DEMO-22", 122, 5, 1.0, demoSHA(5)),
				demoOutcome("demo-grace", "DEMO-23", 123, 5, 1.0, demoSHA(10)),
			}},
		{name: "demo-linus", team: "Platform", actualPaidUSD: 50.00, // Max-tier plan
			events: []store.TokenEvent{
				demoEvent("demo-linus", "DEMO-31", "claude-opus-4-8", 41.00, "demo-linus-1"),
				demoEvent("demo-linus", "DEMO-32", "claude-sonnet-5", 30.00, "demo-linus-2"),
				demoEvent("demo-linus", "DEMO-33", "claude-sonnet-5", 18.00, "demo-linus-3"),
			},
			outcomes: []store.Outcome{
				demoOutcome("demo-linus", "DEMO-31", 131, 5, 1.0, demoSHA(6)),
				demoOutcome("demo-linus", "DEMO-32", 132, 3, 1.0, demoSHA(7)),
				demoOutcome("demo-linus", "DEMO-33", 133, 5, 1.0, demoSHA(11)),
			}},
		{name: "demo-margaret", team: "Platform", actualPaidUSD: 50.00, // Max-tier plan
			events: []store.TokenEvent{
				demoEvent("demo-margaret", "DEMO-41", "claude-opus-4-8", 96.00, "demo-margaret-1"),
				demoEvent("demo-margaret", "DEMO-42", "claude-opus-4-8", 58.00, "demo-margaret-2"),
				demoEvent("demo-margaret", "DEMO-43", "claude-opus-4-8", 40.00, "demo-margaret-3"),
			},
			outcomes: []store.Outcome{
				// A high-spend developer whose latest merged change was reverted
				// (quality drops to the 0.1 floor) — the low-yield end of the spread.
				// Ranked (n>=3, spend>=$5), so the board shows a low number that
				// counts, not a hidden one — high burn, little to show for it.
				demoOutcome("demo-margaret", "DEMO-41", 141, 5, 0.1, demoSHA(8)),
				demoOutcome("demo-margaret", "DEMO-42", 142, 3, 1.0, demoSHA(9)),
				demoOutcome("demo-margaret", "DEMO-43", 143, 3, 1.0, demoSHA(12)),
			}},
	}
}
