package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/store"
)

// doRawPostWithHeader posts a raw (unmarshalled) body string so tests can send
// malformed JSON such as trailing objects.
func doRawPostWithHeader(t *testing.T, h *Handler, target, rawBody string, header http.Header) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(rawBody))
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestGetScores_AliasJoinsCostToOutcome proves the join: a cost row under the OS
// username and an outcome under the GitHub login collapse into ONE developer row
// once the alias maps the GitHub login to the OS username (the canonical id).
func TestGetScores_AliasJoinsCostToOutcome(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice.smith", "issue-1", 1.0) // list-price cost under OS username
	seedOutcome(t, db, "asmith-gh", "issue-1", 3, 1)
	if err := db.UpsertDeveloperAlias(context.Background(), "asmith-gh", "alice.smith"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}

	devs := scoresDevs(t, h)
	if len(devs) != 1 {
		t.Fatalf("got %d developer rows, want 1 merged row; devs=%v", len(devs), devs)
	}
	got, ok := devs["alice.smith"]
	if !ok {
		t.Fatalf("canonical developer alice.smith absent; got %v", devs)
	}
	if got.WeightedPoints != 3 {
		t.Errorf("weighted_points = %v, want 3", got.WeightedPoints)
	}
	if got.TotalCostUSD != 1.0 {
		t.Errorf("total_cost_usd = %v, want 1.0", got.TotalCostUSD)
	}
	if got.TIER <= 0 {
		t.Errorf("tier = %v, want > 0", got.TIER)
	}
	// The alias (raw GitHub login) must NOT appear as its own row.
	if _, ok := devs["asmith-gh"]; ok {
		t.Errorf("raw alias asmith-gh leaked as its own row: %v", devs)
	}
}

// TestGetScores_AliasMergesCostRowsAndActualSpend covers two cost identities and
// two actual_spend identities aliased to one canonical: costs sum and actual_spend
// keys merge additively.
func TestGetScores_AliasMergesCostRowsAndActualSpend(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice.laptop", "issue-1", 1.0)
	seedCosts(t, db, "alice.desktop", "issue-2", 2.0)
	ctx := context.Background()
	period := time.Now().UTC().Format("2006-01")
	for _, dev := range []string{"alice.laptop", "alice.desktop"} {
		if err := db.InsertActualSpend(ctx, store.ActualSpend{
			Developer:       dev,
			Period:          period,
			ActualPaidMicro: store.DollarsToMicro(10.0),
			Timestamp:       time.Now().UTC(),
		}); err != nil {
			t.Fatalf("InsertActualSpend: %v", err)
		}
	}
	if err := db.UpsertDeveloperAlias(ctx, "alice.desktop", "alice.laptop"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}

	devs := scoresDevs(t, h)
	if len(devs) != 1 {
		t.Fatalf("got %d developer rows, want 1 merged; devs=%v", len(devs), devs)
	}
	got := devs["alice.laptop"]
	if got.TotalCostUSD != 3.0 {
		t.Errorf("total_cost_usd = %v, want 3.0 (1+2 merged)", got.TotalCostUSD)
	}
	if got.ActualPaidUSD != 20.0 {
		t.Errorf("actual_paid_usd = %v, want 20.0 (10+10 merged)", got.ActualPaidUSD)
	}
}

// newAliasTestHandler builds a handler with a captured logger and a wired
// identity gauge so the unjoined-visibility behavior can be asserted.
func newAliasTestHandler(t *testing.T) (*Handler, *store.DB, *bytes.Buffer, *metrics.Registry) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tier-alias-test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = os.Remove(path)
	})
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h := New(db, logger, "", nil, "test", RateLimitConfig{})
	reg := metrics.NewRegistry()
	g := reg.NewGauge("tier_identity_unjoined", "test gauge", "side")
	h.SetIdentityGauge(g)
	return h, db, &buf, reg
}

