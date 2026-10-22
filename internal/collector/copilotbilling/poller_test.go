package copilotbilling

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
)

type metricSink struct {
	clamps, skips, events, ok, failed int
	statuses                          []int
}

func (m *metricSink) PollComplete(ok bool) {
	if ok {
		m.ok++
	} else {
		m.failed++
	}
}
func (m *metricSink) EventsIngested(n int) { m.events += n }
func (m *metricSink) Clamps(n int)         { m.clamps += n }
func (m *metricSink) RemainderSkipped()    { m.skips++ }
func (m *metricSink) HTTPError(s int)      { m.statuses = append(m.statuses, s) }

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func roster(t *testing.T, db *store.DB, dev string) {
	t.Helper()
	if err := db.UpsertHierarchy(t.Context(), dev, "team", "division", "acme", "test"); err != nil {
		t.Fatal(err)
	}
}
func testPoller(t *testing.T, db *store.DB, srv *httptest.Server, logs *bytes.Buffer, m *metricSink) *Poller {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(logs, nil))
	c := mustClient(t, ClientConfig{Org: "acme", Token: secretToken, BaseURL: srv.URL, Logger: logger, Now: func() time.Time { return fixedNow }})
	return NewPoller(PollerConfig{Client: c, Store: db, LookbackDays: 2, SettleDays: 2, Logger: logger, Metrics: m, Now: func() time.Time { return fixedNow }})
}
func stored(t *testing.T, db *store.DB) []store.TokenEvent {
	t.Helper()
	day := fixedNow.UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	rows, more, err := db.ListTokenEvents(t.Context(), day, day.AddDate(0, 0, 1), store.PageCursor{TS: day.Add(-time.Second)}, 10000)
	if err != nil || more {
		t.Fatalf("stored rows more=%v err=%v", more, err)
	}
	return rows
}
func totals(t *testing.T, db *store.DB) map[spendKey]int64 {
	t.Helper()
	out := map[spendKey]int64{}
	for _, e := range stored(t, db) {
		if e.Source != collector.SourceCopilotBilling {
			continue
		}
		k, _, err := parseDeltaKey("acme", e.Timestamp, e)
		if err != nil {
			t.Fatal(err)
		}
		out[k] += e.CostMicro
	}
	return out
}

