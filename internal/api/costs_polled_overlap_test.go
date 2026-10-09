package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// newPolledHandler builds a second Handler over db that runs with the given
// polled providers, so one database can be written by an install with a poller
// and one without (the upgrade and re-post cases need both).
func newPolledHandler(db *store.DB, providers ...string) *Handler {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(db, quiet, "", nil, "test", RateLimitConfig{}, WithPolledProviders(providers...))
}

// storedRows returns every token_events row, in insert order.
func storedRows(t *testing.T, db *store.DB) []store.TokenEvent {
	t.Helper()
	rows, _, err := db.ListTokenEvents(context.Background(),
		time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	return rows
}

// errorBody decodes a {"error": "..."} body.
func errorBody(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Error
}

func polledPremise(t *testing.T, model, provider string) {
	t.Helper()
	if got := store.ProviderOf(store.NormalizeModel(model)); got != provider {
		t.Fatalf("test premise broken: ProviderOf(%q) = %q, want %q", model, got, provider)
	}
}

// TestPostCosts_PolledProvider_RefusesUndeclaredRows pins #854's refusal: with
// the Anthropic org poller running, an Anthropic row that does not declare
// billed_to is refused 400 whether it carries tokens or only a cost, the body
// carries the one shared remediation, and nothing is stored.
func TestPostCosts_PolledProvider_RefusesUndeclaredRows(t *testing.T) {
	polledPremise(t, "claude-sonnet-4", "anthropic")
	_, db := newTestHandler(t)
	h := newPolledHandler(db, "anthropic")

	cases := map[string]map[string]any{
		"tokened": {
			"developer": "alice", "issue_id": "issue-1", "model": "claude-sonnet-4",
			"input_tokens": 1000, "output_tokens": 500, "cost_usd": 0.0105,
		},
		"cost-only": {
			"developer": "alice", "issue_id": "issue-1", "model": "claude-sonnet-4",
			"cost_usd": 250.0,
		},
		// The dated id the provider API returns: refused only because the model is
		// normalised before the provider lookup, as the poller does.
		"dated": {
			"developer": "alice", "issue_id": "issue-1", "model": "claude-sonnet-4-20250514",
			"cost_usd": 3.0,
		},
		"override": {
			"developer": "alice", "issue_id": "issue-1", "model": "claude-sonnet-4",
			"cost_usd": 1.0, "idempotency_key": "k-override",
			"override": true, "override_actor": "finance", "override_reason": "invoice",
		},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (a row for a polled provider without billed_to double counts); body = %s", code, body)
			}
			if msg := errorBody(t, body); !strings.Contains(msg, PolledProviderOverlapRemedy) {
				t.Errorf("400 body does not carry PolledProviderOverlapRemedy: %q", msg)
			}
		})
	}
	if rows := storedRows(t, db); len(rows) != 0 {
		t.Errorf("a refused row was stored: %+v", rows)
	}
}

// TestPostCosts_PolledProvider_BilledToOtherAdmittedAndStored pins the escape:
// the same rows declared billed_to "other" are admitted 201 and the declaration
// is stored on the row, readable through the bulk export (JSON and CSV).
func TestPostCosts_PolledProvider_BilledToOtherAdmittedAndStored(t *testing.T) {
	_, db := newTestHandler(t)
	h := newPolledHandler(db, "anthropic")

	for _, payload := range []map[string]any{
		{"developer": "alice", "issue_id": "issue-1", "model": "claude-sonnet-4",
			"input_tokens": 1000, "cost_usd": 0.003, "billed_to": "other"},
		{"developer": "alice", "issue_id": "issue-2", "model": "claude-sonnet-4",
			"cost_usd": 200.0, "billed_to": "other"},
	} {
		if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 for a declared billed_to=other row; body = %s", code, body)
		}
	}
	rows := storedRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("stored %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.BilledTo != BilledToOther {
			t.Errorf("row %d billed_to = %q, want %q", r.ID, r.BilledTo, BilledToOther)
		}
	}

	rec := doExport(t, h, "/api/v1/events", nil)
	var page eventsExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode export: %v (%s)", err, rec.Body.String())
	}
	if len(page.Events) != 2 || page.Events[0].BilledTo != BilledToOther {
		t.Errorf("JSON export billed_to not carried: %+v", page.Events)
	}
	rec = doExport(t, h, "/api/v1/events", http.Header{"Accept": {"text/csv"}})
	records, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil {
		t.Fatalf("read CSV: %v", err)
	}
	col := slices.Index(records[0], "billed_to")
	if col < 0 || records[1][col] != BilledToOther {
		t.Errorf("CSV export: billed_to column %d in %v, want value %q", col, records[0], BilledToOther)
	}
}