// gaugeValue renders the registry and extracts the tier_identity_unjoined value
// for a side.
func gaugeValue(t *testing.T, reg *metrics.Registry, side string) float64 {
	t.Helper()
	var sb bytes.Buffer
	reg.Render(&sb)
	want := `tier_identity_unjoined{side="` + side + `"}`
	for _, line := range strings.Split(sb.String(), "\n") {
		if strings.HasPrefix(line, want+" ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, want+" ")), 64)
			if err != nil {
				t.Fatalf("parse gauge line %q: %v", line, err)
			}
			return v
		}
	}
	return 0
}

// TestGetScores_UnjoinedGaugeAndWarnOnce verifies the gauge counts unjoined
// identities per side and the WARN fires at most once per identifier across two
// consecutive requests.
func TestGetScores_UnjoinedGaugeAndWarnOnce(t *testing.T) {
	h, db, buf, reg := newAliasTestHandler(t)
	// cost-only developer (no outcome) → side=cost unjoined.
	seedCosts(t, db, "cost-only", "issue-1", 1.0)
	// outcome-only developer (no cost) → side=outcome unjoined.
	seedOutcome(t, db, "outcome-only", "issue-2", 3, 1)

	// First request populates the gauge and logs one WARN per side.
	if devs := scoresDevs(t, h); len(devs) != 2 {
		t.Fatalf("got %d developers, want 2; %v", len(devs), devs)
	}
	if v := gaugeValue(t, reg, "cost"); v != 1 {
		t.Errorf("gauge side=cost = %v, want 1", v)
	}
	if v := gaugeValue(t, reg, "outcome"); v != 1 {
		t.Errorf("gauge side=outcome = %v, want 1", v)
	}

	// Second request must NOT re-log the same identifiers (warn-once per process).
	_ = scoresDevs(t, h)
	if n := strings.Count(buf.String(), "developer identity has no join partner"); n != 2 {
		t.Errorf("WARN count = %d, want 2 (once per identifier across two requests); log=\n%s", n, buf.String())
	}
}