func TestCopilotPerUserGrossRowsRemainderRosterPrivacyAndReprice(t *testing.T) {
	db := testDB(t)
	roster(t, db, "alice")
	// This alias points to a non-rostered canonical identity and must never be
	// requested, even though both identifiers look like valid GitHub logins.
	if err := db.UpsertDeveloperAlias(t.Context(), "outsider-private-login", "nonroster", "test"); err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	day := fixedNow.UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login := r.URL.Query().Get("user")
		calls[login]++
		if login != "" && login != "alice" {
			t.Errorf("non-roster request: %q", login)
			w.WriteHeader(403)
			return
		}
		if login == "alice" {
			// netAmount is zero while pooled credits remain. Both SKUs aggregate to
			// one gross row; products sharing a model remain separate keys.
			_, _ = w.Write([]byte(`{"timePeriod":{"year":2026,"month":10,"day":19},"organization":"acme","user":"alice","usageItems":[{"product":"copilot","sku":"one","model":"vendor-model","grossAmount":1.25,"netAmount":0},{"product":"copilot","sku":"two","model":"vendor-model","grossAmount":2.5,"netAmount":0},{"product":"agents","model":"vendor-model","grossAmount":0.0000015,"netAmount":0}]}`))
			return
		}
		rep := report(day, item("vendor-model", "copilot", 8), item("vendor-model", "agents", 0.000003))
		// Unknown organization metadata must neither expand the request roster nor
		// appear in stored fields, keys, or logs.
		raw, _ := json.Marshal(rep)
		raw = append(raw[:len(raw)-1], []byte(`,"users":[{"login":"outsider-private-login"}]}`)...)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	var logs bytes.Buffer
	m := &metricSink{}
	p := testPoller(t, db, srv, &logs, m)
	p.runPass(t.Context(), ingester.Store(db))
	if m.ok != 1 || m.failed != 0 {
		t.Fatalf("poll outcomes %+v logs=%s", m, logs.String())
	}
	got := totals(t, db)
	for k, want := range map[spendKey]int64{{"alice", "vendor-model", "copilot"}: 3750000, {"unattributed", "vendor-model", "copilot"}: 4250000, {"alice", "vendor-model", "agents"}: 2, {"unattributed", "vendor-model", "agents"}: 1} {
		if got[k] != want {
			t.Errorf("%+v=%d want=%d", k, got[k], want)
		}
	}
	if len(got) != 4 || calls["alice"] != 1 || calls[""] != 1 || len(calls) != 2 {
		t.Fatalf("keys=%v calls=%v", got, calls)
	}
	before := stored(t, db)
	for _, e := range before {
		if e.IssueID != collector.UnattributedIssueID || e.Fidelity != collector.FidelityDaily || e.PricingBasis != store.PricingCallerAuthoritative || e.Host != store.HostUnknown || e.BillingMode != store.BillingPerToken || !e.Timestamp.Equal(day) || e.InputTok != 0 || e.OutputTok != 0 || e.CacheRead != 0 || e.CacheWrite5m != 0 || e.CacheWrite1h != 0 || e.SessionID != "" || e.PRNumber != nil || e.Repo != "unqualified" {
			t.Fatalf("row contract: %+v", e)
		}
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "outsider-private-login") || strings.Contains(e.IdempotencyKey, "outsider-private-login") {
			t.Fatal("non-roster login persisted")
		}
	}
	p.runPass(t.Context(), ingester.Store(db))
	if len(stored(t, db)) != len(before) || m.events != 4 {
		t.Fatal("repeat poll not idempotent")
	}
	// A token-derived control must actually reprice, so a no-op Reprice
	// implementation cannot make the protection assertion pass vacuously.
	if err := db.InsertTokenEvent(t.Context(), store.TokenEvent{Developer: "control", Model: "claude-sonnet-4", InputTok: 1000, CostMicro: 42_000_000, Source: collector.SourceJSONL, PriceVersion: 1, IdempotencyKey: "reprice-control", Timestamp: day}); err != nil {
		t.Fatal(err)
	}
	res, err := db.Reprice(t.Context(), store.RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExcludedRowCount != 4 || res.ChangedRowCount != 1 {
		t.Fatalf("reprice result %+v", res)
	}
	after := stored(t, db)
	for i, e := range after[:len(before)] {
		if e.CostMicro != before[i].CostMicro {
			t.Fatal("reprice changed imported gross")
		}
	}
	if strings.Contains(logs.String(), "outsider-private-login") || strings.Contains(logs.String(), secretToken) {
		t.Fatal("privacy leak in logs")
	}
}

