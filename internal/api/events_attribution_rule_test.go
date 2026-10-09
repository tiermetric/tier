package api

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// storedRules reads every stored token event back and returns its attribution
// rule keyed by idempotency key.
func storedRules(t *testing.T, db *store.DB) map[string]store.AttributionRule {
	t.Helper()
	rows, _, err := db.ListTokenEvents(context.Background(),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	out := map[string]store.AttributionRule{}
	for _, r := range rows {
		out[r.IdempotencyKey] = r.AttributionRule
	}
	return out
}

// TestPostEvents_AcceptsRule pins that every member of the closed set crosses
// /events and is stored as sent (#823).
func TestPostEvents_AcceptsRule(t *testing.T) {
	h, db := newTestHandler(t)
	var batch []map[string]any
	for _, r := range store.AttributionRules() {
		e := validEvent("k-rule-" + string(r))
		e["attribution_rule"] = string(r)
		batch = append(batch, e)
	}
	if code, body := postEvents(t, h, batch); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}
	got := storedRules(t, db)
	for _, r := range store.AttributionRules() {
		if g := got["k-rule-"+string(r)]; g != r {
			t.Errorf("stored attribution_rule for %q = %q, want %q", r, g, r)
		}
	}
}

// TestPostEvents_RejectsUnknownRule pins that a rule outside the closed set is a
// clean 400 naming the row and the allowed set, never echoing the refused value,
// and that the valid sibling does not land.
func TestPostEvents_RejectsUnknownRule(t *testing.T) {
	for _, bad := range []string{"legacy", "Branch", "carry ", "/Users/alice/secret-wt-9f3a"} {
		t.Run(bad, func(t *testing.T) {
			h, db := newTestHandler(t)
			e := validEvent("k-bad-rule")
			e["attribution_rule"] = bad
			code, body := postEvents(t, h, []map[string]any{validEvent("k-ok"), e})
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", code, body)
			}
			msg := string(body)
			if !strings.Contains(msg, "events[1]") {
				t.Errorf("error should name events[1]; body = %s", msg)
			}
			for _, r := range store.AttributionRules() {
				if !strings.Contains(msg, string(r)) {
					t.Errorf("error should name allowed rule %q; body = %s", r, msg)
				}
			}
			if strings.Contains(msg, bad) {
				t.Errorf("error echoes the refused value %q; body = %s", bad, msg)
			}
			if costs := developerCosts(t, db); len(costs) != 0 {
				t.Errorf("valid sibling inserted from a rejected batch: %v", costs)
			}
		})
	}
}

// TestPostEvents_PreChangeShapeAccepted pins that a pre-#823 shipper, which sends
// no attribution_rule key, is still accepted and its row stores no rule.
func TestPostEvents_PreChangeShapeAccepted(t *testing.T) {
	h, db := newTestHandler(t)
	e := validEvent("k-pre-823")
	if _, ok := e["attribution_rule"]; ok {
		t.Fatal("fixture must omit attribution_rule to model a pre-#823 shipper")
	}
	if code, body := postEvents(t, h, []map[string]any{e}); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}
	got := storedRules(t, db)
	if r, ok := got["k-pre-823"]; !ok || r != store.AttributionRuleNone {
		t.Errorf("stored attribution_rule = %q (present %v), want none", r, ok)
	}
}

// TestScores_ForeignRepoNotExploratory pins that unattributed:foreign-repo is
// accepted on /events, counts as unattributed, and is not exploratory spend: the
// exploratory shares are the main bucket alone.
func TestScores_ForeignRepoNotExploratory(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 0.40)
	seedCosts(t, db, "alice", store.UnattributedMainBucket, 0.30)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)
	e := validEvent("k-foreign")
	e["issue_id"] = store.UnattributedForeignRepoBucket
	e["input_tokens"] = 100000 // $0.30 at claude-sonnet-4's $3/M input rate
	e["output_tokens"] = 0
	e["cost_usd"] = 0.30
	e["attribution_rule"] = string(store.AttributionRuleWorktreeCWD)
	e["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	if code, body := postEvents(t, h, []map[string]any{e}); code != http.StatusCreated {
		t.Fatalf("POST foreign-repo event: status = %d, want 201; body = %s", code, body)
	}

	resp := decodeScores(t, h)
	dq := resp.DataQuality
	if dq == nil {
		t.Fatal("data_quality absent")
	}
	if dq.AttributedCostShare == nil || math.Abs(*dq.AttributedCostShare-0.40) > 1e-9 {
		t.Errorf("attributed_cost_share = %v, want 0.40 (foreign-repo is unattributed)", dq.AttributedCostShare)
	}
	if dq.ExploratoryCostShare == nil || math.Abs(*dq.ExploratoryCostShare-0.30) > 1e-9 {
		t.Errorf("exploratory_cost_share = %v, want 0.30 (main only, not main + foreign-repo)", dq.ExploratoryCostShare)
	}
	var foreign float64
	for _, b := range dq.UnattributedBuckets {
		if b.Bucket == store.UnattributedForeignRepoBucket {
			foreign = b.Share
		}
	}
	if math.Abs(foreign-0.30) > 1e-9 {
		t.Errorf("foreign-repo bucket share = %v, want 0.30; buckets = %+v", foreign, dq.UnattributedBuckets)
	}
	if d, ok := scoresDevsFrom(resp)["alice"]; !ok {
		t.Fatal("alice missing from developer rows")
	} else if math.Abs(d.ExploratoryCostShare-0.30) > 1e-9 {
		t.Errorf("alice exploratory_cost_share = %v, want 0.30 (main only)", d.ExploratoryCostShare)
	}
}

// TestPostEvents_ForeignRepoCarriesNoRepo pins #823 Q3: an unattributed:foreign-repo
// row stores no repository name. Omitted or empty repo is accepted and stored as
// repoid.Unqualified; any other value, the literal sentinel included (validateRepo
// already reserves it), is a 400 that never echoes the value, and the batch sibling
// does not land.
func TestPostEvents_ForeignRepoCarriesNoRepo(t *testing.T) {
	foreign := func(key string) map[string]any {
		e := validEvent(key)
		e["issue_id"] = store.UnattributedForeignRepoBucket
		return e
	}
	for _, bad := range []string{"acme/secret", repoid.Unqualified} {
		t.Run("refuse "+bad, func(t *testing.T) {
			h, db := newTestHandler(t)
			e := foreign("k-foreign-bad")
			e["repo"] = bad
			code, body := postEvents(t, h, []map[string]any{validEvent("k-ok"), e})
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", code, body)
			}
			if bad != repoid.Unqualified && strings.Contains(string(body), bad) {
				t.Errorf("error echoes the refused repo %q; body = %s", bad, body)
			}
			if costs := developerCosts(t, db); len(costs) != 0 {
				t.Errorf("valid sibling inserted from a rejected batch: %v", costs)
			}
		})
	}
	h, db := newTestHandler(t)
	omitted := foreign("k-foreign-omitted")
	empty := foreign("k-foreign-empty")
	empty["repo"] = ""
	if code, body := postEvents(t, h, []map[string]any{omitted, empty}); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}
	rows, _, err := db.ListTokenEvents(context.Background(),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("stored %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Repo != repoid.Unqualified {
			t.Errorf("%s stored repo = %q, want %q", r.IdempotencyKey, r.Repo, repoid.Unqualified)
		}
	}
}