// TestPostCosts_PolledProvider_FollowingTheRemedyIsAdmitted pins that the 400's
// advice works when followed literally: the refused request plus the field and
// value the remedy names is a 201.
func TestPostCosts_PolledProvider_FollowingTheRemedyIsAdmitted(t *testing.T) {
	_, db := newTestHandler(t)
	h := newPolledHandler(db, "anthropic")
	payload := map[string]any{"developer": "alice", "issue_id": "issue-1", "model": "claude-sonnet-4", "cost_usd": 5.0}
	if code, _ := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusBadRequest {
		t.Fatalf("premise: undeclared row status = %d, want 400", code)
	}
	if !strings.Contains(PolledProviderOverlapRemedy, `"billed_to": "`+BilledToOther+`"`) {
		t.Fatalf("remedy does not name the field and value a caller must send: %q", PolledProviderOverlapRemedy)
	}
	payload["billed_to"] = BilledToOther
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusCreated {
		t.Fatalf("following the remedy: status = %d, want 201; body = %s", code, body)
	}
}

// TestPostCosts_NoPoller_Unchanged pins that an install with no poller for the
// row's provider keeps today's 201 and stores no declaration — both for a
// handler with no pollers at all and for one that polls a different provider.
func TestPostCosts_NoPoller_Unchanged(t *testing.T) {
	polledPremise(t, "gpt-4o", "openai")
	plain, db := newTestHandler(t)
	openaiOnly := newPolledHandler(db, "openai")
	for name, h := range map[string]*Handler{"no poller": plain, "openai poller only": openaiOnly} {
		payload := map[string]any{"developer": "bob-" + strings.ReplaceAll(name, " ", "-"), "issue_id": "issue-1",
			"model": "claude-sonnet-4", "input_tokens": 1000, "cost_usd": 0.003}
		if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusCreated {
			t.Errorf("%s: status = %d, want 201 (no Anthropic poller, behaviour unchanged); body = %s", name, code, body)
		}
	}
	for _, r := range storedRows(t, db) {
		if r.BilledTo != "" {
			t.Errorf("row %d billed_to = %q, want empty (undeclared)", r.ID, r.BilledTo)
		}
	}
}

// TestPostCosts_BilledTo_InvalidValueRejected pins that "other" is the only
// accepted value, with or without a poller: a near miss is a 400, never a silent
// undeclared row.
func TestPostCosts_BilledTo_InvalidValueRejected(t *testing.T) {
	plain, db := newTestHandler(t)
	polled := newPolledHandler(db, "anthropic")
	for _, v := range []string{"Other", " other", "org", "anthropic", "self"} {
		for name, h := range map[string]*Handler{"no poller": plain, "polled": polled} {
			payload := map[string]any{"developer": "alice", "issue_id": "issue-1",
				"model": "gpt-4o", "cost_usd": 1.0, "billed_to": v}
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
			if code != http.StatusBadRequest {
				t.Errorf("%s billed_to=%q: status = %d, want 400; body = %s", name, v, code, body)
			}
		}
	}
	if rows := storedRows(t, db); len(rows) != 0 {
		t.Errorf("an invalid billed_to row was stored: %+v", rows)
	}
}

// TestPostCosts_PolledProvider_UnknownModelAdmitted pins the provider match: a
// model the price table does not recognise (ProviderOf "") is admitted, even
// when an empty tag was passed as "polled" — including a spelling of a polled
// model the normaliser does not map, whose usage the poller counted under the
// canonical id (docs/api-compatibility.md says to post canonical ids).
func TestPostCosts_PolledProvider_UnknownModelAdmitted(t *testing.T) {
	polledPremise(t, "totally-unknown-model-854", "")
	_, db := newTestHandler(t)
	h := newPolledHandler(db, "anthropic", "openai", "")
	payload := map[string]any{"developer": "alice", "issue_id": "issue-1",
		"model": "totally-unknown-model-854", "input_tokens": 10, "cost_usd": 1.0}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (an unpriced model is not polled); body = %s", code, body)
	}
}