func TestCopilotOrgRemainderClampCounting(t *testing.T) {
	db := testDB(t)
	roster(t, db, "alice")
	day := fixedNow.AddDate(0, 0, -2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("user") != "" {
			writeUserReport(t, w, r, report(day, item("m", "copilot", 5), item("missing-org-model", "copilot", 2)))
			return
		}
		writeUserReport(t, w, r, report(day, item("m", "copilot", 3)))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	m := &metricSink{}
	p := testPoller(t, db, srv, &logs, m)
	p.runPass(t.Context(), ingester.Store(db))
	if m.clamps != 2 || strings.Count(logs.String(), "spend clamped to zero") != 1 {
		t.Fatalf("clamps=%d logs=%s", m.clamps, logs.String())
	}
	got := totals(t, db)
	if got[spendKey{"alice", "m", "copilot"}] != 5000000 || len(stored(t, db)) != 2 {
		t.Fatalf("mapped spend lost or negative remainder stored: %v", got)
	}
}

func TestCopilotNegativeUserClampedBeforeRemainder(t *testing.T) {
	db := testDB(t)
	roster(t, db, "alice")
	day := fixedNow.AddDate(0, 0, -2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gross := 5.0
		if r.URL.Query().Get("user") != "" {
			gross = -1
		}
		writeUserReport(t, w, r, report(day, item("m", "copilot", gross)))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	m := &metricSink{}
	p := testPoller(t, db, srv, &logs, m)
	p.runPass(t.Context(), ingester.Store(db))
	got := totals(t, db)
	if got[spendKey{"alice", "m", "copilot"}] != 0 || got[spendKey{"unattributed", "m", "copilot"}] != 5_000_000 || m.clamps != 1 || m.ok != 1 {
		t.Fatalf("negative user inflated remainder: totals=%v metrics=%+v logs=%s", got, m, logs.String())
	}
}

func TestCopilotFailedDayPreservesStoredSpend(t *testing.T) {
	for _, mode := range []string{"TokenExchange404", "OrgAndUsers404"} {
		t.Run(mode, func(t *testing.T) {
			db := testDB(t)
			roster(t, db, "alice")
			day := fixedNow.AddDate(0, 0, -2)
			fail := false
			exchanges, userCalls, orgCalls := 0, 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					exchanges++
					if fail {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"token": "installation-token", "expires_at": fixedNow.Add(time.Hour)})
					return
				}
				gross := 5.0
				if r.URL.Query().Get("user") != "" {
					userCalls++
					gross = 2
				} else {
					orgCalls++
				}
				if fail {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				writeUserReport(t, w, r, report(day, item("m", "copilot", gross)))
			}))
			defer srv.Close()
			var logs bytes.Buffer
			m := &metricSink{}
			p := testPoller(t, db, srv, &logs, m)
			if mode == "TokenExchange404" {
				_, keyPEM := rsaFixture(t)
				p.cfg.Client = mustClient(t, ClientConfig{Org: "acme", BaseURL: srv.URL, AppID: "42", InstallationID: "123", PrivateKey: keyPEM, Now: func() time.Time { return fixedNow }})
			}
			settled := 0
			p.cfg.Settled = func(context.Context, time.Time, time.Time) { settled++ }
			p.runPass(t.Context(), ingester.Store(db))
			before := stored(t, db)
			if len(before) != 2 || m.ok != 1 || settled != 1 {
				t.Fatalf("initial spend missing: rows=%v metrics=%+v settled=%d", before, m, settled)
			}
			fail = true
			p.cfg.Client.expires = fixedNow // Force another installation-token exchange.
			exchanges, userCalls, orgCalls = 0, 0, 0
			p.runPass(t.Context(), ingester.Store(db))
			if after := stored(t, db); !reflect.DeepEqual(after, before) {
				t.Fatalf("failed day changed stored rows: before=%v after=%v", before, after)
			}
			if m.failed != 1 || m.skips != 1 || settled != 1 {
				t.Fatalf("failed day not skipped: metrics=%+v settled=%d logs=%s", m, settled, logs.String())
			}
			if mode == "TokenExchange404" && (exchanges != 1 || userCalls != 0 || orgCalls != 0) {
				t.Fatalf("token failure reached usage endpoint: exchanges=%d users=%d org=%d", exchanges, userCalls, orgCalls)
			}
			if mode == "OrgAndUsers404" && (userCalls != 1 || orgCalls != 1) {
				t.Fatalf("missing usage 404s: users=%d org=%d", userCalls, orgCalls)
			}
		})
	}
}

