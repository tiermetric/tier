package openaiusage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// Persist each successful event immediately, like the serve-time ingester.
// Failing the second write leaves a genuinely partial day in SQLite.
type retryIngester struct {
	db       *store.DB
	attempts []collector.TokenEvent
	failAt   int
	cancel   context.CancelFunc
}

func (i *retryIngester) Ingest(ctx context.Context, ev collector.TokenEvent) error {
	i.attempts = append(i.attempts, ev)
	if i.failAt == len(i.attempts) {
		if i.cancel != nil {
			i.cancel()
			return ctx.Err()
		}
		return errors.New("injected second-model write failure")
	}
	return i.db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: ev.Developer, IssueID: ev.IssueID, Model: ev.Model,
		InputTok: ev.InputTok, OutputTok: ev.OutputTok, CacheRead: ev.CacheRead,
		CacheWrite5m: ev.CacheWrite5m, CacheWrite1h: ev.CacheWrite1h,
		CostMicro: ev.CostMicro, Source: ev.Source, Fidelity: ev.Fidelity,
		IdempotencyKey: ev.IdempotencyKey, BillingMode: ev.BillingMode, Timestamp: ev.Timestamp,
	})
}

func retryPoller(t *testing.T, models ...string) (*Poller, *retryIngester) {
	t.Helper()
	var results []usageResult
	for _, model := range models {
		results = append(results, modelResult(model, 1000, 100, 200))
	}
	report := mustJSON(t, usageReport{Data: []usageBucket{dayBucket("2026-06-15", results...)}})
	costs := mustJSON(t, costReport{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, usagePath) {
			_, _ = w.Write(report)
		} else {
			_, _ = w.Write(costs)
		}
	}))
	t.Cleanup(srv.Close)
	db, err := store.Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := NewPoller(PollerConfig{
		Client: newTestClient(t, srv, nil), Store: db, Org: "acme",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return fixedNow },
	})
	return p, &retryIngester{db: db}
}

func retryRows(t *testing.T, db *store.DB) []store.TokenEvent {
	t.Helper()
	day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	rows, _, err := db.ListTokenEvents(context.Background(), day, day.Add(48*time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var pollerRows []store.TokenEvent
	for _, row := range rows {
		if row.Source == collector.SourceOpenAIUsage {
			pollerRows = append(pollerRows, row)
		}
	}
	return pollerRows
}

// RED A: one committed model must not make a failed/cancelled day complete.
func TestPollerRetryCompletesPartialDay(t *testing.T) {
	for _, failure := range []string{"write error", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			p, ing := retryPoller(t, "gpt-4o", "gpt-4.1")
			// New-rule days must retain that rule on retry: these captures
			// belong to subscriptions/third-party invoices, not this account.
			for _, model := range []string{"gpt-4o", "gpt-4.1"} {
				for _, capture := range []struct{ host, mode string }{
					{"api." + provider + ".com", store.BillingSubscription},
					{"openrouter.ai", store.BillingPerToken},
				} {
					if err := ing.db.InsertTokenEvent(context.Background(), store.TokenEvent{
						Developer: "alice", IssueID: "1", Model: model, InputTok: 100, OutputTok: 20,
						Source: "proxy", Host: capture.host, BillingMode: capture.mode,
						Timestamp: time.Date(2026, 6, 15, 1, 0, 0, 0, time.UTC),
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ing.failAt = 2
			if failure == "cancellation" {
				ing.cancel = cancel
			}
			if _, err := p.pollAt(ctx, ing, fixedNow); err == nil {
				t.Fatal("first pass succeeded, want injected failure")
			}
			before := retryRows(t, ing.db)
			if len(before) != 1 || len(ing.attempts) != 2 {
				t.Fatalf("partial pass stored %d rows after %d attempts, want 1 after 2", len(before), len(ing.attempts))
			}
			missing := ing.attempts[1]
			ing.failAt = 0
			ing.cancel = nil
			ing.attempts = nil
			if _, err := p.pollAt(context.Background(), ing, fixedNow); err != nil {
				t.Fatal(err)
			}
			after := retryRows(t, ing.db)
			if len(after) != 2 {
				t.Errorf("retry stored %d models, want 2: missing model %s was skipped", len(after), missing.Model)
			}
			if len(ing.attempts) != 1 || !reflect.DeepEqual(ing.attempts[0], missing) {
				t.Errorf("retry attempted %d events, want only the unchanged missing-model remainder", len(ing.attempts))
			}
			for _, row := range after {
				if row.Model == before[0].Model && !reflect.DeepEqual(row, before[0]) {
					t.Error("retry rewrote the committed model's tokens/cost")
				}
			}
		})
	}
}

// RED B: a stored priced model must neither hide unpriced usage nor prevent
// recovery on the next pass after the operator updates the price table.
func TestPollerRetryRecoversMixedDayAfterPriceUpdate(t *testing.T) {
	// Price-table changes are serial, matching restart-time --prices loading.
	original, err := os.ReadFile("../../store/prices.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pricesPath := filepath.Join(t.TempDir(), "prices.yaml")
	restorePath := filepath.Join(t.TempDir(), "original-prices.yaml")
	if err := os.WriteFile(restorePath, original, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.LoadPriceTable(restorePath); err != nil {
			t.Errorf("restore prices: %v", err)
		}
	})
	p, ing := retryPoller(t, "gpt-4o", "gpt-retry-unpriced")
	settled := 0
	p.settled = func(context.Context, time.Time, time.Time) { settled++ }
	p.runPass(context.Background(), ing)
	before := retryRows(t, ing.db)
	if len(before) != 1 || settled != 0 {
		t.Fatalf("mixed first pass: rows=%d settled=%d, want 1/0", len(before), settled)
	}
	ing.attempts = nil
	unpriced, err := p.pollAt(context.Background(), ing, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !unpriced {
		t.Error("mixed-day retry unpriced=false, want true while model has no price")
	}
	p.runPass(context.Background(), ing)
	if settled != 0 {
		t.Errorf("mixed-day retry settled %d times before price update, want 0", settled)
	}
	if len(ing.attempts) != 0 {
		t.Error("mixed-day retry re-emitted stored priced model")
	}

	updated := strings.Replace(string(original), "version: 12", "version: 1000012", 1) +
		"\n  gpt-retry-unpriced: { input_per_m: 7.00, output_per_m: 21.00, provider: openai }\n"
	if err := os.WriteFile(pricesPath, []byte(updated), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadPriceTable(pricesPath); err != nil {
		t.Fatal(err)
	}
	if store.ProviderOf("gpt-retry-unpriced") != provider {
		t.Fatal("price update did not resolve missing model")
	}
	settled = 0
	p.runPass(context.Background(), ing)
	after := retryRows(t, ing.db)
	if len(after) != 2 {
		t.Errorf("after price update stored %d models, want 2", len(after))
	}
	if len(ing.attempts) != 1 || ing.attempts[0].Model != "gpt-retry-unpriced" || ing.attempts[0].CostMicro <= 0 {
		t.Errorf("after price update attempted %d events, want one priced gpt-retry-unpriced remainder", len(ing.attempts))
	}
	if settled != 1 {
		t.Errorf("after price update settled=%d, want 1", settled)
	}
	for _, row := range after {
		if row.Model == before[0].Model && !reflect.DeepEqual(row, before[0]) {
			t.Error("price update rewrote stored model")
		}
	}
}