// TestPostCosts_BilledTo_NotPartOfRepostIdentity pins the re-post contract:
// billed_to is outside the keyed re-post comparison, so a differing declaration
// is neither a 409 nor a rewrite; and on a polled install an undeclared re-post
// of a row stored before the upgrade is refused 400 before the store is asked.
func TestPostCosts_BilledTo_NotPartOfRepostIdentity(t *testing.T) {
	plain, db := newTestHandler(t)
	polled := newPolledHandler(db, "anthropic")
	post := func(h *Handler, key, billedTo string) (int, []byte) {
		p := map[string]any{"developer": "alice", "issue_id": "issue-1", "model": "claude-sonnet-4",
			"input_tokens": 1000, "cost_usd": 0.003, "idempotency_key": key}
		if billedTo != "" {
			p["billed_to"] = billedTo
		}
		return doRequest(t, h, http.MethodPost, "/api/v1/costs", p)
	}
	billedToOf := func(key string) string {
		for _, r := range storedRows(t, db) {
			if r.IdempotencyKey == key {
				return r.BilledTo
			}
		}
		t.Fatalf("no row for key %q", key)
		return ""
	}

	// A row stored undeclared (no poller at the time) ...
	if code, body := post(plain, "k-pre", ""); code != http.StatusCreated {
		t.Fatalf("seed: status = %d; body = %s", code, body)
	}
	// ... re-posted declared on the polled install: idempotent 201, row unchanged.
	if code, body := post(polled, "k-pre", BilledToOther); code != http.StatusCreated {
		t.Errorf("declared re-post of an undeclared row: status = %d, want 201 (billed_to is not identity); body = %s", code, body)
	}
	if got := billedToOf("k-pre"); got != "" {
		t.Errorf("idempotent re-post rewrote billed_to to %q; the stored row must stay as first recorded", got)
	}
	// ... re-posted undeclared on the polled install: the documented retry edge.
	if code, body := post(polled, "k-pre", ""); code != http.StatusBadRequest {
		t.Errorf("undeclared re-post on a polled install: status = %d, want 400; body = %s", code, body)
	}

	// A row stored declared, re-posted undeclared where no poller runs: 201.
	if code, body := post(polled, "k-declared", BilledToOther); code != http.StatusCreated {
		t.Fatalf("seed declared: status = %d; body = %s", code, body)
	}
	if code, body := post(plain, "k-declared", ""); code != http.StatusCreated {
		t.Errorf("undeclared re-post of a declared row, no poller: status = %d, want 201; body = %s", code, body)
	}
	if got := billedToOf("k-declared"); got != BilledToOther {
		t.Errorf("idempotent re-post cleared billed_to to %q; want %q", got, BilledToOther)
	}
	if n := len(storedRows(t, db)); n != 2 {
		t.Errorf("stored %d rows, want 2 (every re-post is a no-op)", n)
	}
}

// TestPostCosts_PolledProvider_OverrideCorrectsExistingRow pins the remedy for a
// stored double count: on a polled install, the audited override of an EXISTING
// keyed undeclared row is admitted without billed_to and corrects it (200, one
// audit row), while an override whose key owns no row — which would insert a
// fresh undeclared row — is refused with the same 400 and writes nothing.
func TestPostCosts_PolledProvider_OverrideCorrectsExistingRow(t *testing.T) {
	plain, db := newTestHandler(t)
	polled := newPolledHandler(db, "anthropic")
	row := func(key string, cost float64) map[string]any {
		return map[string]any{"developer": "alice", "issue_id": "issue-1", "model": "claude-sonnet-4",
			"input_tokens": 1000, "cost_usd": cost, "idempotency_key": key}
	}
	if code, body := doRequest(t, plain, http.MethodPost, "/api/v1/costs", row("k-pre", 12.5)); code != http.StatusCreated {
		t.Fatalf("seed pre-upgrade row: status = %d; body = %s", code, body)
	}

	fix := row("k-pre", 0)
	fix["override"], fix["override_actor"], fix["override_reason"] = true, "finance", "counted by the org poller"
	code, body := doRequest(t, polled, http.MethodPost, "/api/v1/costs", fix)
	if code != http.StatusOK {
		t.Fatalf("override of an existing undeclared row: status = %d, want 200 (the remedy must work); body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 0 {
		t.Errorf("corrected cost_micro = %d, want 0", got)
	}

	fresh := row("k-new", 3)
	fresh["override"], fresh["override_actor"], fresh["override_reason"] = true, "finance", "new row"
	code, body = doRequest(t, polled, http.MethodPost, "/api/v1/costs", fresh)
	if code != http.StatusBadRequest {
		t.Fatalf("override with a new key and no billed_to: status = %d, want 400; body = %s", code, body)
	}
	if msg := errorBody(t, body); !strings.Contains(msg, PolledProviderOverlapRemedy) {
		t.Errorf("400 body does not carry PolledProviderOverlapRemedy: %q", msg)
	}
	if n := len(storedRows(t, db)); n != 1 {
		t.Errorf("stored %d rows, want 1 (the refused override must insert nothing)", n)
	}
	if n, err := db.CostCorrectionAuditCount(context.Background()); err != nil || n != 1 {
		t.Errorf("audit rows = %d (%v), want 1 (the one real correction)", n, err)
	}
}

// TestPostCosts_BothPollersRefuseTheirOwnProviders pins the set built from two
// tags: with both org pollers running, an undeclared row for EITHER provider is
// refused, and each is admitted once declared billed_to "other".
func TestPostCosts_BothPollersRefuseTheirOwnProviders(t *testing.T) {
	polledPremise(t, "claude-sonnet-4", "anthropic")
	polledPremise(t, "gpt-4o", "openai")
	_, db := newTestHandler(t)
	h := newPolledHandler(db, "anthropic", "openai")
	for _, model := range []string{"claude-sonnet-4", "gpt-4o"} {
		payload := map[string]any{"developer": "alice", "issue_id": "issue-1", "model": model, "cost_usd": 2.0}
		if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusBadRequest {
			t.Errorf("%s undeclared: status = %d, want 400 (both providers are polled); body = %s", model, code, body)
		}
		payload["billed_to"] = BilledToOther
		if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusCreated {
			t.Errorf("%s declared other: status = %d, want 201; body = %s", model, code, body)
		}
	}
	if n := len(storedRows(t, db)); n != 2 {
		t.Errorf("stored %d rows, want 2 (the two declared rows)", n)
	}
}