func TestCopilotSkipRemainderPreservesPriorTotal(t *testing.T) {
	for _, mode := range []string{"requester", "403", "invalid-items", "wrong-period", "wrong-org", "filtered-org", "null-items"} {
		t.Run(mode, func(t *testing.T) {
			db := testDB(t)
			roster(t, db, "alice")
			day := fixedNow.AddDate(0, 0, -2)
			bad := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("user") != "" {
					v := 2.0
					if bad {
						v = 3
					}
					writeUserReport(t, w, r, report(day, item("m", "copilot", v)))
					return
				}
				rep := report(day, item("m", "copilot", 7))
				if bad {
					switch mode {
					case "requester":
						rep.User = "outsider-private-login"
					case "403":
						w.WriteHeader(403)
						return
					case "invalid-items":
						rep.UsageItems = []usageItem{{Model: "m", Product: "copilot"}}
					case "wrong-period":
						rep.TimePeriod.Day++
					case "wrong-org":
						rep.Organization = "wrong"
					case "filtered-org":
						rep.Product = "copilot"
					case "null-items":
						rep.UsageItems = nil
					}
				}
				writeUserReport(t, w, r, rep)
			}))
			defer srv.Close()
			var logs bytes.Buffer
			m := &metricSink{}
			p := testPoller(t, db, srv, &logs, m)
			p.runPass(t.Context(), ingester.Store(db))
			bad = true
			p.runPass(t.Context(), ingester.Store(db))
			got := totals(t, db)
			if got[spendKey{"alice", "m", "copilot"}] != 2000000 || got[spendKey{"unattributed", "m", "copilot"}] != 5000000 || len(stored(t, db)) != 2 || m.skips != 1 || m.failed != 1 || !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "remainder skipped") {
				t.Fatalf("skip not visible or prior total lost: %v metrics=%+v logs=%s", got, m, logs.String())
			}
			if strings.Contains(logs.String(), "outsider-private-login") {
				t.Fatal("requester leaked")
			}
		})
	}
}

func TestCopilotDayWideReconcileUpDownAndDisappearedProduct(t *testing.T) {
	db := testDB(t)
	roster(t, db, "alice")
	day := fixedNow.AddDate(0, 0, -2)
	value := 2.0
	product := "copilot"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeUserReport(t, w, r, report(day, item("m", product, value)))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	m := &metricSink{}
	p := testPoller(t, db, srv, &logs, m)
	for _, v := range []float64{2, 4, 1, 1} {
		value = v
		p.runPass(t.Context(), ingester.Store(db))
	}
	if len(stored(t, db)) != 3 || totals(t, db)[spendKey{"alice", "m", "copilot"}] != 1000000 {
		t.Fatalf("revisions not reconciled: %+v", stored(t, db))
	}
	product = "agents"
	p.runPass(t.Context(), ingester.Store(db))
	got := totals(t, db)
	if got[spendKey{"alice", "m", "copilot"}] != 0 || got[spendKey{"alice", "m", "agents"}] != 1000000 {
		t.Fatalf("vanished product not subtracted: %v", got)
	}
	rows := stored(t, db)
	negative := 0
	for _, e := range rows {
		if e.CostMicro < 0 {
			negative++
		}
	}
	if negative != 2 {
		t.Fatalf("negative deltas=%d want downward revision + vanished key", negative)
	}
	// New Poller instance reads durable sequences rather than starting over.
	p = testPoller(t, db, srv, &logs, m)
	p.runPass(t.Context(), ingester.Store(db))
	if len(stored(t, db)) != len(rows) {
		t.Fatal("restart duplicated deltas")
	}
}