// TestDeveloperAliasEndpoints_ValidationAndAuth is the table-driven contract for
// the admin alias API: auth gate, input validation, chain rejection, and the
// happy-path status codes.
func TestDeveloperAliasEndpoints_ValidationAndAuth(t *testing.T) {
	const token = "s3cret-alias-token-1234"

	t.Run("401 without bearer", func(t *testing.T) {
		h, _ := newTestHandlerWithToken(t, token)
		code, _ := doRequest(t, h, http.MethodPost, "/api/v1/developer_alias",
			map[string]any{"alias": "gh", "canonical": "os"})
		if code != http.StatusUnauthorized {
			t.Errorf("POST without bearer: status = %d, want 401", code)
		}
		code, _ = doRequest(t, h, http.MethodGet, "/api/v1/developer_alias", nil)
		if code != http.StatusUnauthorized {
			t.Errorf("GET without bearer: status = %d, want 401", code)
		}
		code, _ = doRequest(t, h, http.MethodDelete, "/api/v1/developer_alias/gh", nil)
		if code != http.StatusUnauthorized {
			t.Errorf("DELETE without bearer: status = %d, want 401", code)
		}
	})

	authHdr := http.Header{"Authorization": {"Bearer " + token}}

	t.Run("post validation", func(t *testing.T) {
		long := strings.Repeat("x", maxIdentifierLen+1)
		cases := []struct {
			name string
			body any
			want int
		}{
			{"happy 201", map[string]any{"alias": "gh", "canonical": "os"}, http.StatusCreated},
			{"missing alias", map[string]any{"canonical": "os"}, http.StatusBadRequest},
			{"missing canonical", map[string]any{"alias": "gh"}, http.StatusBadRequest},
			{"self map", map[string]any{"alias": "same", "canonical": "same"}, http.StatusBadRequest},
			{"oversized alias", map[string]any{"alias": long, "canonical": "os"}, http.StatusBadRequest},
			{"oversized canonical", map[string]any{"alias": "gh2", "canonical": long}, http.StatusBadRequest},
			{"unknown field", map[string]any{"alias": "gh3", "canonical": "os", "extra": 1}, http.StatusBadRequest},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h, _ := newTestHandlerWithToken(t, token)
				code, body := doRequestWithHeader(t, h, http.MethodPost, "/api/v1/developer_alias", tc.body, authHdr)
				if code != tc.want {
					t.Errorf("status = %d, want %d; body = %s", code, tc.want, body)
				}
			})
		}
	})

	t.Run("trailing json rejected", func(t *testing.T) {
		h, _ := newTestHandlerWithToken(t, token)
		code, body := doRawPostWithHeader(t, h, "/api/v1/developer_alias",
			`{"alias":"gh","canonical":"os"}{"alias":"x","canonical":"y"}`, authHdr)
		if code != http.StatusBadRequest {
			t.Errorf("trailing JSON: status = %d, want 400; body = %s", code, body)
		}
	})

	t.Run("chain rejection", func(t *testing.T) {
		h, _ := newTestHandlerWithToken(t, token)
		// a → b established.
		code, body := doRequestWithHeader(t, h, http.MethodPost, "/api/v1/developer_alias",
			map[string]any{"alias": "a", "canonical": "b"}, authHdr)
		if code != http.StatusCreated {
			t.Fatalf("seed a→b: status = %d, body = %s", code, body)
		}
		// b → c would make b both a canonical and an alias (chain) → 400.
		code, body = doRequestWithHeader(t, h, http.MethodPost, "/api/v1/developer_alias",
			map[string]any{"alias": "b", "canonical": "c"}, authHdr)
		if code != http.StatusBadRequest {
			t.Errorf("chain b→c: status = %d, want 400; body = %s", code, body)
		}
		// c → a would make a (already an alias) a canonical → 400.
		code, body = doRequestWithHeader(t, h, http.MethodPost, "/api/v1/developer_alias",
			map[string]any{"alias": "c", "canonical": "a"}, authHdr)
		if code != http.StatusBadRequest {
			t.Errorf("chain c→a: status = %d, want 400; body = %s", code, body)
		}
	})

	t.Run("delete found and not found", func(t *testing.T) {
		h, _ := newTestHandlerWithToken(t, token)
		code, body := doRequestWithHeader(t, h, http.MethodPost, "/api/v1/developer_alias",
			map[string]any{"alias": "gh", "canonical": "os"}, authHdr)
		if code != http.StatusCreated {
			t.Fatalf("seed: status = %d, body = %s", code, body)
		}
		code, _ = doRequestWithHeader(t, h, http.MethodDelete, "/api/v1/developer_alias/gh", nil, authHdr)
		if code != http.StatusNoContent {
			t.Errorf("delete existing: status = %d, want 204", code)
		}
		code, _ = doRequestWithHeader(t, h, http.MethodDelete, "/api/v1/developer_alias/gh", nil, authHdr)
		if code != http.StatusNotFound {
			t.Errorf("delete absent: status = %d, want 404", code)
		}
	})

	t.Run("get list shape", func(t *testing.T) {
		h, _ := newTestHandlerWithToken(t, token)
		for _, kv := range [][2]string{{"gh1", "os1"}, {"gh2", "os2"}} {
			code, body := doRequestWithHeader(t, h, http.MethodPost, "/api/v1/developer_alias",
				map[string]any{"alias": kv[0], "canonical": kv[1]}, authHdr)
			if code != http.StatusCreated {
				t.Fatalf("seed %v: status = %d, body = %s", kv, code, body)
			}
		}
		code, body := doRequestWithHeader(t, h, http.MethodGet, "/api/v1/developer_alias", nil, authHdr)
		if code != http.StatusOK {
			t.Fatalf("GET list: status = %d, body = %s", code, body)
		}
		var resp struct {
			Aliases map[string]string `json:"aliases"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Aliases["gh1"] != "os1" || resp.Aliases["gh2"] != "os2" {
			t.Errorf("aliases = %v, want gh1→os1, gh2→os2", resp.Aliases)
		}
	})
}

// TestGetDeveloperScore_ResolvesAlias proves GET /scores/{developer} resolves the
// path value through the alias map: querying the raw GitHub login returns the
// merged canonical row.
func TestGetDeveloperScore_ResolvesAlias(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice.smith", "issue-1", 2.0)
	seedOutcome(t, db, "asmith-gh", "issue-1", 4, 1)
	if err := db.UpsertDeveloperAlias(context.Background(), "asmith-gh", "alice.smith"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores/asmith-gh", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores/asmith-gh: status = %d, body = %s", code, body)
	}
	var resp developerDetailResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Developer != "alice.smith" {
		t.Errorf("developer = %q, want alice.smith (canonical)", resp.Developer)
	}
	if resp.TotalCostUSD != 2.0 {
		t.Errorf("total_cost_usd = %v, want 2.0", resp.TotalCostUSD)
	}
	if resp.WeightedPoints != 4 {
		t.Errorf("weighted_points = %v, want 4", resp.WeightedPoints)
	}
	if resp.TIER <= 0 {
		t.Errorf("tier = %v, want > 0", resp.TIER)
	}
}

// TestGetScores_DeleteAliasResplitsAndGaugeClears proves the read-side design:
// deleting an alias un-joins the two identities (they split back into two rows),
// and the unjoined gauge tracks the transition (0/0 while joined, 1/1 after the
// split). Positive control that the gauge uses Set (not Add) across reads.
func TestGetScores_DeleteAliasResplitsAndGaugeClears(t *testing.T) {
	h, db, _, reg := newAliasTestHandler(t)
	ctx := context.Background()
	seedCosts(t, db, "os-name", "issue-1", 1.0)
	seedOutcome(t, db, "gh-name", "issue-1", 3, 1)
	if err := db.UpsertDeveloperAlias(ctx, "gh-name", "os-name"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}

	// Joined: one row, both gauge sides 0.
	if devs := scoresDevs(t, h); len(devs) != 1 {
		t.Fatalf("joined: got %d rows, want 1; %v", len(devs), devs)
	}
	if v := gaugeValue(t, reg, "cost"); v != 0 {
		t.Errorf("joined gauge cost = %v, want 0", v)
	}
	if v := gaugeValue(t, reg, "outcome"); v != 0 {
		t.Errorf("joined gauge outcome = %v, want 0", v)
	}

	// Delete the alias → the two identities split back into two rows.
	found, err := db.DeleteDeveloperAlias(ctx, "gh-name")
	if err != nil || !found {
		t.Fatalf("DeleteDeveloperAlias: found=%v err=%v", found, err)
	}
	devs := scoresDevs(t, h)
	if len(devs) != 2 {
		t.Fatalf("after delete: got %d rows, want 2 (split); %v", len(devs), devs)
	}
	if _, ok := devs["os-name"]; !ok {
		t.Errorf("os-name (cost) missing after split: %v", devs)
	}
	if _, ok := devs["gh-name"]; !ok {
		t.Errorf("gh-name (outcome) missing after split: %v", devs)
	}
	if v := gaugeValue(t, reg, "cost"); v != 1 {
		t.Errorf("split gauge cost = %v, want 1", v)
	}
	if v := gaugeValue(t, reg, "outcome"); v != 1 {
		t.Errorf("split gauge outcome = %v, want 1", v)
	}
}

// TestGetScores_UpdateAliasRejoins proves that re-pointing an existing alias to
// the correct canonical retroactively re-joins history: an alias initially
// pointing at the wrong canonical leaves cost and outcome split, and updating it
// to the cost-side identity merges them into one row.
func TestGetScores_UpdateAliasRejoins(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	seedCosts(t, db, "os-name", "issue-1", 2.0)
	seedOutcome(t, db, "gh-name", "issue-1", 3, 1)

	// Wrong mapping: outcome canonicalizes to "typo", cost stays "os-name" → split.
	if err := db.UpsertDeveloperAlias(ctx, "gh-name", "typo"); err != nil {
		t.Fatalf("seed wrong alias: %v", err)
	}
	if devs := scoresDevs(t, h); len(devs) != 2 {
		t.Fatalf("wrong alias: got %d rows, want 2 (still split); %v", len(devs), devs)
	}

	// Corrected mapping re-points the same alias to the cost-side identity.
	if err := db.UpsertDeveloperAlias(ctx, "gh-name", "os-name"); err != nil {
		t.Fatalf("update alias: %v", err)
	}
	devs := scoresDevs(t, h)
	if len(devs) != 1 {
		t.Fatalf("corrected alias: got %d rows, want 1 (rejoined); %v", len(devs), devs)
	}
	got := devs["os-name"]
	if got.WeightedPoints != 3 || got.TotalCostUSD != 2.0 {
		t.Errorf("rejoined row = %+v, want points=3 cost=2.0", got)
	}
}

// TestGetDeveloperScore_SumsCostRowsAcrossAliases pins that the detail endpoint
// AGGREGATES every cost row resolving to the target (it replaced a
// break-on-first-match loop). A cost row under the alias AND under the canonical
// must both count.
func TestGetDeveloperScore_SumsCostRowsAcrossAliases(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice.smith", "issue-1", 2.0) // canonical
	seedCosts(t, db, "asmith-gh", "issue-2", 3.0)   // alias
	seedOutcome(t, db, "asmith-gh", "issue-1", 4, 1)
	if err := db.UpsertDeveloperAlias(context.Background(), "asmith-gh", "alice.smith"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores/asmith-gh", nil)
	if code != http.StatusOK {
		t.Fatalf("GET: status = %d, body = %s", code, body)
	}
	var resp developerDetailResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Developer != "alice.smith" {
		t.Errorf("developer = %q, want alice.smith", resp.Developer)
	}
	if resp.TotalCostUSD != 5.0 {
		t.Errorf("total_cost_usd = %v, want 5.0 (2.0 canonical + 3.0 alias)", resp.TotalCostUSD)
	}
	if resp.WeightedPoints != 4 {
		t.Errorf("weighted_points = %v, want 4", resp.WeightedPoints)
	}
}

// TestGetScores_TeamFilterResolvesCanonical pins spec step 7: the ?team= filter
// resolves teams[canon], and org_hierarchy must hold the CANONICAL identity. An
// alias-joined developer enrolled under their canonical id must appear in the
// team rollup.
func TestGetScores_TeamFilterResolvesCanonical(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	seedCosts(t, db, "alice.smith", "issue-1", 1.0)
	seedOutcome(t, db, "asmith-gh", "issue-1", 3, 1)
	if err := db.UpsertDeveloperAlias(ctx, "asmith-gh", "alice.smith"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}
	// org_hierarchy keyed by the CANONICAL identity.
	if err := db.UpsertHierarchy(ctx, "alice.smith", "eng", "platform", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy: %v", err)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?team=eng", nil)
	if code != http.StatusOK {
		t.Fatalf("GET: status = %d, body = %s", code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Team == nil {
		t.Fatalf("team rollup nil; the canonical developer was not resolved into team eng")
	}
	if resp.Team.WeightedPoints != 3 {
		t.Errorf("team weighted_points = %v, want 3", resp.Team.WeightedPoints)
	}
	if resp.Team.TotalCostUSD != 1.0 {
		t.Errorf("team total_cost_usd = %v, want 1.0", resp.Team.TotalCostUSD)
	}
}
