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
	"io/fs"
	"math"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// demoOrg is the unmistakably-synthetic org seedDemo stamps on every demo
// developer. Hoisted to package scope so the #475 real-database guard can
// recognise a prior demo database from its org without re-deriving the string.
const demoOrg = "ACME (DEMO)"

// runDemo seeds a synthetic database and hands off to runServe. It returns the
// serve exit code; the seed is deterministic and the DB is recreated each run.
func runDemo(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultListenAddr, "loopback listen address for the demo dashboard")
	dbPath := fs.String("db", filepath.Join(os.TempDir(), "tier-demo.db"), "SQLite path for the synthetic demo data (recreated each run)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	// #475: NEVER delete a real capture database. --db could point at a user's
	// live ~/.tier/tier.db; the Remove below would silently destroy
	// it. Only proceed when the target is absent or is a prior demo database
	// (all-synthetic developers, the demo org). A real or unreadable file is
	// refused, deleting nothing.
	//
	// SECURITY INVARIANT (#508): "a `tierd demo` boot can never let a byte of
	// pre-existing --db content reach an HTTP client." Two independent controls
	// jointly hold this, and BOTH are load-bearing — removing either one alone
	// re-opens the exposure #508 describes (a misconfigured public origin serving
	// real per-developer data):
	//
	//   1. guardDemoDBPath (just below): if the file holds ANY real data, this
	//      returns non-nil and runDemo returns 1 here — execution never reaches
	//      the Remove/seedDemo/serve below, so a real database is refused, never
	//      served (guardDemoDBPath DOES read the file, via store.InspectIdentities
	//      / store.PopulatedTablesOutside, to make that decision — "refused, not
	//      served" is the accurate claim, not "refused, not read from").
	//      TestGuardDemoDBPath_RefusesRealDB and
	//      TestGuardDemoDBPath_RefusesWebhookOnlyDB (demo_test.go) call
	//      guardDemoDBPath directly, so they pin its DECISION but cannot detect
	//      the call site below being deleted or short-circuited from runDemo.
	//      TestDemoSmoke_RefusesRealDataAndNeverBinds (demo_smoke_test.go,
	//      integration-tagged) is the only control arm that drives the real
	//      subprocess end to end and would fail if this call site were removed —
	//      that is the mutant it exists to catch.
	//
	//   2. The FAIL-CLOSED os.Remove loop immediately below: once the guard has
	//      passed, whatever was actually in the file — even guard-approved
	//      content that is not the exact current synthetic cast (e.g. a strict
	//      subset of the current demo-* cast, or extra rows under demo-* names) —
	//      is deleted before store.Open re-creates the file and seedDemo writes
	//      the fixed, deterministic dataset into it. seedDemo alone does NOT
	//      provide this: it is additive/idempotent by design (dedups on
	//      idempotency key / merge SHA, see its doc comment below), so run
	//      without the preceding Remove it MERGES into whatever guard-approved
	//      content was already there rather than replacing it. Removing the
	//      os.Remove calls (while leaving the guard and seedDemo intact) is
	//      exactly the "dark control" #508's re-scope names: it reads as
	//      redundant busywork once the guard has already said "fine, proceed",
	//      and an "optimization" along those lines would silently make the
	//      invariant above depend on guard-approved content being byte-identical
	//      to the canonical cast — which guardDemoDBPath's own doc comment does
	//      NOT promise (it only protects a real DB from being CLOBBERED; a
	//      guard-approved SUBSET or an extra guard-approved row both pass it).
	//      TestDemoSmoke_AlwaysReseedsCanonicalData (demo_smoke_test.go,
	//      integration-tagged) plants a guard-approved artifact seedDemo cannot
	//      produce or overwrite and asserts it is ABSENT from what gets served —
	//      the control arm for "os.Remove specifically", not merely "the reseed
	//      step generically".
	//
	// Neither control alone is the invariant; both must hold. See #475 (guard
	// (1)'s origin) and #508 (this comment's own issue) for the history; do not
	// cite guard (1) alone as "the reason the demo serves only synthetic data" —
	// it decides what is ALLOWED to proceed, not what ends up being SERVED.
	if err := guardDemoDBPath(*dbPath, stderr); err != nil {
		return 1
	}
	// Recreate each run so the demo is deterministic and never accretes. Remove
	// the WAL/SHM sidecars too so "recreated each run" is literally clean — a
	// Ctrl-C'd serve can leave an uncheckpointed WAL behind.
	//
	// 🔴 THE ERRORS ARE FATAL, AND THAT IS THE WHOLE POINT. The first version of
	// this block discarded them (`_ = os.Remove(...)`) while the comment above
	// called it UNCONDITIONAL. It was not: it was best-effort, and a security
	// review REPRODUCED the consequence end to end. With a guard-approved demo DB
	// carrying one extra demo-* row at $999 in a chmod-0555 directory, every
	// unlink failed silently, store.Open reopened the surviving file, seedDemo
	// MERGED into it (it is additive by design), and `GET /api/v1/scores` served
	// demo-ada at total_cost_usd=1024.70 against a canonical 25.70 — with the
	// "SYNTHETIC DATA" banner printed and not one warning on stderr.
	//
	// unlink can fail while the file stays perfectly openable: a non-writable
	// containing directory, a Docker single-file bind mount (EBUSY on Linux), or
	// Windows blocking deletion of an open handle. On any of those, control (1)
	// becomes the ONLY thing between pre-existing content and the HTTP client —
	// and the guard only promises "no developer outside the demo cast", which a
	// planted demo-* row satisfies. Fail closed instead.
	//
	// ⚠️ os.ErrNotExist, NOT fs.ErrNotExist: runDemo shadows the io/fs import
	// with its own `fs := flag.NewFlagSet(...)` above.
	for _, stale := range []string{*dbPath, *dbPath + "-wal", *dbPath + "-shm"} {
		if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(stderr, "demo: cannot delete %s (%v)\n", stale, err)
			_, _ = fmt.Fprintf(stderr, "demo: refusing to serve — the demo recreates its DB every run, and content that survives that is not guaranteed synthetic\n")
			return 1
		}
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "demo: open db: %v\n", err)
		return 1
	}
	if err := seedDemo(context.Background(), db); err != nil {
		_ = db.Close()
		_, _ = fmt.Fprintf(stderr, "demo: seed synthetic data: %v\n", err)
		return 1
	}
	_ = db.Close() // runServe re-opens the same file.

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
		[]string{"--db", *dbPath, "--aggregation", "developer", "--addr", *addr, "--read-only"},
		serveOptions{syntheticDemo: true},
	)
}