func TestCopilotAliasMoveSubtractsUnattributedDayWide(t *testing.T) {
	db := testDB(t)
	roster(t, db, "alice")
	day := fixedNow.AddDate(0, 0, -2)
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login := r.URL.Query().Get("user")
		calls[login]++
		switch login {
		case "":
			writeUserReport(t, w, r, report(day, item("m", "copilot", 8)))
		case "alice":
			writeUserReport(t, w, r, report(day))
		case "alice-gh":
			writeUserReport(t, w, r, report(day, item("m", "copilot", 6)))
		default:
			t.Errorf("unexpected user %s", login)
		}
	}))
	defer srv.Close()
	var logs bytes.Buffer
	m := &metricSink{}
	p := testPoller(t, db, srv, &logs, m)
	p.runPass(t.Context(), ingester.Store(db))
	if calls["alice-gh"] != 0 || totals(t, db)[spendKey{"unattributed", "m", "copilot"}] != 8000000 {
		t.Fatal("unmapped alias was requested")
	}
	if err := db.UpsertDeveloperAlias(t.Context(), "alice-gh", "alice", "test"); err != nil {
		t.Fatal(err)
	}
	p.runPass(t.Context(), ingester.Store(db))
	got := totals(t, db)
	if got[spendKey{"alice", "m", "copilot"}] != 6000000 || got[spendKey{"unattributed", "m", "copilot"}] != 2000000 {
		t.Fatalf("alias move totals %v", got)
	}
	negative := false
	for _, e := range stored(t, db) {
		if e.Developer == "unattributed" && e.CostMicro == -6000000 {
			negative = true
		}
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "alice-gh") {
			t.Fatal("alias persisted instead of canonical")
		}
	}
	if !negative {
		t.Fatal("missing negative remainder delta")
	}
}

func TestCopilotNeverBelowZeroClamp(t *testing.T) {
	db := testDB(t)
	day := fixedNow.UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	k := spendKey{"alice", "m", "copilot"}
	ev := collector.TokenEvent{Developer: k.developer, IssueID: "unattributed", Model: k.model, Source: collector.SourceCopilotBilling, Fidelity: collector.FidelityDaily, CostMicro: 1000000, IdempotencyKey: deltaKey("acme", day, k, 1), Timestamp: day}
	if err := ingester.Store(db).Ingest(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(PollerConfig{Store: db, Client: mustClient(t, ClientConfig{Org: "acme", Token: secretToken})})
	deltas, clamps, err := p.reconcileDay(t.Context(), day, map[spendKey]float64{k: -4}, true)
	if err != nil || clamps != 1 || len(deltas) != 1 || deltas[0].CostMicro != -1000000 {
		t.Fatalf("clamp failed: %v %d %v", deltas, clamps, err)
	}
	if err := ingester.Store(db).Ingest(t.Context(), deltas[0]); err != nil {
		t.Fatal(err)
	}
	if got := totals(t, db)[k]; got != 0 {
		t.Fatalf("net below zero: %d", got)
	}
	// Recovery from an invalid negative stored baseline is also counted.
	ev.CostMicro = -500000
	ev.IdempotencyKey = deltaKey("acme", day, k, 3)
	if err := ingester.Store(db).Ingest(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	deltas, clamps, err = p.reconcileDay(t.Context(), day, nil, true)
	if err != nil || clamps != 1 || len(deltas) != 1 || deltas[0].CostMicro != 500000 {
		t.Fatalf("negative baseline not repaired: %v %d %v", deltas, clamps, err)
	}
}

func TestCopilotForbiddenLoudAndPartialUsersNeverReconcile(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			db := testDB(t)
			roster(t, db, "alice")
			roster(t, db, "bob")
			day := fixedNow.AddDate(0, 0, -2)
			fail := false
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if fail && r.URL.Query().Get("user") == "bob" {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(secretToken + "outsider-private-login"))
					return
				}
				writeUserReport(t, w, r, report(day, item("m", "copilot", 1)))
			}))
			defer srv.Close()
			var logs bytes.Buffer
			m := &metricSink{}
			p := testPoller(t, db, srv, &logs, m)
			p.runPass(t.Context(), ingester.Store(db))
			before := len(stored(t, db))
			fail = true
			calls = 0
			p.runPass(t.Context(), ingester.Store(db))
			if len(stored(t, db)) != before || calls != 2 || m.failed != 1 || len(m.statuses) != 1 || m.statuses[0] != status || !strings.Contains(logs.String(), permissionHint) || !strings.Contains(logs.String(), "level=ERROR") {
				t.Fatalf("HTTP failure not loud/atomic: calls=%d metrics=%+v logs=%s", calls, m, logs.String())
			}
			if strings.Contains(logs.String(), secretToken) || strings.Contains(logs.String(), "outsider-private-login") || strings.Contains(logs.String(), "user=") {
				t.Fatal("HTTP error leaked response or query")
			}
		})
	}
}