// guardDemoDBPath refuses to let `tierd demo` clobber a real database at path
// (#475). A nonexistent path is fine (nothing to protect). An existing file is
// permitted only when store.InspectIdentities confirms it holds nothing but
// synthetic demo identities. Anything else — a directory, an unreadable or
// non-SQLite file, or a database with a single real developer or org — is
// refused with a message naming the path, and NOTHING is deleted. All error
// returns are non-nil so the caller aborts before the Remove calls.
func guardDemoDBPath(path string, stderr io.Writer) error {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // fresh path — nothing to protect
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "demo: cannot inspect %s: %v\n", path, err)
		return err
	}
	if info.IsDir() {
		_, _ = fmt.Fprintf(stderr, "demo: %s is a directory, not a demo database\n", path)
		return errors.New("demo db path is a directory")
	}
	ok, err := isRecreatableDemoDB(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "demo: refusing to touch %s: cannot confirm it is a synthetic demo database (%v); demo only operates on its own synthetic database — pass --db with a new path\n", path, err)
		return err
	}
	if !ok {
		_, _ = fmt.Fprintf(stderr, "demo: refusing to overwrite %s: it contains real (non-demo) data; demo only operates on its own synthetic database — pass --db with a different path\n", path)
		return errors.New("demo db path holds real data")
	}
	return nil
}

// isRecreatableDemoDB reports whether the SQLite file at path is safe for `tierd
// demo` to delete and reseed: every developer it holds must be one of the exact
// synthetic identities seedDemo writes, and every org must be the demo org. An
// empty database qualifies (nothing real to lose). The demo identity set is
// derived from demoData() rather than hardcoded, so the cast can change without
// this guard drifting. A read error (non-SQLite / corrupt / foreign schema) is
// propagated so the caller refuses — fail-closed.
// demoSeededTables is the exact set of tables seedDemo writes: token_events
// (InsertTokenEvents), outcomes (InsertOutcome), actual_spend (InsertActualSpend),
// and org_hierarchy + period_membership (UpsertHierarchies writes BOTH — see
// upsertHierarchyTx in store.go). A recreatable demo database has rows in NO
// other user table. Keep this in lockstep with seedDemo; a table seedDemo starts
// writing must be added here, and the fail-closed default (refuse) makes an
// omission safe — it over-refuses, never over-deletes.
var demoSeededTables = []string{"token_events", "outcomes", "actual_spend", "org_hierarchy", "period_membership"}

func isRecreatableDemoDB(path string) (bool, error) {
	ctx := context.Background()

	// Fail-closed FIRST: any row in a table the demo seeder does not write — e.g.
	// webhook_payloads (the audit trail a webhook-configured deployment holds
	// before any capture), quality_events, developer_alias — means real data,
	// even when no developer/org identity is present to see it (#475). Enumerating
	// from sqlite_master protects tables added to the schema in the future too.
	extra, err := store.PopulatedTablesOutside(ctx, path, demoSeededTables)
	if err != nil {
		return false, err
	}
	if len(extra) > 0 {
		return false, nil
	}

	// Then confirm the identities in the seeded tables are all synthetic.
	ids, err := store.InspectIdentities(ctx, path)
	if err != nil {
		return false, err
	}
	demoDevs := make(map[string]bool)
	for _, d := range demoData() {
		demoDevs[d.name] = true
	}
	for _, dev := range ids.Developers {
		if !demoDevs[dev] {
			return false, nil
		}
	}
	for _, org := range ids.Orgs {
		if org != demoOrg {
			return false, nil
		}
	}
	return true, nil
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
	now := time.Now()
	base := 7 // freshest merge sits ~7 days back, leaving room for the cost lead
	for di := range devs {
		d := &devs[di]
		mergeAt := make(map[string]time.Time, len(d.outcomes))
		for oi := range d.outcomes {
			ts := now.AddDate(0, 0, -(base + oi*3))
			d.outcomes[oi].Timestamp = ts
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
	if err := db.UpsertHierarchies(ctx, hier); err != nil {
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