func TestCopilotSettledUTCWindowAndCollect(t *testing.T) {
	db := testDB(t)
	var days []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		day, err := time.Parse("2006-1-2", r.URL.Query().Get("year")+"-"+r.URL.Query().Get("month")+"-"+r.URL.Query().Get("day"))
		if err != nil {
			t.Error(err)
		}
		days = append(days, day.Format("2006-01-02"))
		writeUserReport(t, w, r, report(day, item("m", "copilot", 1)))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	m := &metricSink{}
	p := testPoller(t, db, srv, &logs, m)
	p.cfg.LookbackDays = 4
	p.cfg.Now = func() time.Time { return time.Date(2026, 10, 20, 23, 30, 0, 0, time.FixedZone("local", -4*3600)) }
	events, err := p.Collect(t.Context(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(days, ",") != "2026-10-17,2026-10-18,2026-10-19" || len(events) != 3 || len(stored(t, db)) != 0 {
		t.Fatalf("settlement window days=%v events=%d", days, len(events))
	}
	for _, e := range events {
		if e.Timestamp.Hour() != 0 || e.Timestamp.Location() != time.UTC {
			t.Fatalf("not UTC midnight: %v", e.Timestamp)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Run(ctx, time.Time{}, ingester.Store(db)); err != nil {
		t.Fatal(err)
	}
	if err := p.Run(t.Context(), time.Time{}, nil); err == nil {
		t.Fatal("nil ingester accepted")
	}
}

// pageStore forces the existing export pagination path while retaining the
// real SQL cursor and source rows, including a different producer's row.
type pageStore struct{ *store.DB }

func (s pageStore) ListTokenEvents(ctx context.Context, since, until time.Time, after store.PageCursor, _ int) ([]store.TokenEvent, bool, error) {
	return s.DB.ListTokenEvents(ctx, since, until, after, 1)
}

type failAfterIngester struct {
	next      collector.Ingester
	remaining int
}

func (f *failAfterIngester) Ingest(ctx context.Context, e collector.TokenEvent) error {
	if f.remaining == 0 {
		return context.DeadlineExceeded
	}
	f.remaining--
	return f.next.Ingest(ctx, e)
}

func TestCopilotPartialIngestRestartAndPaginatedSourceIsolation(t *testing.T) {
	db := testDB(t)
	roster(t, db, "alice")
	day := fixedNow.UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	if err := db.InsertTokenEvent(t.Context(), store.TokenEvent{Developer: "alice", Source: collector.SourceOpenAIUsage, Model: "m", CostMicro: 9000000, IdempotencyKey: "other-source", Timestamp: day}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeUserReport(t, w, r, report(day, item("m", "copilot", 3), item("m", "agents", 2)))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	m := &metricSink{}
	p := testPoller(t, db, srv, &logs, m)
	p.cfg.Store = pageStore{db}
	err := p.pollOnce(t.Context(), &failAfterIngester{next: ingester.Store(db), remaining: 1})
	if err == nil || len(stored(t, db)) != 2 {
		t.Fatalf("failure injection did not fire: %v rows=%d", err, len(stored(t, db)))
	}
	p = testPoller(t, db, srv, &logs, m)
	p.cfg.Store = pageStore{db}
	p.runPass(t.Context(), ingester.Store(db))
	got := totals(t, db)
	if got[spendKey{"alice", "m", "copilot"}] != 3000000 || got[spendKey{"alice", "m", "agents"}] != 2000000 || len(stored(t, db)) != 3 {
		t.Fatalf("partial restart/source isolation failed: %v", got)
	}
	p.runPass(t.Context(), ingester.Store(db))
	if len(stored(t, db)) != 3 {
		t.Fatal("repeat after partial write duplicated rows")
	}
}

func TestCopilotOversizedDeltaFailsBeforeStoreCanTruncate(t *testing.T) {
	db := testDB(t)
	p := NewPoller(PollerConfig{Store: db, Client: mustClient(t, ClientConfig{Org: "acme", Token: secretToken})})
	day := fixedNow.UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	events, _, err := p.reconcileDay(t.Context(), day, map[spendKey]float64{{"alice", "m", "copilot"}: 10001}, true)
	if err == nil || len(events) != 0 || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("oversized spend silently clamped: events=%v err=%v", events, err)
	}
}

func TestCopilotOversizedKeyPreservesCompliantKeyWithoutSettling(t *testing.T) {
	db := testDB(t)
	roster(t, db, "alice")
	roster(t, db, "bob")
	day := fixedNow.AddDate(0, 0, -2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gross := 10006.0
		switch r.URL.Query().Get("user") {
		case "alice":
			gross = 10001
		case "bob":
			gross = 5
		}
		writeUserReport(t, w, r, report(day, item("m", "copilot", gross)))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	m := &metricSink{}
	p := testPoller(t, db, srv, &logs, m)
	settled := 0
	p.cfg.Settled = func(context.Context, time.Time, time.Time) { settled++ }
	p.runPass(t.Context(), ingester.Store(db))
	rows := stored(t, db)
	if len(rows) != 1 || rows[0].Developer != "bob" || rows[0].CostMicro != 5_000_000 || settled != 0 || m.failed != 1 || m.ok != 0 || m.events != 1 || !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "ceiling") {
		t.Fatalf("oversized key blocked compliant capture or settled day: rows=%v settled=%d metrics=%+v logs=%s", rows, settled, m, logs.String())
	}
}

// A filter the server ignores must never attribute the organization total to a user.
func TestCopilotUserEchoRequiredBeforeReconcile(t *testing.T) {
	for _, echo := range []string{"", "other-private-login"} {
		name := "missing"
		if echo != "" {
			name = "different"
		}
		t.Run(name, func(t *testing.T) {
			db := testDB(t)
			roster(t, db, "alice")
			day := fixedNow.AddDate(0, 0, -2)
			bad := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rep := report(day, item("m", "copilot", 7))
				rep.User = r.URL.Query().Get("user")
				if bad && rep.User != "" {
					rep.User = echo
					rep.UsageItems = []usageItem{item("m", "copilot", 9)}
				}
				writeReport(t, w, rep)
			}))
			defer srv.Close()
			var logs bytes.Buffer
			m := &metricSink{}
			p := testPoller(t, db, srv, &logs, m)
			p.runPass(t.Context(), ingester.Store(db))
			before := len(stored(t, db))
			bad = true
			logs.Reset()
			p.runPass(t.Context(), ingester.Store(db))
			if before == 0 || len(stored(t, db)) != before || m.failed != 1 || len(m.statuses) != 1 || m.statuses[0] != 200 || !strings.Contains(logs.String(), "level=ERROR") {
				t.Fatalf("unusable user response reconciled: rows=%d before=%d metrics=%+v logs=%s", len(stored(t, db)), before, m, logs.String())
			}
			if strings.Contains(logs.String(), "alice") || strings.Contains(logs.String(), "other-private-login") {
				t.Fatal("login leaked")
			}
		})
	}
}

func TestCopilotPerUser404ReconcilesOrgRemainder(t *testing.T) {
	db := testDB(t)
	roster(t, db, "alice")
	roster(t, db, "bob")
	day := fixedNow.AddDate(0, 0, -2)
	missing := false
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if missing && r.URL.Query().Get("user") == "bob" {
			w.WriteHeader(404)
			return
		}
		value := 1.0
		if r.URL.Query().Get("user") == "" {
			value = 5
		}
		writeUserReport(t, w, r, report(day, item("m", "copilot", value)))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	m := &metricSink{}
	p := testPoller(t, db, srv, &logs, m)
	p.runPass(t.Context(), ingester.Store(db))
	missing = true
	calls = 0
	p.runPass(t.Context(), ingester.Store(db))
	got := totals(t, db)
	if calls != 3 || m.failed != 0 || m.ok != 2 || len(m.statuses) != 1 || m.statuses[0] != 404 || got[spendKey{"alice", "m", "copilot"}] != 1_000_000 || got[spendKey{"bob", "m", "copilot"}] != 0 || got[spendKey{"unattributed", "m", "copilot"}] != 4_000_000 {
		t.Fatalf("404 blocked reconciliation: totals=%v calls=%d metrics=%+v logs=%s", got, calls, m, logs.String())
	}
	if strings.Contains(logs.String(), permissionHint) {
		t.Fatal("non-member 404 logged permission hint")
	}
}

func TestCopilotSettledOnlyAfterCompleteDayReconciled(t *testing.T) {
	for _, mode := range []string{"success", "empty", "user-echo", "user-403", "org-403", "invalid-org", "ingest", "reconcile", "collect"} {
		t.Run(mode, func(t *testing.T) {
			db := testDB(t)
			roster(t, db, "alice")
			day := fixedNow.UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				login := r.URL.Query().Get("user")
				if mode == "user-403" && login != "" || mode == "org-403" && login == "" {
					w.WriteHeader(403)
					return
				}
				value := 1.0
				if mode == "reconcile" {
					value = 10001
				}
				rep := report(day, item("m", "copilot", value))
				rep.User = login
				if mode == "empty" {
					rep.UsageItems = []usageItem{}
				}
				if mode == "user-echo" && login != "" {
					rep.User = ""
				}
				if mode == "invalid-org" && login == "" {
					rep.UsageItems = nil
				}
				writeReport(t, w, rep)
			}))
			defer srv.Close()
			var logs bytes.Buffer
			p := testPoller(t, db, srv, &logs, &metricSink{})
			var spans [][2]time.Time
			p.cfg.Settled = func(_ context.Context, from, through time.Time) {
				if mode == "success" && len(stored(t, db)) != 1 {
					t.Fatal("settled before ingest")
				}
				spans = append(spans, [2]time.Time{from, through})
			}
			switch mode {
			case "collect":
				_, _ = p.Collect(t.Context(), time.Time{})
			case "ingest":
				_ = p.pollOnce(t.Context(), &failAfterIngester{next: ingester.Store(db), remaining: 0})
			default:
				p.runPass(t.Context(), ingester.Store(db))
			}
			want := mode == "success" || mode == "empty"
			if want && (len(spans) != 1 || spans[0] != [2]time.Time{day, day.AddDate(0, 0, 1)}) || !want && len(spans) != 0 {
				t.Fatalf("mode=%s settled=%v logs=%s", mode, spans, logs.String())
			}
		})
	}
}

func TestCopilotOrganizationChangeHasIndependentDeltaBaseline(t *testing.T) {
	db := testDB(t)
	roster(t, db, "alice")
	day := fixedNow.AddDate(0, 0, -2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep := report(day, item("m", "copilot", 2))
		if strings.Contains(r.URL.Path, "/another-org/") {
			rep.Organization = "another-org"
		}
		writeUserReport(t, w, r, rep)
	}))
	defer srv.Close()
	var logs bytes.Buffer
	p := testPoller(t, db, srv, &logs, &metricSink{})
	p.runPass(t.Context(), ingester.Store(db))
	p.cfg.Client.cfg.Org = "another-org"
	p.runPass(t.Context(), ingester.Store(db))
	rows := stored(t, db)
	if len(rows) != 2 || rows[0].CostMicro != 2_000_000 || rows[1].CostMicro != 2_000_000 || rows[0].IdempotencyKey == rows[1].IdempotencyKey {
		t.Fatalf("org change reused baseline: %+v logs=%s", rows, logs.String())
	}
	p.runPass(t.Context(), ingester.Store(db))
	if len(stored(t, db)) != 2 {
		t.Fatal("org-specific restart duplicated rows")
	}
}
