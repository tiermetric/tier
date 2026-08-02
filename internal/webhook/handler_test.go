package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/prderive"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// sizeWeightVia resolves labels through the shared prderive derivation using the
// handler's configured table — the same call handlePR makes. These size-label tests
// verify the WithSizeLabels → h.sizeLabels wiring is installed correctly; the
// derivation itself is pinned in internal/prderive (#301).
func sizeWeightVia(h *Handler, labels []prLabel) float64 {
	return prderive.SizeWeight(labelNames(labels), h.sizeLabels)
}

// testSecret is the webhook secret used across handler tests. Post-#60 the
// handler is fail-closed, so every test that expects processing must sign
// its body — which also exercises verifySignature on every dispatch.
const testSecret = "test-webhook-secret-60"

// sign computes the X-Hub-Signature-256 header value for body.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// fakeStore is an in-memory Store implementation that satisfies the webhook
// Store interface. Keeps tests pure-Go and free of SQLite setup overhead.
// Mirrors the real store's contracts on empty inputs, lookup semantics, AND
// the #60 unique-merge-SHA insert (ON CONFLICT DO NOTHING) so the mock
// can't pass where the real store would behave differently. Mutex-guarded
// because the #60 concurrency tests drive ServeHTTP from many goroutines.
type fakeStore struct {
	mu             sync.Mutex
	outcomes       []store.Outcome
	qualityUpdates []qualityUpdate
	byMergeCommit  map[string]store.Outcome
	// events records every AppendQualityEvent, enforcing the real store's
	// (outcome_id, event_type, source_ref) unique key so replay idempotency is
	// exercised identically to SQLite (#134).
	events []store.QualityEvent
	nextID int64 // assigns outcomes.id so row-targeted writes can address rows
	// insertErrs > 0 fails the next InsertOutcome calls, decrementing each
	// time — transient-error injection for the retry-not-suppressed test.
	insertErrs int
	// payloads records every InsertWebhookPayload call for the audit-trail
	// tests (#137). payloadErr, when set, makes InsertWebhookPayload fail so
	// the best-effort persistence path can be exercised.
	payloads   []persistedPayload
	payloadErr error
	// pushDaily mirrors the real store's (issue_id, UTC-day) partial unique index
	// for source='push' rows (#196): a second UpsertPushOutcome for the same key is
	// a DO-NOTHING no-op, so the fake exercises idempotency identically to SQLite.
	pushDaily map[string]bool
	// upsertErrs > 0 fails the next UpsertPushOutcome calls, decrementing each time —
	// transient-error injection for the capture-path retry test.
	upsertErrs int
}

// qualityUpdate records one UpdateQualityForOutcome call (#134). Developer and
// IssueID are resolved from the target outcome so the existing revert tests can
// keep asserting "penalty hit the original author".
type qualityUpdate struct {
	OutcomeID int64
	Developer string
	IssueID   string
	Quality   float64
	Reason    string
	SourceRef string
}

// persistedPayload captures one InsertWebhookPayload call. Body is copied so a
// later reuse of the handler's read buffer can't mutate the recorded bytes.
type persistedPayload struct {
	Event      string
	DeliveryID string
	Body       []byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byMergeCommit: map[string]store.Outcome{},
	}
}

func (r *fakeStore) InsertOutcome(_ context.Context, o store.Outcome) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.insertErrs > 0 {
		r.insertErrs--
		return false, errInjectedInsert
	}
	// Mirror the real store's partial UNIQUE index + ON CONFLICT DO
	// NOTHING (#60): a duplicate non-empty merge SHA is a silent no-op and
	// reports inserted=false (#188).
	if o.MergeCommitSHA != "" {
		if _, dup := r.byMergeCommit[o.MergeCommitSHA]; dup {
			return false, nil
		}
		r.byMergeCommit[o.MergeCommitSHA] = o
	}
	// Assign a row id like the real AUTOINCREMENT so row-targeted quality
	// writes (#134) can address this outcome.
	r.nextID++
	o.ID = r.nextID
	r.outcomes = append(r.outcomes, o)
	return true, nil
}

var errInjectedInsert = errors.New("injected transient insert failure")

var errInjectedUpsert = errors.New("injected transient upsert failure")

// UpsertPushOutcome mirrors the real store (#196): idempotent on (issue_id, day)
// for source='push' rows, forcing source='push' and no-opping a replay. Empty day
// fails loud exactly as the real method does.
func (r *fakeStore) UpsertPushOutcome(_ context.Context, o store.Outcome, day string) (bool, error) {
	if day == "" {
		return false, errors.New("fake UpsertPushOutcome: empty day")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.upsertErrs > 0 {
		r.upsertErrs--
		return false, errInjectedUpsert
	}
	if r.pushDaily == nil {
		r.pushDaily = map[string]bool{}
	}
	key := o.IssueID + "\x00" + day
	if r.pushDaily[key] {
		return false, nil // (issue, day) already captured — ON CONFLICT DO NOTHING
	}
	r.pushDaily[key] = true
	r.nextID++
	o.ID = r.nextID
	o.Source = store.OutcomeSourcePush
	r.outcomes = append(r.outcomes, o)
	return true, nil
}

// InsertWebhookPayload records the raw body for audit (#137). Returns
// payloadErr when set so tests can drive the best-effort failure path.
func (r *fakeStore) InsertWebhookPayload(_ context.Context, event, deliveryID string, rawBody []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.payloadErr != nil {
		return r.payloadErr
	}
	body := append([]byte(nil), rawBody...)
	r.payloads = append(r.payloads, persistedPayload{Event: event, DeliveryID: deliveryID, Body: body})
	return nil
}

// persistedPayloads returns a copy of the recorded payloads under the lock.
func (r *fakeStore) persistedPayloads() []persistedPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]persistedPayload(nil), r.payloads...)
}

// AppendQualityEvent mirrors the real store: it rejects unknown event types and
// is idempotent on the (outcome_id, event_type, source_ref) unique key (#134).
func (r *fakeStore) AppendQualityEvent(_ context.Context, e store.QualityEvent) (bool, error) {
	if !fakeValidEventType(e.EventType) {
		return false, fmt.Errorf("AppendQualityEvent: unknown event_type %q", e.EventType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ex := range r.events {
		if ex.OutcomeID == e.OutcomeID && ex.EventType == e.EventType && ex.SourceRef == e.SourceRef {
			return false, nil // replay: unique-key conflict, insert nothing
		}
	}
	e.EventTS = e.EventTS.UTC()
	r.events = append(r.events, e)
	return true, nil
}

// QualityEventsForOutcome returns the events for an outcome in insertion order.
func (r *fakeStore) QualityEventsForOutcome(_ context.Context, outcomeID int64) ([]store.QualityEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []store.QualityEvent
	for _, e := range r.events {
		if e.OutcomeID == outcomeID {
			out = append(out, e)
		}
	}
	return out, nil
}

// UpdateQualityForOutcome updates the target outcome's quality in place (so
// subsequent reads see the new value) and records the call for assertion (#134).
// Mirrors the real store's no-op suppression: when the target value already
// equals the current quality it writes nothing and records no update — this is
// what keeps clean CI passes and replayed deliveries from spamming transitions.
func (r *fakeStore) UpdateQualityForOutcome(_ context.Context, outcomeID int64, quality float64, reason, sourceRef string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.outcomes {
		if r.outcomes[i].ID == outcomeID {
			if r.outcomes[i].Quality == quality {
				return nil // no-op: derived value already applied
			}
			r.outcomes[i].Quality = quality
			r.qualityUpdates = append(r.qualityUpdates, qualityUpdate{
				OutcomeID: outcomeID,
				Developer: r.outcomes[i].Developer,
				IssueID:   r.outcomes[i].IssueID,
				Quality:   quality,
				Reason:    reason,
				SourceRef: sourceRef,
			})
			return nil
		}
	}
	return fmt.Errorf("fake UpdateQualityForOutcome: no outcome with id %d", outcomeID)
}

// fakeValidEventType mirrors store.validQualityEventTypes so the fake rejects a
// bad type exactly as the real store would.
func fakeValidEventType(t string) bool {
	switch t {
	case "ci_pass", "ci_fail", "ci_fail_flaky", "revert_quality", "revert_strategic":
		return true
	}
	return false
}

// OutcomeByMergeCommit searches the outcomes slice (not byMergeCommit) so it
// reflects in-place quality updates. Reverse scan mirrors the real store's
// ORDER BY id DESC LIMIT 1.
func (r *fakeStore) OutcomeByMergeCommit(_ context.Context, sha string) (store.Outcome, bool, error) {
	// Mirror the real store's empty-input guard so the fake never returns
	// a "found" for SHA="".
	if sha == "" {
		return store.Outcome{}, false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.outcomes) - 1; i >= 0; i-- {
		if r.outcomes[i].MergeCommitSHA == sha {
			return r.outcomes[i], true, nil
		}
	}
	return store.Outcome{}, false, nil
}

// LatestOutcomeByIssue returns the most recently inserted outcome that matches the
// given (repo, issue id). The fake walks outcomes in reverse to mimic the real
// store's ORDER BY id DESC, and reproduces its TOLERANT repo match (#231): an exact
// repo match wins, and a row carrying the 'unqualified' sentinel is still reachable
// so pre-#231 outcomes keep receiving revert penalties.
//
// This mirrors the real SQL deliberately. A fake that matched on issue id alone
// would make every webhook test pass while the store fused two repos' issues.
func (r *fakeStore) LatestOutcomeByIssue(_ context.Context, repo, issueID string) (store.Outcome, bool, error) {
	if issueID == "" {
		return store.Outcome{}, false, nil
	}
	if repo == "" {
		repo = repoid.Unqualified
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var fallback store.Outcome
	var haveFallback bool
	for i := len(r.outcomes) - 1; i >= 0; i-- {
		o := r.outcomes[i]
		if o.IssueID != issueID {
			continue
		}
		oRepo := o.Repo
		if oRepo == "" {
			oRepo = repoid.Unqualified
		}
		if oRepo == repo {
			return o, true, nil
		}
		if oRepo == repoid.Unqualified && !haveFallback {
			fallback, haveFallback = o, true
		}
	}
	if haveFallback {
		return fallback, true, nil
	}
	return store.Outcome{}, false, nil
}

// outcomeCount returns the number of recorded outcomes under the lock.
func (r *fakeStore) outcomeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.outcomes)
}

// captureLogs runs fn against a logger backed by an in-memory buffer and
// returns the captured text. Used to assert slog.Info fired on the tier-3
// path. Filters out the noisy "TIER_WEBHOOK_SECRET is not set" startup
// warning that New emits on every handler construction — that line would
// otherwise pollute substring-match assertions on the buffer.
func captureLogs(fn func(logger *slog.Logger)) string {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	fn(logger)
	out := buf.String()
	// Strip the startup warning line; it has nothing to do with what the
	// test under inspection actually logged.
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "TIER_WEBHOOK_SECRET is not set") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// doPush wires a Handler around the supplied store + logger and dispatches a
// synthetic push event with the given commits.
func doPush(t *testing.T, st Store, logger *slog.Logger, commits ...map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"commits": commits})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	h := New(st, testSecret, logger)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("push handler returned %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGitHeuristic_LabelScaleAlignment pins the #132 rescale: the unlabeled git
// heuristic now maps the effort proxy (lines + 10*files) onto the SAME
// 0.5/1/3/5/8 buckets the size-label table uses, so an unlabeled PR is commensurate with
// a labeled one. The worked examples come straight from the spec's table; the
// boundary cases pin each threshold edge (<= is inclusive on the lower bucket).
// These FAIL on the pre-#132 log2 formula (which scored e.g. 50/3 as 7, not 3).
func TestGitHeuristic_LabelScaleAlignment(t *testing.T) {
	cases := []struct {
		name         string
		lines, files int
		want         float64
	}{
		// Worked examples from the spec (effort = lines + 10*files).
		{"typo 1 line 1 file (effort 11)", 1, 1, 0.5},
		{"50 lines 3 files (effort 80)", 50, 3, 3.0},
		{"200 lines 5 files (effort 250)", 200, 5, 5.0},
		{"1000 lines 10 files (effort 1100)", 1000, 10, 8.0},
		{"degenerate empty payload (effort 0)", 0, 0, 0.5},
		// Threshold boundaries (files=0 so effort == lines).
		{"effort 15 -> xs floor", 15, 0, 0.5},
		{"effort 16 -> s", 16, 0, 1.0},
		{"effort 60 -> s upper edge", 60, 0, 1.0},
		{"effort 61 -> m", 61, 0, 3.0},
		{"effort 200 -> m upper edge", 200, 0, 3.0},
		{"effort 201 -> l", 201, 0, 5.0},
		{"effort 1000 -> l upper edge", 1000, 0, 5.0},
		{"effort 1001 -> xl", 1001, 0, 8.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gitHeuristic(c.lines, c.files); got != c.want {
				t.Errorf("gitHeuristic(%d,%d) = %v, want %v", c.lines, c.files, got, c.want)
			}
		})
	}
}

// TestHandlePR_RecordsWeightSourceProvenance pins the #132 provenance wiring:
// handlePR must stamp weight_source by which scale produced the weight ("label"
// when a size label is present, "git-heuristic" on the diff-size fallback) and
// persist the raw diff stats. Without this the rescale is pinned but the
// provenance the commit advertises could silently regress.
func TestHandlePR_RecordsWeightSourceProvenance(t *testing.T) {
	t.Run("labeled PR -> source label", func(t *testing.T) {
		st := newFakeStore()
		h := New(st, testSecret, quietLogger())
		body, err := json.Marshal(map[string]any{
			"action": "closed",
			"pull_request": map[string]any{
				"number":           7,
				"merged":           true,
				"merge_commit_sha": "sha-labeled",
				"head":             map[string]any{"ref": "feature/42-foo"},
				"user":             map[string]any{"login": "alice"},
				"labels":           []map[string]any{{"name": "size/m"}},
				"additions":        4,
				"deletions":        1,
				"changed_files":    1,
			},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if code := doPR(t, h, body, "d-labeled", sign(testSecret, body)); code != http.StatusNoContent {
			t.Fatalf("doPR status = %d, want 204", code)
		}
		o, ok, _ := st.LatestOutcomeByIssue(context.Background(), "", "issue-42")
		if !ok {
			t.Fatal("no outcome recorded")
		}
		if o.WeightSource != store.WeightSourceLabel {
			t.Errorf("WeightSource = %q, want %q", o.WeightSource, store.WeightSourceLabel)
		}
		if o.Weight != 3.0 {
			t.Errorf("Weight = %v, want 3.0 (size/m)", o.Weight)
		}
		if o.Additions != 4 || o.Deletions != 1 || o.ChangedFiles != 1 {
			t.Errorf("diff stats = (%d,%d,%d), want (4,1,1)", o.Additions, o.Deletions, o.ChangedFiles)
		}
	})

	t.Run("unlabeled PR -> source git-heuristic", func(t *testing.T) {
		st := newFakeStore()
		h := New(st, testSecret, quietLogger())
		body := prMergedBody(t, 8, "sha-unlabeled") // no labels; additions=100, del=20, files=3
		if code := doPR(t, h, body, "d-heur", sign(testSecret, body)); code != http.StatusNoContent {
			t.Fatalf("doPR status = %d, want 204", code)
		}
		o, ok, _ := st.LatestOutcomeByIssue(context.Background(), "", "issue-42")
		if !ok {
			t.Fatal("no outcome recorded")
		}
		if o.WeightSource != store.WeightSourceHeuristic {
			t.Errorf("WeightSource = %q, want %q", o.WeightSource, store.WeightSourceHeuristic)
		}
		if o.Additions != 100 || o.Deletions != 20 || o.ChangedFiles != 3 {
			t.Errorf("diff stats = (%d,%d,%d), want (100,20,3)", o.Additions, o.Deletions, o.ChangedFiles)
		}
	})
}

// TestHandlePR_MultiIssueAttributesToPrimary pins the #189 multi-issue rule: a
// merged PR that closes several issues yields exactly ONE outcome, attributed to
// the PRIMARY (leftmost) closed issue, and logs the full closed set so the
// un-credited secondaries are observable. The branch here carries no issue id, so
// the primary is resolved from the body's leftmost close directive.
func TestHandlePR_MultiIssueAttributesToPrimary(t *testing.T) {
	st := newFakeStore()
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := New(st, testSecret, logger)

	body, err := json.Marshal(map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":           21,
			"merged":           true,
			"merge_commit_sha": "sha-multi",
			"head":             map[string]any{"ref": "trunk"}, // no issue id in branch
			"body":             "Wraps up the work. closes #12, #15",
			"user":             map[string]any{"login": "alice"},
			"additions":        10,
			"deletions":        2,
			"changed_files":    1,
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if code := doPR(t, h, body, "d-multi", sign(testSecret, body)); code != http.StatusNoContent {
		t.Fatalf("doPR status = %d, want 204", code)
	}

	// Exactly one outcome, under the primary (leftmost) issue.
	if n := st.outcomeCount(); n != 1 {
		t.Fatalf("outcomeCount = %d, want 1 (one merged PR = one outcome)", n)
	}
	if _, ok, _ := st.LatestOutcomeByIssue(context.Background(), "", "issue-12"); !ok {
		t.Error("no outcome under the primary issue-12")
	}
	if _, ok, _ := st.LatestOutcomeByIssue(context.Background(), "", "issue-15"); ok {
		t.Error("secondary issue-15 must NOT get its own outcome (merge_commit_sha is unique)")
	}
	// The secondaries are logged, not silently dropped.
	if out := logBuf.String(); !strings.Contains(out, "PR closes multiple issues") ||
		!strings.Contains(out, "issue-15") {
		t.Errorf("expected a multi-issue INFO log naming the closed set, got: %s", out)
	}
}

// TestHandlePush_RevertResolvedByIssueIDLookup covers the issue-id-in-message
// path (tier 2 of resolution — runs when no SHA footer is present). The
// revert subject inherits the original commit's "closes #N", so issueref
// extracts the issue. We then look up the LATEST outcome for that issue
// (not the reverter's outcomes) so the penalty lands on the original PR's
// author — not on whoever pushed the revert. This is the exact semantic
// that #20 set out to fix.
func TestHandlePush_RevertResolvedByIssueIDLookup(t *testing.T) {
	st := newFakeStore()
	// Pre-seed: bob's PR for issue-42 was merged. No merge_commit_sha for
	// this case — we want the footer path to MISS so the issue-id lookup
	// runs.
	_, _ = st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "bob",
		IssueID:   "issue-42",
		PRNumber:  10,
		Weight:    5,
		Quality:   1.0,
		Timestamp: time.Now().UTC(), // within the 60d revert window
	})

	logs := captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id":      "rev1",
			"message": `Revert "feat: add foo (closes #42)"`,
			"author":  map[string]any{"username": "alice"},
		})
	})

	if len(st.qualityUpdates) != 1 {
		t.Fatalf("expected 1 quality update, got %d", len(st.qualityUpdates))
	}
	u := st.qualityUpdates[0]
	// Penalty goes to bob (original author), NOT alice (reverter). The
	// message has no strategic/quality keyword, so it classifies as a quality
	// revert and floors to 0.1 (#134, spec §3 Event 4 — down from the old 0.5).
	if u.Developer != "bob" || u.IssueID != "issue-42" || u.Quality != 0.1 {
		t.Errorf("quality update = %+v, want {bob, issue-42, 0.1} — penalty must hit original author not reverter", u)
	}
	if u.Reason != "revert_quality" {
		t.Errorf("history reason = %q, want revert_quality", u.Reason)
	}
	if strings.Contains(logs, "revert detected but no issue id derivable") {
		t.Errorf("issue-id path resolved successfully; should not log fallback; logs=%s", logs)
	}
}

// TestHandlePush_RevertWithIssueIDButNoOriginalOutcome covers the edge case
// where the revert message names an issue but the original outcome was
// never recorded (webhook lost, manual setup, etc). Falls through to the
// tier-3 log — we don't fabricate a developer to penalize.
func TestHandlePush_RevertWithIssueIDButNoOriginalOutcome(t *testing.T) {
	st := newFakeStore()
	// No seeded outcomes.
	logs := captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id":      "rev-orphan",
			"message": `Revert "feat: add foo (closes #99)"`,
			"author":  map[string]any{"username": "alice"},
		})
	})
	if len(st.qualityUpdates) != 0 {
		t.Errorf("expected 0 quality updates when no original outcome exists, got %d: %+v",
			len(st.qualityUpdates), st.qualityUpdates)
	}
	if !strings.Contains(logs, "revert detected but no issue id derivable") {
		t.Errorf("expected fallback slog.Info, logs=%s", logs)
	}
}

// TestHandlePush_RevertResolvedByMergeCommitFooter covers tier 2 (#20's main
// fix): the revert message has no issue id of its own, but the standard
// "This reverts commit <sha>" footer points at a merge commit recorded in
// the outcomes table. We use the original outcome's issue_id and developer
// to apply the quality penalty.
func TestHandlePush_RevertResolvedByMergeCommitFooter(t *testing.T) {
	st := newFakeStore()
	// Pre-seed: bob's PR for issue-7 was merged at SHA abcdef0.
	_, _ = st.InsertOutcome(context.Background(), store.Outcome{
		Developer:      "bob",
		IssueID:        "issue-7",
		PRNumber:       42,
		Weight:         5,
		Quality:        1.0,
		MergeCommitSHA: "abcdef0123456789abcdef0123456789abcdef01",
		Timestamp:      time.Now().UTC(), // within the 60d revert window
	})

	// alice reverted bob's commit. The original commit's subject had no
	// issue id, so the revert message inherits none either — but the
	// auto-generated footer does.
	logs := captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id": "rev2",
			"message": `Revert "feat: add foo"

This reverts commit abcdef0123456789abcdef0123456789abcdef01.`,
			"author": map[string]any{"username": "alice"},
		})
	})

	if len(st.qualityUpdates) != 1 {
		t.Fatalf("expected 1 quality update via footer lookup, got %d", len(st.qualityUpdates))
	}
	u := st.qualityUpdates[0]
	// The penalty applies to the original PR's author (bob), not the reverter
	// (alice), and floors to the quality-revert value 0.1 (#134).
	if u.Developer != "bob" || u.IssueID != "issue-7" || u.Quality != 0.1 {
		t.Errorf("quality update = %+v, want {bob, issue-7, 0.1}", u)
	}
	if strings.Contains(logs, "revert detected but no issue id derivable") {
		t.Errorf("tier-2 path resolved successfully; should not log fallback; logs=%s", logs)
	}
}

// TestHandlePush_RevertNoIssueIDLogsAndContinues covers tier 3: revert
// detected but neither the message nor the footer give us a usable id.
// Previously this no-opped silently; now it emits slog.Info so the gap is
// discoverable.
func TestHandlePush_RevertNoIssueIDLogsAndContinues(t *testing.T) {
	st := newFakeStore()
	logs := captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id":      "rev3",
			"message": `Revert "feat: add foo"`, // no issue id, no footer
			"author":  map[string]any{"username": "alice"},
		})
	})
	if len(st.qualityUpdates) != 0 {
		t.Errorf("expected 0 quality updates, got %d", len(st.qualityUpdates))
	}
	if !strings.Contains(logs, "revert detected but no issue id derivable") {
		t.Errorf("expected fallback slog.Info, logs=%s", logs)
	}
	if !strings.Contains(logs, "rev3") {
		t.Errorf("log must include commit SHA for discoverability; logs=%s", logs)
	}
}

// TestHandlePush_RevertFooterPointsToUnknownSHA: the footer is well-formed but
// the SHA isn't in our outcomes table (we never saw that PR's merge). Falls
// through to the tier-3 log.
func TestHandlePush_RevertFooterPointsToUnknownSHA(t *testing.T) {
	st := newFakeStore()
	logs := captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id": "rev4",
			"message": `Revert "feat: add foo"

This reverts commit 0000000000000000000000000000000000000000.`,
			"author": map[string]any{"username": "alice"},
		})
	})
	if len(st.qualityUpdates) != 0 {
		t.Errorf("expected 0 quality updates (unknown SHA), got %d", len(st.qualityUpdates))
	}
	if !strings.Contains(logs, "revert detected but no issue id derivable") {
		t.Errorf("expected fallback slog.Info, logs=%s", logs)
	}
}

// TestHandlePush_NonRevertCommitIgnored confirms a normal commit doesn't
// trigger any branch of revert detection — basic sanity to guard against the
// regex accidentally matching too broadly.
func TestHandlePush_NonRevertCommitIgnored(t *testing.T) {
	st := newFakeStore()
	logs := captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id":      "normal1",
			"message": "feat: add a new feature (closes #99)",
			"author":  map[string]any{"username": "alice"},
		})
	})
	if len(st.qualityUpdates) != 0 {
		t.Errorf("non-revert triggered quality update: %+v", st.qualityUpdates)
	}
	if strings.Contains(logs, "revert detected") {
		t.Errorf("non-revert produced revert log: %s", logs)
	}
}

// TestHandlePush_MalformedFooterFallsThrough covers a regex-boundary case: a
// short or otherwise non-conforming SHA in the footer (or an uppercase SHA)
// must NOT match. The revert falls through to tier-3 logging.
func TestHandlePush_MalformedFooterFallsThrough(t *testing.T) {
	cases := []struct {
		name   string
		footer string
	}{
		{"short SHA (5 chars)", "This reverts commit abcde."},
		{"short SHA (39 chars)", "This reverts commit abcdef0123456789abcdef0123456789abcdef0."},
		{"uppercase SHA", "This reverts commit ABCDEF0123456789ABCDEF0123456789ABCDEF01."},
		{"missing footer", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			logs := captureLogs(func(l *slog.Logger) {
				doPush(t, st, l, map[string]any{
					"id":      "rev-bad-footer",
					"message": "Revert \"feat: foo\"\n\n" + tc.footer,
					"author":  map[string]any{"username": "alice"},
				})
			})
			if len(st.qualityUpdates) != 0 {
				t.Errorf("expected 0 quality updates, got %d", len(st.qualityUpdates))
			}
			if !strings.Contains(logs, "revert detected but no issue id derivable") {
				t.Errorf("expected fallback slog.Info, logs=%s", logs)
			}
		})
	}
}

// TestHandlePush_LogsAuthor confirms the tier-3 fallback log includes the
// reverter's username so an operator investigating "this revert didn't
// register" can see who pushed it.
func TestHandlePush_LogsAuthor(t *testing.T) {
	st := newFakeStore()
	logs := captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id":      "rev-nolookup",
			"message": `Revert "feat: foo"`,
			"author":  map[string]any{"username": "alice"},
		})
	})
	// The username is now routed through logSafeStr (#288, go/log-injection), so it
	// renders quoted and bound to the author key - still fully observable to an
	// operator, just injection-safe. Assert the exact rendered form so the key/value
	// binding is verified (a bare Contains("alice") would pass on any field).
	if !strings.Contains(logs, `author="\"alice\""`) {
		t.Errorf("expected sanitized reverter username in tier-3 log, got: %s", logs)
	}
}

// TestHandlePush_MultiCommit verifies the loop's per-commit behavior when a
// push event delivers a normal commit AND a revert in the same payload.
// Both should be processed; the loop's lastErr accumulation shouldn't
// short-circuit on the first non-revert.
func TestHandlePush_MultiCommit(t *testing.T) {
	st := newFakeStore()
	_, _ = st.InsertOutcome(context.Background(), store.Outcome{
		Developer:      "bob",
		IssueID:        "issue-7",
		MergeCommitSHA: "abcdef0123456789abcdef0123456789abcdef01",
		Timestamp:      time.Now().UTC(), // within the 60d revert window
	})

	captureLogs(func(l *slog.Logger) {
		doPush(t, st, l,
			map[string]any{
				"id":      "normal-c1",
				"message": "feat: a feature",
				"author":  map[string]any{"username": "alice"},
			},
			map[string]any{
				"id": "rev-c2",
				"message": `Revert "feat: a feature"

This reverts commit abcdef0123456789abcdef0123456789abcdef01.`,
				"author": map[string]any{"username": "alice"},
			},
		)
	})
	if len(st.qualityUpdates) != 1 {
		t.Fatalf("expected 1 quality update from the revert commit, got %d", len(st.qualityUpdates))
	}
	if st.qualityUpdates[0].Developer != "bob" {
		t.Errorf("multi-commit push: penalty target = %q, want bob", st.qualityUpdates[0].Developer)
	}
}

// --- #134: workflow_run CI signals + event-derived quality ---

// wfRunPayload builds a workflow_run webhook body. updatedAt is emitted as
// RFC3339; pass the zero time to omit it (exercises the time.Now fallback).
func wfRunPayload(headSHA, branch, conclusion string, attempt int, updatedAt time.Time) map[string]any {
	wr := map[string]any{
		"head_sha":    headSHA,
		"head_branch": branch,
		"conclusion":  conclusion,
		"run_attempt": attempt,
	}
	if !updatedAt.IsZero() {
		wr["updated_at"] = updatedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"action":       "completed",
		"workflow_run": wr,
		"repository":   map[string]any{"default_branch": "main"},
	}
}

// doWorkflowRun dispatches a signed workflow_run event and returns the status.
func doWorkflowRun(t *testing.T, h *Handler, payload map[string]any, deliveryID string) int {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal workflow_run: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "workflow_run")
	if deliveryID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryID)
	}
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// qualityBySHA reads an outcome's current quality via the fake's merge-commit
// lookup (which reflects in-place quality updates).
func (r *fakeStore) qualityBySHA(sha string) (float64, bool) {
	o, ok, _ := r.OutcomeByMergeCommit(context.Background(), sha)
	return o.Quality, ok
}

const wfMergeSHA = "0123456789abcdef0123456789abcdef01234567"

// seedMergedOutcome inserts a merged outcome for the CI tests, merged now so it
// sits inside the 48h window.
func seedMergedOutcome(t *testing.T, st *fakeStore) {
	t.Helper()
	if _, err := st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "alice", IssueID: "issue-42", PRNumber: 7, Weight: 3, Quality: 1.0,
		MergeCommitSHA: wfMergeSHA, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
}

func newTestHandler(st Store) *Handler {
	return New(st, testSecret, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
}

// TestHandleWorkflowRun_FailureDegradesOutcomeTo07 — the headline behavior; this
// FAILS ON MAIN, where workflow_run is dropped at the event switch.
func TestHandleWorkflowRun_FailureDegradesOutcomeTo07(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	h := newTestHandler(st)

	if code := doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "failure", 1, time.Now().UTC()), "wf-1"); code != http.StatusNoContent {
		t.Fatalf("workflow_run: status = %d, want 204", code)
	}
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.7 {
		t.Errorf("quality after CI failure = %v, want 0.7", q)
	}
}

// TestHandleWorkflowRun_SuccessNoPenalty — a clean pass records ci_pass but does
// not change quality (no spurious quality_history write on a still-clean row).
func TestHandleWorkflowRun_SuccessNoPenalty(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	h := newTestHandler(st)

	if code := doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "success", 1, time.Now().UTC()), "wf-ok"); code != http.StatusNoContent {
		t.Fatalf("workflow_run: status = %d, want 204", code)
	}
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 1.0 {
		t.Errorf("quality after CI success = %v, want 1.0", q)
	}
	if len(st.qualityUpdates) != 0 {
		t.Errorf("clean pass should write no quality transition, got %d", len(st.qualityUpdates))
	}
	if len(st.events) != 1 || st.events[0].EventType != "ci_pass" {
		t.Errorf("expected one ci_pass event, got %+v", st.events)
	}
}

// TestHandleWorkflowRun_FlakyRerunNeutralises — a same-SHA success within 30 min
// of a failure reclassifies it as flaky and restores quality to 1.0.
func TestHandleWorkflowRun_FlakyRerunNeutralises(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	h := newTestHandler(st)

	failTS := time.Now().UTC()
	if code := doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "failure", 1, failTS), "wf-f"); code != http.StatusNoContent {
		t.Fatalf("failure: status %d", code)
	}
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.7 {
		t.Fatalf("after failure quality = %v, want 0.7", q)
	}
	// Re-run of the SAME sha succeeds 10 minutes later.
	if code := doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "success", 2, failTS.Add(10*time.Minute)), "wf-s"); code != http.StatusNoContent {
		t.Fatalf("success rerun: status %d", code)
	}
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 1.0 {
		t.Errorf("after flaky rerun quality = %v, want 1.0 (neutralised)", q)
	}
}

// TestHandleWorkflowRun_SuccessAfter30MinNotFlaky — a success outside the 30-min
// window is a plain ci_pass, not a flaky reclassification; the failure stands.
func TestHandleWorkflowRun_SuccessAfter30MinNotFlaky(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	h := newTestHandler(st)

	failTS := time.Now().UTC().Add(-time.Hour) // still within 48h of merge
	_ = doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "failure", 1, failTS), "wf-f2")
	// Success 45 minutes later — past the flaky window.
	_ = doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "success", 2, failTS.Add(45*time.Minute)), "wf-s2")
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.7 {
		t.Errorf("quality = %v, want 0.7 (late success does not neutralise)", q)
	}
}

// TestHandleWorkflowRun_IgnoresNonDefaultBranch — CI on a non-default branch is
// not a merged-code signal.
func TestHandleWorkflowRun_IgnoresNonDefaultBranch(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	h := newTestHandler(st)

	_ = doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "feature/x", "failure", 1, time.Now().UTC()), "wf-nb")
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 1.0 {
		t.Errorf("quality = %v, want 1.0 (non-default branch ignored)", q)
	}
	if len(st.events) != 0 {
		t.Errorf("expected no events for non-default branch, got %+v", st.events)
	}
}

// TestHandleWorkflowRun_IgnoresIncomplete — only action=completed is processed.
func TestHandleWorkflowRun_IgnoresIncomplete(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	h := newTestHandler(st)

	payload := wfRunPayload(wfMergeSHA, "main", "failure", 1, time.Now().UTC())
	payload["action"] = "requested"
	_ = doWorkflowRun(t, h, payload, "wf-inc")
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 1.0 {
		t.Errorf("quality = %v, want 1.0 (incomplete run ignored)", q)
	}
}

// TestHandleWorkflowRun_UnknownSHANoop — a CI run for a head_sha with no recorded
// outcome (unmerged branch) is a silent no-op.
func TestHandleWorkflowRun_UnknownSHANoop(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	h := newTestHandler(st)

	_ = doWorkflowRun(t, h, wfRunPayload("ffffffffffffffffffffffffffffffffffffffff", "main", "failure", 1, time.Now().UTC()), "wf-unk")
	if len(st.events) != 0 || len(st.qualityUpdates) != 0 {
		t.Errorf("unknown SHA must be a no-op; events=%+v updates=%+v", st.events, st.qualityUpdates)
	}
}

// TestHandleWorkflowRun_Past48hWindowIgnored — a CI signal more than 48h after
// merge is stale and ignored.
func TestHandleWorkflowRun_Past48hWindowIgnored(t *testing.T) {
	st := newFakeStore()
	// Merge 3 days ago.
	if _, err := st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "alice", IssueID: "issue-42", Weight: 3, Quality: 1.0,
		MergeCommitSHA: wfMergeSHA, Timestamp: time.Now().UTC().Add(-72 * time.Hour),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := newTestHandler(st)

	_ = doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "failure", 1, time.Now().UTC()), "wf-old")
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 1.0 {
		t.Errorf("quality = %v, want 1.0 (signal outside 48h window ignored)", q)
	}
	if len(st.events) != 0 {
		t.Errorf("expected no events outside window, got %+v", st.events)
	}
}

// TestHandleWorkflowRun_ReplayedDeliveryIdempotent — the same failure delivered
// twice (fresh GUIDs to bypass the in-memory ring) produces ONE event and the
// same quality: replay-safety rests on the quality_events unique key.
func TestHandleWorkflowRun_ReplayedDeliveryIdempotent(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	h := newTestHandler(st)

	payload := wfRunPayload(wfMergeSHA, "main", "failure", 1, time.Now().UTC())
	_ = doWorkflowRun(t, h, payload, "wf-r1")
	_ = doWorkflowRun(t, h, payload, "wf-r2") // fresh GUID, identical content

	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.7 {
		t.Errorf("quality = %v, want 0.7", q)
	}
	ciFails := 0
	for _, e := range st.events {
		if e.EventType == "ci_fail" {
			ciFails++
		}
	}
	if ciFails != 1 {
		t.Errorf("ci_fail events = %d, want 1 (replay must not double-append)", ciFails)
	}
	if len(st.qualityUpdates) != 1 {
		t.Errorf("quality writes = %d, want 1 (replay must not re-write)", len(st.qualityUpdates))
	}
}

// TestHandlePush_QualityRevertNowFloors01 pins the value change (spec §3 Event
// 4): a keyword-free / quality-keyword revert floors to 0.1. FAILS ON MAIN,
// which writes 0.5.
func TestHandlePush_QualityRevertNowFloors01(t *testing.T) {
	st := newFakeStore()
	if _, err := st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "bob", IssueID: "issue-7", Weight: 5, Quality: 1.0,
		MergeCommitSHA: wfMergeSHA, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id":      "revsha1",
			"message": "Revert \"feat: add foo\"\n\nThis reverts commit " + wfMergeSHA + ". broke production, OOM",
			"author":  map[string]any{"username": "alice"},
		})
	})
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.1 {
		t.Errorf("quality after quality-revert = %v, want 0.1", q)
	}
}

// TestHandlePush_StrategicRevertFloors08 — a business-reason revert floors to
// 0.8, not 0.1.
func TestHandlePush_StrategicRevertFloors08(t *testing.T) {
	st := newFakeStore()
	if _, err := st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "bob", IssueID: "issue-7", Weight: 5, Quality: 1.0,
		MergeCommitSHA: wfMergeSHA, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id":      "revsha2",
			"message": "Revert \"feat: widget\"\n\nThis reverts commit " + wfMergeSHA + ". product decision to remove feature",
			"author":  map[string]any{"username": "alice"},
		})
	})
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.8 {
		t.Errorf("quality after strategic revert = %v, want 0.8", q)
	}
	if st.qualityUpdates[0].Reason != "revert_strategic" {
		t.Errorf("reason = %q, want revert_strategic", st.qualityUpdates[0].Reason)
	}
}

// TestHandlePush_RevertPast60dIgnored — a revert landing >60d after merge is not
// a quality signal.
func TestHandlePush_RevertPast60dIgnored(t *testing.T) {
	st := newFakeStore()
	if _, err := st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "bob", IssueID: "issue-7", Weight: 5, Quality: 1.0,
		MergeCommitSHA: wfMergeSHA, Timestamp: time.Now().UTC().Add(-90 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id":        "revsha3",
			"message":   "Revert \"feat: x\"\n\nThis reverts commit " + wfMergeSHA + ". broke prod",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"author":    map[string]any{"username": "alice"},
		})
	})
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 1.0 {
		t.Errorf("quality = %v, want 1.0 (revert outside 60d ignored)", q)
	}
	if len(st.events) != 0 {
		t.Errorf("expected no events for out-of-window revert, got %+v", st.events)
	}
}

// TestRevert_OnlyRevertedOutcomeDegraded is the §C9 scoping fix: two PRs (two
// outcomes) on ONE issue, a revert naming one merge SHA degrades ONLY that row.
// FAILS ON MAIN, where the issue-wide UPDATE hits both.
func TestRevert_OnlyRevertedOutcomeDegraded(t *testing.T) {
	st := newFakeStore()
	const sha1 = "1111111111111111111111111111111111111111"
	const sha2 = "2222222222222222222222222222222222222222"
	now := time.Now().UTC()
	// Two PRs on the same issue.
	_, _ = st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "bob", IssueID: "issue-9", Weight: 3, Quality: 1.0, MergeCommitSHA: sha1, Timestamp: now,
	})
	_, _ = st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "bob", IssueID: "issue-9", Weight: 5, Quality: 1.0, MergeCommitSHA: sha2, Timestamp: now,
	})
	// Revert names ONLY sha1's merge commit.
	captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id":      "rev-c9",
			"message": "Revert \"feat: first PR\"\n\nThis reverts commit " + sha1 + ". broke prod",
			"author":  map[string]any{"username": "alice"},
		})
	})
	if q, _ := st.qualityBySHA(sha1); q != 0.1 {
		t.Errorf("reverted outcome (sha1) quality = %v, want 0.1", q)
	}
	if q, _ := st.qualityBySHA(sha2); q != 1.0 {
		t.Errorf("sibling outcome (sha2) quality = %v, want 1.0 — must NOT be degraded (C9)", q)
	}
}

// TestHandleWorkflowRun_FlakyExactly30MinNeutralises pins the INCLUSIVE flaky
// boundary: a success exactly 30 minutes after the failure still neutralises it.
// Guards against a future `<` regression silently narrowing the window.
func TestHandleWorkflowRun_FlakyExactly30MinNeutralises(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	h := newTestHandler(st)

	failTS := time.Now().UTC().Add(-time.Hour) // within 48h of merge
	_ = doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "failure", 1, failTS), "wf-f30")
	_ = doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "success", 2, failTS.Add(30*time.Minute)), "wf-s30")
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 1.0 {
		t.Errorf("quality = %v, want 1.0 (success at exactly 30min is flaky)", q)
	}
}

// TestHandleWorkflowRun_At48hBoundaryAccepted pins the INCLUSIVE 48h window: a
// failure whose event timestamp is exactly merge+48h is still applied (only
// STRICTLY-after is ignored). Guards the `.After` boundary from an off-by-one.
func TestHandleWorkflowRun_At48hBoundaryAccepted(t *testing.T) {
	st := newFakeStore()
	mergedAt := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "alice", IssueID: "issue-42", Weight: 3, Quality: 1.0,
		MergeCommitSHA: wfMergeSHA, Timestamp: mergedAt,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := newTestHandler(st)

	// Event exactly at merge+48h.
	_ = doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "failure", 1, mergedAt.Add(48*time.Hour)), "wf-48h")
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.7 {
		t.Errorf("quality = %v, want 0.7 (event exactly at 48h is within window)", q)
	}
}

// TestHandlePush_RevertAt60dBoundaryApplied pins the INCLUSIVE 60d window: a
// revert whose commit timestamp is exactly merge+60d is still applied.
func TestHandlePush_RevertAt60dBoundaryApplied(t *testing.T) {
	st := newFakeStore()
	mergedAt := time.Now().UTC().Add(-60 * 24 * time.Hour)
	if _, err := st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "bob", IssueID: "issue-7", Weight: 5, Quality: 1.0,
		MergeCommitSHA: wfMergeSHA, Timestamp: mergedAt,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, map[string]any{
			"id":        "rev60d",
			"message":   "Revert \"feat: x\"\n\nThis reverts commit " + wfMergeSHA + ". broke prod",
			"timestamp": mergedAt.Add(60 * 24 * time.Hour).Format(time.RFC3339),
			"author":    map[string]any{"username": "alice"},
		})
	})
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.1 {
		t.Errorf("quality = %v, want 0.1 (revert exactly at 60d is within window)", q)
	}
}

// TestHandlePush_RevertReplayIdempotent directly pins the push-path replay
// mechanism (the rewritten REPLAY NOTE): doPush sets no X-GitHub-Delivery, so
// the in-memory ring never dedups — safety rests entirely on the quality_events
// unique key with source_ref = the revert commit SHA. The same commit delivered
// twice yields ONE revert_quality event, ONE quality write, and quality 0.1.
func TestHandlePush_RevertReplayIdempotent(t *testing.T) {
	st := newFakeStore()
	if _, err := st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "bob", IssueID: "issue-7", Weight: 5, Quality: 1.0,
		MergeCommitSHA: wfMergeSHA, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	commit := map[string]any{
		"id":      "revsha-replay",
		"message": "Revert \"feat: x\"\n\nThis reverts commit " + wfMergeSHA + ". broke prod",
		"author":  map[string]any{"username": "alice"},
	}
	captureLogs(func(l *slog.Logger) {
		doPush(t, st, l, commit)
		doPush(t, st, l, commit) // replayed delivery, identical content
	})

	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.1 {
		t.Errorf("quality = %v, want 0.1", q)
	}
	reverts := 0
	for _, e := range st.events {
		if e.EventType == "revert_quality" {
			reverts++
		}
	}
	if reverts != 1 {
		t.Errorf("revert_quality events = %d, want 1 (replay must not double-append)", reverts)
	}
	if len(st.qualityUpdates) != 1 {
		t.Errorf("quality writes = %d, want 1 (replay must not re-write)", len(st.qualityUpdates))
	}
}

// --- #60: fail-closed + replay protection ---

// prMergedBody builds a minimal merged-PR payload whose branch name yields
// issue-42 via issueref.FromBranchOrBody.
func prMergedBody(t *testing.T, number int, mergeSHA string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":           number,
			"merged":           true,
			"merge_commit_sha": mergeSHA,
			"head":             map[string]any{"ref": "feature/42-foo"},
			"user":             map[string]any{"login": "alice"},
			"additions":        100,
			"deletions":        20,
			"changed_files":    3,
		},
	})
	if err != nil {
		t.Fatalf("marshal PR payload: %v", err)
	}
	return body
}

// doPR posts body to h as a signed pull_request event and returns the status.
func doPR(t *testing.T, h *Handler, body []byte, deliveryID string, signature string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	if deliveryID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryID)
	}
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestServeHTTP_NoSecretFailsClosed pins the #60 posture change: an empty
// secret rejects every request instead of skipping validation. Pre-#60 this
// payload would have been processed and inserted.
func TestServeHTTP_NoSecretFailsClosed(t *testing.T) {
	st := newFakeStore()
	h := New(st, "", slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))

	code := doPR(t, h, prMergedBody(t, 7, "a1b2"), "delivery-1", "")
	if code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (fail closed without secret)", code)
	}
	if len(st.outcomes) != 0 {
		t.Errorf("outcomes inserted = %d, want 0", len(st.outcomes))
	}
}

// TestServeHTTP_SignatureValidation covers the verifySignature dispatch
// paths end-to-end: missing header, wrong scheme, bad hex, wrong key, and a
// valid signature.
func TestServeHTTP_SignatureValidation(t *testing.T) {
	body := prMergedBody(t, 7, "sha-sig-test")
	cases := []struct {
		name      string
		signature string
		want      int
	}{
		{"missing header", "", http.StatusForbidden},
		{"missing sha256= prefix", "sha1=deadbeef", http.StatusForbidden},
		{"invalid hex", "sha256=not-hex!", http.StatusForbidden},
		{"wrong secret", sign("some-other-secret", body), http.StatusForbidden},
		{"truncated mac", "sha256=deadbeef", http.StatusForbidden},
		{"valid", sign(testSecret, body), http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			h := New(st, testSecret, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
			if code := doPR(t, h, body, "", tc.signature); code != tc.want {
				t.Errorf("status = %d, want %d", code, tc.want)
			}
			wantOutcomes := 0
			if tc.want == http.StatusNoContent {
				wantOutcomes = 1
			}
			if len(st.outcomes) != wantOutcomes {
				t.Errorf("outcomes = %d, want %d", len(st.outcomes), wantOutcomes)
			}
		})
	}
}

// TestServeHTTP_DuplicateDeliverySkipped pins the in-memory dedup: GitHub
// redelivers with the same X-GitHub-Delivery GUID; the second delivery must
// 204 (so GitHub records success) without re-processing.
func TestServeHTTP_DuplicateDeliverySkipped(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
	body := prMergedBody(t, 7, "sha-dup-delivery")
	sig := sign(testSecret, body)

	for i := 0; i < 2; i++ {
		if code := doPR(t, h, body, "delivery-same", sig); code != http.StatusNoContent {
			t.Fatalf("request %d: status = %d, want 204", i, code)
		}
	}
	if len(st.outcomes) != 1 {
		t.Errorf("outcomes = %d, want 1 (duplicate delivery must not re-insert)", len(st.outcomes))
	}
}

// TestHandlePR_ReplayWithFreshDeliveryIDDeduped pins the durable guard: the
// HMAC covers only the body, so an attacker can replay a captured request
// with a different X-GitHub-Delivery header. The merge_commit_sha lookup
// must stop the double-insert.
func TestHandlePR_ReplayWithFreshDeliveryIDDeduped(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
	body := prMergedBody(t, 7, "sha-replay")
	sig := sign(testSecret, body)

	if code := doPR(t, h, body, "delivery-original", sig); code != http.StatusNoContent {
		t.Fatalf("original: status = %d, want 204", code)
	}
	if code := doPR(t, h, body, "delivery-forged-fresh", sig); code != http.StatusNoContent {
		t.Fatalf("replay: status = %d, want 204", code)
	}
	if len(st.outcomes) != 1 {
		t.Errorf("outcomes = %d, want 1 (replay with fresh delivery ID must dedup on merge SHA)", len(st.outcomes))
	}
	if len(st.outcomes) == 1 {
		o := st.outcomes[0]
		if o.Developer != "alice" || o.IssueID != "issue-42" || o.MergeCommitSHA != "sha-replay" {
			t.Errorf("outcome = %+v, want alice/issue-42/sha-replay", o)
		}
	}
}

// TestDeliverySet_RingEviction pins the bounded-memory contract: when the
// ring wraps, the oldest GUID is forgotten (a very late redelivery would
// re-process — accepted; the content-level guard catches it) and the set
// never exceeds its capacity.
func TestDeliverySet_RingEviction(t *testing.T) {
	d := newDeliverySet(3)
	for _, id := range []string{"a", "b", "c"} {
		if !d.firstSeen(id) {
			t.Fatalf("first sighting of %q reported as duplicate", id)
		}
	}
	if d.firstSeen("a") {
		t.Error("a within window reported as first sighting")
	}
	// d wraps: "d" evicts "a" (oldest).
	if !d.firstSeen("d") {
		t.Error("d is new, want first sighting")
	}
	if !d.firstSeen("a") {
		t.Error("a was evicted by the wrap, want re-admitted as first sighting")
	}
	if len(d.seen) > 3 {
		t.Errorf("set size = %d, want <= 3", len(d.seen))
	}
}

// TestServeHTTP_FailedHandlerDoesNotSuppressRetry pins the forget-on-error
// contract: when processing fails (500 → GitHub retries), the SAME delivery
// GUID must be reprocessed on retry rather than skipped as a duplicate —
// otherwise the outcome is permanently lost.
func TestServeHTTP_FailedHandlerDoesNotSuppressRetry(t *testing.T) {
	st := newFakeStore()
	st.insertErrs = 1 // first insert fails transiently
	h := New(st, testSecret, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
	body := prMergedBody(t, 7, "sha-retry")
	sig := sign(testSecret, body)

	if code := doPR(t, h, body, "delivery-retry", sig); code != http.StatusInternalServerError {
		t.Fatalf("failing delivery: status = %d, want 500", code)
	}
	// GitHub retries the exact same delivery GUID.
	if code := doPR(t, h, body, "delivery-retry", sig); code != http.StatusNoContent {
		t.Fatalf("retry: status = %d, want 204", code)
	}
	if got := st.outcomeCount(); got != 1 {
		t.Errorf("outcomes = %d, want 1 (retry after failure must be processed)", got)
	}
}

// TestServeHTTP_SameGUIDDifferentEventBothProcessed pins the composite dedup
// key: a GUID reused across event types (manual redelivery tooling, test
// clients) must not suppress the other event.
func TestServeHTTP_SameGUIDDifferentEventBothProcessed(t *testing.T) {
	st := newFakeStore()
	// Seed an outcome so the push's revert lookup resolves (within the 60d
	// revert window so the quality event applies).
	_, _ = st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "bob", IssueID: "issue-42", PRNumber: 1, Weight: 5, Quality: 1.0,
		Timestamp: time.Now().UTC(),
	})
	h := New(st, testSecret, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))

	pushBody, err := json.Marshal(map[string]any{"commits": []map[string]any{{
		"id":      "rev1",
		"message": `Revert "feat: add foo (closes #42)"`,
		"author":  map[string]any{"username": "alice"},
	}}})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(pushBody))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "guid-shared")
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, pushBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("push: status = %d, want 204", rec.Code)
	}

	prBody := prMergedBody(t, 9, "sha-shared-guid")
	if code := doPR(t, h, prBody, "guid-shared", sign(testSecret, prBody)); code != http.StatusNoContent {
		t.Fatalf("pull_request with reused GUID: status = %d, want 204", code)
	}
	if got := st.outcomeCount(); got != 2 {
		t.Errorf("outcomes = %d, want 2 (seed + PR; reused GUID on a different event must not dedup)", got)
	}
	if len(st.qualityUpdates) != 1 {
		t.Errorf("quality updates = %d, want 1 (push must have been processed)", len(st.qualityUpdates))
	}
}

// TestServeHTTP_ConcurrentSameSHAReplays drives N concurrent replays of the
// same signed body with FRESH delivery GUIDs (bypassing the in-memory ring
// by design) and asserts exactly one outcome lands. The fakeStore mirrors
// the real store's unique-index ON CONFLICT DO NOTHING, which is the atomic
// boundary; the handler's read-then-insert alone is TOCTOU.
func TestServeHTTP_ConcurrentSameSHAReplays(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
	body := prMergedBody(t, 7, "sha-concurrent-replay")
	sig := sign(testSecret, body)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
			req.Header.Set("X-GitHub-Event", "pull_request")
			req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("forged-guid-%d", i))
			req.Header.Set("X-Hub-Signature-256", sig)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Errorf("request %d: status = %d, want 204", i, rec.Code)
			}
		}(i)
	}
	wg.Wait()
	if got := st.outcomeCount(); got != 1 {
		t.Errorf("outcomes = %d, want 1 (concurrent replays must collapse on the unique merge SHA)", got)
	}
}

// TestDeliverySet_ConcurrentFirstSeen locks in the mutex contract: exactly
// one of N concurrent sightings of the same GUID wins.
func TestDeliverySet_ConcurrentFirstSeen(t *testing.T) {
	d := newDeliverySet(deliveryWindow)
	const n = 32
	var wg sync.WaitGroup
	var wins atomic.Int32
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if d.firstSeen("same-guid") {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("firstSeen wins = %d, want exactly 1", wins.Load())
	}
}

// TestDeliverySet_ForgetReadmitsAndKeepsRingConsistent pins forget's ring
// hygiene: after forget + re-admission into a new slot, the OLD slot's
// eventual eviction must not delete the live entry.
func TestDeliverySet_ForgetReadmitsAndKeepsRingConsistent(t *testing.T) {
	d := newDeliverySet(3)
	if !d.firstSeen("x") {
		t.Fatal("x: want first sighting")
	}
	d.forget("x")
	if !d.firstSeen("x") {
		t.Error("x after forget: want re-admitted")
	}
	// Fill the ring so the slot x originally occupied (now cleared) and
	// then x's new slot cycle through. If forget had left the stale "x"
	// string in slot 0, this wrap would delete the live entry.
	for _, id := range []string{"a", "b", "c"} {
		d.firstSeen(id)
	}
	// x's new slot has now been evicted by the wrap — x should be
	// re-admittable again, and the set must stay consistent (no panic, no
	// phantom entries).
	if !d.firstSeen("x") {
		t.Error("x after full wrap: want first sighting again (evicted by wrap)")
	}
	if len(d.seen) > 3 {
		t.Errorf("set size = %d, want <= 3", len(d.seen))
	}
}

// --- #137: raw webhook payload audit trail ---

// TestServeHTTP_PersistsRawPayload asserts a signed, processed pull_request
// delivery persists the exact raw body under the (event, delivery id) it came
// in on.
func TestServeHTTP_PersistsRawPayload(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger())
	body := prMergedBody(t, 1, "sha-persist-1")
	if code := doPR(t, h, body, "deliv-abc", sign(testSecret, body)); code != http.StatusNoContent {
		t.Fatalf("doPR returned %d, want 204", code)
	}

	got := st.persistedPayloads()
	if len(got) != 1 {
		t.Fatalf("persisted payloads = %d, want 1", len(got))
	}
	if got[0].Event != "pull_request" || got[0].DeliveryID != "deliv-abc" {
		t.Errorf("persisted (event,delivery) = (%s,%s), want (pull_request,deliv-abc)", got[0].Event, got[0].DeliveryID)
	}
	if !bytes.Equal(got[0].Body, body) {
		t.Errorf("persisted body mismatch: got %d bytes, want %d", len(got[0].Body), len(body))
	}
	// The outcome must still have been processed.
	if st.outcomeCount() != 1 {
		t.Errorf("outcomes = %d, want 1", st.outcomeCount())
	}
}

// TestServeHTTP_PayloadPersistFailureDoesNotFailWebhook asserts audit persistence
// is best-effort: an InsertWebhookPayload error is swallowed, the PR is still
// processed, and the delivery returns 204.
func TestServeHTTP_PayloadPersistFailureDoesNotFailWebhook(t *testing.T) {
	st := newFakeStore()
	st.payloadErr = errors.New("disk full")
	h := New(st, testSecret, quietLogger())
	body := prMergedBody(t, 2, "sha-persist-fail")
	if code := doPR(t, h, body, "deliv-fail", sign(testSecret, body)); code != http.StatusNoContent {
		t.Fatalf("doPR returned %d, want 204 (persist failure must not fail the webhook)", code)
	}
	if len(st.persistedPayloads()) != 0 {
		t.Errorf("persisted payloads = %d, want 0 (insert failed)", len(st.persistedPayloads()))
	}
	if st.outcomeCount() != 1 {
		t.Errorf("outcomes = %d, want 1 (PR must still be processed)", st.outcomeCount())
	}
}

// TestServeHTTP_IgnoredEventsNotPersisted asserts an unprocessed event type
// (issue_comment) leaves no audit row — only processed events are retained.
func TestServeHTTP_IgnoredEventsNotPersisted(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger())
	body := []byte(`{"action":"created"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-GitHub-Delivery", "deliv-ic")
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("issue_comment returned %d, want 204", rec.Code)
	}
	if n := len(st.persistedPayloads()); n != 0 {
		t.Errorf("persisted payloads = %d, want 0 (ignored event must not be retained)", n)
	}
}

// quietLogger returns a logger that discards output, for tests that don't assert
// on log content.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))
}

// --- #196 push-to-default-branch capture ---

// fakePushCounter is a minimal PushUnattributedCounter for capture tests: it
// counts Inc calls so a test can assert an unattributed commit was OBSERVED
// (counted), not silently dropped.
type fakePushCounter struct{ n atomic.Int64 }

func (c *fakePushCounter) Inc(_ ...string) { c.n.Add(1) }
func (c *fakePushCounter) count() int64    { return c.n.Load() }

// pushCommit builds one push-payload commit object.
func pushCommit(sha, msg, author string, ts time.Time) map[string]any {
	return map[string]any{
		"id":        sha,
		"message":   msg,
		"timestamp": ts.Format(time.RFC3339),
		"author":    map[string]any{"username": author},
	}
}

// dispatchPush drives one push delivery (ref + repository.default_branch + commits)
// through h and asserts a 204. No X-GitHub-Delivery header is set, so each call is
// processed without the in-memory delivery dedup — replay idempotency is therefore
// exercised at the store layer (the real backstop), which is the point.
func dispatchPush(t *testing.T, h *Handler, ref, defaultBranch string, commits ...map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"ref":        ref,
		"repository": map[string]any{"default_branch": defaultBranch},
		"commits":    commits,
	})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("push returned %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

// pushSource counts outcomes recorded with source='push' (the capture rows).
func (r *fakeStore) pushSource() []store.Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []store.Outcome
	for _, o := range r.outcomes {
		if o.Source == store.OutcomeSourcePush {
			out = append(out, o)
		}
	}
	return out
}

// TestHandlePush_CaptureDisabled pins the config gate: with push capture OFF (the
// default handler), a direct commit that closes an issue on the default branch
// produces NO outcome — handlePush is reverts-only, exactly as pre-#196.
func TestHandlePush_CaptureDisabled(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger()) // no WithPushCapture
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommit("sha1", "feat: add flag (closes #42)", "alice", ts))
	if got := st.outcomeCount(); got != 0 {
		t.Fatalf("capture disabled: outcomeCount = %d, want 0 (reverts-only)", got)
	}
}

// TestHandlePush_CaptureSingleCommit pins the happy path: a qualifying direct
// commit to the default branch becomes ONE degraded outcome — weight 0.5,
// weight_source='push', source='push', attributed to the commit author + issue.
func TestHandlePush_CaptureSingleCommit(t *testing.T) {
	st := newFakeStore()
	ctr := &fakePushCounter{}
	h := New(st, testSecret, quietLogger(), WithPushCapture(ctr))
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommit("sha1", "feat: add flag (closes #42)", "alice", ts))

	got := st.pushSource()
	if len(got) != 1 {
		t.Fatalf("push outcomes = %d, want 1", len(got))
	}
	o := got[0]
	if o.Weight != 0.5 {
		t.Errorf("Weight = %v, want 0.5 (degraded floor)", o.Weight)
	}
	if o.WeightSource != store.WeightSourcePush {
		t.Errorf("WeightSource = %q, want %q", o.WeightSource, store.WeightSourcePush)
	}
	if o.Source != store.OutcomeSourcePush {
		t.Errorf("Source = %q, want %q", o.Source, store.OutcomeSourcePush)
	}
	if o.Developer != "alice" {
		t.Errorf("Developer = %q, want alice", o.Developer)
	}
	if o.IssueID != "issue-42" {
		t.Errorf("IssueID = %q, want issue-42", o.IssueID)
	}
	if o.Quality != 1.0 {
		t.Errorf("Quality = %v, want 1.0", o.Quality)
	}
	if ctr.count() != 0 {
		t.Errorf("unattributed counter = %d, want 0 on the happy path", ctr.count())
	}
}

// TestHandlePush_CaptureAggregatesPerIssuePerDay pins RULING B: MANY qualifying
// commits sharing an issue within one UTC day collapse to ONE 0.5 outcome (never
// 0.5×N — that is the commit-splitting inflation vector the grain closes), and a
// full replay of the same push adds nothing.
func TestHandlePush_CaptureAggregatesPerIssuePerDay(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))
	day := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	commits := []map[string]any{
		pushCommit("sha1", "feat: part 1 (closes #42)", "alice", day),
		pushCommit("sha2", "feat: part 2 closes #42", "alice", day.Add(3*time.Hour)),
		pushCommit("sha3", "feat: part 3 fixes #42", "bob", day.Add(6*time.Hour)),
	}
	dispatchPush(t, h, "refs/heads/main", "main", commits...)
	if got := len(st.pushSource()); got != 1 {
		t.Fatalf("after 3 same-issue same-day commits: push outcomes = %d, want 1 (aggregated, NOT summed)", got)
	}
	// Replay the identical push: idempotent, still exactly one.
	dispatchPush(t, h, "refs/heads/main", "main", commits...)
	if got := len(st.pushSource()); got != 1 {
		t.Fatalf("after replay: push outcomes = %d, want 1 (idempotent)", got)
	}
}

// TestHandlePush_CaptureUTCMidnightBoundary pins that the aggregation day is a UTC
// calendar day: two commits on the same issue straddling UTC midnight are TWO
// distinct outcomes (one per day), not one.
func TestHandlePush_CaptureUTCMidnightBoundary(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))
	beforeMidnight := time.Date(2026, 7, 8, 23, 30, 0, 0, time.UTC)
	afterMidnight := time.Date(2026, 7, 9, 0, 30, 0, 0, time.UTC)
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommit("sha1", "feat: late (closes #42)", "alice", beforeMidnight),
		pushCommit("sha2", "feat: early (closes #42)", "alice", afterMidnight))
	if got := len(st.pushSource()); got != 2 {
		t.Fatalf("commits straddling UTC midnight: push outcomes = %d, want 2 (one per UTC day)", got)
	}
}

// TestHandlePush_CaptureSquashDedup pins constraint #1: a squash-merged PR emits
// both the pull_request webhook (which already stored an outcome under the squash
// commit SHA) AND a push carrying that same SHA. The push must SKIP it — no
// duplicate outcome.
func TestHandlePush_CaptureSquashDedup(t *testing.T) {
	st := newFakeStore()
	// Seed the PR-path outcome the squash merge already produced.
	if _, err := st.InsertOutcome(context.Background(), store.Outcome{
		Developer: "bob", IssueID: "issue-7", Weight: 3.0,
		WeightSource: store.WeightSourceLabel, Quality: 1.0,
		MergeCommitSHA: "squashsha", Source: store.OutcomeSourceGitHubWebhook,
	}); err != nil {
		t.Fatalf("seed PR outcome: %v", err)
	}
	h := New(st, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	// The push carries the squash commit SHA; its message even names a (different)
	// issue, to prove the SKIP is driven by the SHA match, not the issue.
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommit("squashsha", "feat: squashed (closes #99)", "bob", ts))
	if got := len(st.pushSource()); got != 0 {
		t.Fatalf("squash double-fire: push outcomes = %d, want 0 (PR path already captured)", got)
	}
	if got := st.outcomeCount(); got != 1 {
		t.Fatalf("total outcomes = %d, want 1 (only the seeded PR outcome)", got)
	}
}

// TestHandlePush_CaptureSkipsMergeCommit pins constraint #2: a 2-parent merge
// commit (git's canonical "Merge …" subject) is skipped even when its message
// carries an issue ref — merge commits arrive via the PR webhook, not here.
func TestHandlePush_CaptureSkipsMergeCommit(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	for _, subj := range []string{
		"Merge pull request #123 from acme/feature\n\ncloses #42",
		"Merge branch 'feature' into main",
		"Merge remote-tracking branch 'origin/main'",
	} {
		dispatchPush(t, h, "refs/heads/main", "main", pushCommit("m1", subj, "alice", ts))
	}
	if got := len(st.pushSource()); got != 0 {
		t.Fatalf("merge commits: push outcomes = %d, want 0 (skipped)", got)
	}
}

// TestHandlePush_CaptureUnattributedIsObservable pins constraint #6: a direct
// commit with no resolvable issue (or no author login) creates NO outcome but is
// OBSERVABLE — a log line AND a counter increment, never a silent drop.
func TestHandlePush_CaptureUnattributedIsObservable(t *testing.T) {
	st := newFakeStore()
	ctr := &fakePushCounter{}
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	logs := captureLogs(func(logger *slog.Logger) {
		h := New(st, testSecret, logger, WithPushCapture(ctr))
		dispatchPush(t, h, "refs/heads/main", "main",
			pushCommit("sha1", "chore: tidy up with no issue reference", "alice", ts))
	})
	if got := len(st.pushSource()); got != 0 {
		t.Fatalf("unattributed commit: push outcomes = %d, want 0", got)
	}
	if ctr.count() != 1 {
		t.Fatalf("unattributed counter = %d, want 1 (observable)", ctr.count())
	}
	if !strings.Contains(logs, "not scored") {
		t.Errorf("expected an unattributed log line, got: %q", logs)
	}
}

// TestHandlePush_CaptureIgnoresNonDefaultBranch pins that only the default branch
// is captured: a push to a feature branch is left for its PR to score.
func TestHandlePush_CaptureIgnoresNonDefaultBranch(t *testing.T) {
	st := newFakeStore()
	ctr := &fakePushCounter{}
	h := New(st, testSecret, quietLogger(), WithPushCapture(ctr))
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	dispatchPush(t, h, "refs/heads/feature/42-foo", "main",
		pushCommit("sha1", "feat: wip (closes #42)", "alice", ts))
	if got := st.outcomeCount(); got != 0 {
		t.Fatalf("non-default branch: outcomeCount = %d, want 0 (ignored)", got)
	}
	if ctr.count() != 0 {
		t.Errorf("non-default branch must not even reach the attribution check; counter = %d, want 0", ctr.count())
	}
}

// TestHandlePush_CaptureSkipsReverts pins that a revert on the default branch is
// handled by the revert path (quality degradation on the ORIGINAL outcome) and is
// NOT captured as a new productive outcome.
func TestHandlePush_CaptureSkipsReverts(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommit("r1", "Revert \"feat: add flag\" (closes #42)", "alice", ts))
	if got := len(st.pushSource()); got != 0 {
		t.Fatalf("revert commit: push outcomes = %d, want 0 (not new work)", got)
	}
}

// TestHandlePR_RecordsWorkType pins the end-to-end wiring: a merged PR carrying a
// `security` label stores work_type=security/source=label, a `type:incident` label
// stores incident/label, and an unlabeled PR falls back to feature/default (#187).
func TestHandlePR_RecordsWorkType(t *testing.T) {
	prBody := func(t *testing.T, num int, sha, ref string, labels []map[string]any) []byte {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"action": "closed",
			"pull_request": map[string]any{
				"number":           num,
				"merged":           true,
				"merge_commit_sha": sha,
				"head":             map[string]any{"ref": ref},
				"user":             map[string]any{"login": "alice"},
				"labels":           labels,
				"additions":        10, "deletions": 2, "changed_files": 1,
			},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return body
	}

	cases := []struct {
		name       string
		labels     []map[string]any
		issue      string
		ref        string
		sha        string
		wantType   string
		wantSource string
	}{
		{"security label", []map[string]any{{"name": "security"}}, "issue-10", "feature/10-x", "sha-sec", store.WorkTypeSecurity, store.WorkTypeSourceLabel},
		{"type:incident label", []map[string]any{{"name": "type:incident"}}, "issue-11", "feature/11-y", "sha-inc", store.WorkTypeIncident, store.WorkTypeSourceLabel},
		{"no label -> feature/default", nil, "issue-12", "feature/12-z", "sha-plain", store.WorkTypeFeature, store.WorkTypeSourceDefault},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newFakeStore()
			h := New(st, testSecret, quietLogger())
			body := prBody(t, 10, c.sha, c.ref, c.labels)
			if code := doPR(t, h, body, "d-"+c.name, sign(testSecret, body)); code != http.StatusNoContent {
				t.Fatalf("doPR status = %d, want 204", code)
			}
			o, ok, _ := st.LatestOutcomeByIssue(context.Background(), "", c.issue)
			if !ok {
				t.Fatal("no outcome recorded")
			}
			if o.WorkType != c.wantType || o.WorkTypeSource != c.wantSource {
				t.Errorf("work_type = (%q,%q), want (%q,%q)", o.WorkType, o.WorkTypeSource, c.wantType, c.wantSource)
			}
		})
	}
}

// --- Generated-file exclusion (#240, audit C4) ---

// pushCommitFiles is pushCommit plus the GitHub push payload's per-commit changed-
// file arrays (added/removed/modified), which the generated-file exclusion inspects.
func pushCommitFiles(sha, msg, author string, ts time.Time, added, removed, modified []string) map[string]any {
	c := pushCommit(sha, msg, author, ts)
	c["added"] = added
	c["removed"] = removed
	c["modified"] = modified
	return c
}

// TestIsGeneratedPath pins the deny-list matcher's three pattern shapes (#240):
// a trailing "/" is a directory prefix (root OR nested), a leading "*" is a
// filename suffix, and a bare token is an exact basename anywhere in the tree.
func TestIsGeneratedPath(t *testing.T) {
	pats := defaultGeneratedPaths
	cases := []struct {
		path string
		want bool
	}{
		{"vendor/modules.txt", true},             // root dir prefix
		{"third_party/vendor/x/y.go", true},      // nested dir prefix
		{"node_modules/left-pad/index.js", true}, // dir prefix
		{"api/v1/service.pb.go", true},           // *.pb.go suffix
		{"internal/store/schema_generated.go", true},
		{"internal/foo.gen.go", true},
		{"go.sum", true},              // exact basename at root
		{"services/api/go.sum", true}, // exact basename nested
		{"web/package-lock.json", true},
		{"Cargo.lock", true},
		{"internal/webhook/handler.go", false}, // real source
		{"README.md", false},
		{"cmd/tierd/main.go", false},
		{"vendored_helpers.go", false},  // "vendor" not a dir segment
		{"api/v1/service_pb.go", false}, // "_pb.go" is not ".pb.go"
		{"go.mod", false},               // go.mod is authored, not generated
	}
	for _, c := range cases {
		if got := isGeneratedPath(c.path, pats); got != c.want {
			t.Errorf("isGeneratedPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestIsGeneratedPath_EmptyPatternsMatchNothing pins that an explicit empty deny-
// list disables exclusion entirely (an operator's `generated_paths: []`).
func TestIsGeneratedPath_EmptyPatternsMatchNothing(t *testing.T) {
	if isGeneratedPath("vendor/x.go", nil) {
		t.Error("nil patterns should match nothing")
	}
	if isGeneratedPath("go.sum", []string{}) {
		t.Error("empty patterns should match nothing")
	}
}

// TestHandlePush_CaptureSkipsGeneratedOnlyCommit pins #240 / audit C4: a direct
// commit to the default branch whose EVERY changed file is generated/vendored
// (regenerated protobuf, `go mod vendor`, a lockfile bump) carries no engineering
// outcome and must NOT earn a degraded push outcome, even though it names an issue.
func TestHandlePush_CaptureSkipsGeneratedOnlyCommit(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommitFiles("gen1", "chore: regen protobuf (closes #42)", "alice", ts,
			[]string{"api/v1/service.pb.go"}, nil, []string{"go.sum", "vendor/modules.txt"}))
	if got := len(st.pushSource()); got != 0 {
		t.Fatalf("all-generated commit: push outcomes = %d, want 0 (excluded, #240)", got)
	}
}

// TestHandlePush_CaptureMixedFilesStillCaptured pins that a commit touching even
// ONE non-generated file is still real work — the exclusion is all-or-nothing and
// must never suppress a commit that changed authored source.
func TestHandlePush_CaptureMixedFilesStillCaptured(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommitFiles("mix1", "feat: add endpoint (closes #42)", "alice", ts,
			[]string{"api/v1/service.pb.go", "internal/api/handler.go"}, nil, []string{"go.sum"}))
	if got := len(st.pushSource()); got != 1 {
		t.Fatalf("mixed commit: push outcomes = %d, want 1 (has authored source)", got)
	}
}

// TestHandlePush_CaptureNoFileArraysUnaffected pins the safety gate: a push payload
// that omits the per-commit file arrays (minimal senders, older fixtures) behaves
// EXACTLY as before — the exclusion only ever removes a spurious outcome on real
// file data, never suppresses one when the data is absent.
func TestHandlePush_CaptureNoFileArraysUnaffected(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommit("nofiles", "feat: add flag (closes #42)", "alice", ts))
	if got := len(st.pushSource()); got != 1 {
		t.Fatalf("no file arrays: push outcomes = %d, want 1 (unchanged behavior)", got)
	}
}

// TestWithGeneratedPaths_OverridesDefault pins that WithGeneratedPaths replaces the
// built-in deny-list: with an empty override, an otherwise all-generated commit is
// captured because nothing is treated as generated.
func TestWithGeneratedPaths_OverridesDefault(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger(),
		WithPushCapture(&fakePushCounter{}), WithGeneratedPaths([]string{}))
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommitFiles("gen2", "chore: regen (closes #42)", "alice", ts,
			[]string{"api/v1/service.pb.go"}, nil, []string{"go.sum"}))
	if got := len(st.pushSource()); got != 1 {
		t.Fatalf("empty override: push outcomes = %d, want 1 (exclusion disabled)", got)
	}
}

// TestHandlePush_GeneratedSkipLogNotForgeable is a security regression guard (#240,
// CodeQL go/log-injection; sibling of TestLogSafeErr and
// TestProxy_RejectedRepoHeaderIsNotLogged).
//
// A push payload's commit id is attacker-controlled JSON. The generated-file skip
// log (#240) emits that id, so a raw value carrying a newline could forge a second,
// standalone log record ("<sha>\ntime=... level=ERROR msg=...") that an operator or
// a line-oriented SIEM reads as genuine. logSafeStr must strip the newline (#321): the
// diagnostic survives on ONE record, and no forged record ever appears on its own line.
func TestHandlePush_GeneratedSkipLogNotForgeable(t *testing.T) {
	st := newFakeStore()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := New(st, testSecret, logger, WithPushCapture(&fakePushCounter{}))
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	forgedID := "evilsha\ntime=2026-07-12T00:00:00Z " + forgedMarker
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommitFiles(forgedID, "chore: regen protobuf (closes #42)", "alice", ts,
			[]string{"api/v1/service.pb.go"}, nil, []string{"go.sum"}))

	// The commit must have been excluded (all files generated) — that is the path
	// that reaches the flagged log statement.
	if got := len(st.pushSource()); got != 0 {
		t.Fatalf("all-generated commit: push outcomes = %d, want 0 (excluded, #240)", got)
	}
	logs := buf.String()
	// The skip must stay observable — the sanitizer must not destroy the diagnostic.
	if !strings.Contains(logs, "all changed files are generated") {
		t.Fatalf("generated-file skip was not logged; diagnostic lost:\n%s", logs)
	}
	// The forged record must never appear on its own line: the embedded newline has
	// to be stripped, not emitted raw.
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("commit id forged a standalone log record — log injection:\n%s", logs)
		}
	}
	// And confirm the CR/LF was STRIPPED (the CodeQL-recognized barrier, #321), not
	// emitted raw: the two halves of the commit id are joined onto the field's single
	// line ("evilsha"+"time="), and the `commit="\"` prefix pins that the value went
	// through logSafeStr's %q, not slog's bare quoting. Removing the sanitizer makes
	// this fail (slog would render `commit="evilsha\ntime=...` on a wrapped line).
	if !strings.Contains(logs, `commit="\"evilshatime=`) {
		t.Errorf("expected the commit-id CR/LF stripped and value rendered via logSafeStr:\n%s", logs)
	}
}

// TestHandlePush_RevertLogNotForgeable is a security regression guard (#288,
// CodeQL go/log-injection; sibling of TestHandlePush_GeneratedSkipLogNotForgeable,
// TestLogSafeErr and TestProxy_RejectedRepoHeaderIsNotLogged).
//
// A push payload's commit author username and commit id are attacker-controlled
// JSON: GitHub signs the delivery, but a malicious contributor fully controls
// their own git author name and can embed a newline in it. The "revert detected
// but no issue id derivable" diagnostic (#20) logs the commit id, subject, and
// author verbatim, so a raw author name (or id) carrying a newline could forge a
// second, standalone log record ("<forged>\ntime=... level=ERROR msg=...") that an
// operator or a line-oriented SIEM reads as genuine. logSafeStr must strip the
// newline (#321): the diagnostic survives on ONE record, and no forged record ever
// appears on its own line.
//
// This test PINS THE MECHANISM, not just the property. slog's TextHandler already
// auto-quotes a value containing a newline, so the "no standalone forged record"
// check alone would still pass with logSafeStr removed. To force a failure when the
// sanitizer is stripped, we also assert the exact strip-then-%q-then-slog rendering
// (`author="\"evetime=...`, `commit="\"evilshatime=...`): the escaped-quote
// immediately after slog's opening quote is logSafeStr's %q signature (the same
// double-escape TestHandlePush_LogsAuthor pins), which slog's own single quoting
// never produces (it renders a bare value as `author="eve\ntime=...`), and the
// joined "evetime="/"evilshatime=" proves the CR/LF was removed, not escaped.
func TestHandlePush_RevertLogNotForgeable(t *testing.T) {
	st := newFakeStore()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := New(st, testSecret, logger)
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	forgedAuthor := "eve\ntime=2026-07-12T00:00:00Z " + forgedMarker
	forgedID := "evilsha\ntime=2026-07-12T00:00:00Z " + forgedMarker

	// A revert commit whose message names no issue and carries no "This reverts
	// commit <sha>" footer: neither resolution tier matches, so it reaches the
	// "no issue id derivable" log - the sink that emits commit id, subject, author.
	dispatchPush(t, h, "refs/heads/main", "main",
		pushCommit(forgedID, `Revert "a change with no issue reference"`, forgedAuthor, ts))

	logs := buf.String()
	// The diagnostic must survive - sanitization must not destroy it.
	if !strings.Contains(logs, "revert detected but no issue id derivable") {
		t.Fatalf("revert-no-issue diagnostic was not logged; lost:\n%s", logs)
	}
	// The forged record must never appear on its own line: the embedded newline in
	// the author name (or commit id) has to be stripped, not emitted raw.
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("attacker-controlled field forged a standalone log record - log injection:\n%s", logs)
		}
	}
	// MECHANISM PIN + STRIP PROOF: assert the exact logSafeStr rendering AND that the
	// CR/LF was STRIPPED (the CodeQL-recognized barrier, #321), not emitted raw.
	// logSafeStr strips "\n"/"\r" then %q-wraps, so a sanitized value renders as
	// `key="\"<joined-value>...` - the `"\"` (opening slog quote immediately followed
	// by an escaped quote) is a signature slog's own quoting cannot produce for a bare
	// value (it would render `author="eve\ntime=...` on a wrapped line). The joined
	// "evetime="/"evilshatime=" text (the forged "\ntime=" rider glued onto the field)
	// proves the newline was removed, not escaped. Removing logSafeStr from the author
	// or commit arg of the tier-3 sink makes these fail.
	if !strings.Contains(logs, `author="\"evetime=`) {
		t.Errorf("author not stripped+rendered via logSafeStr (expected `author=\"\\\"evetime=...`):\n%s", logs)
	}
	if !strings.Contains(logs, `commit="\"evilshatime=`) {
		t.Errorf("commit id not stripped+rendered via logSafeStr (expected `commit=\"\\\"evilshatime=...`):\n%s", logs)
	}
}

// TestServeHTTP_DeliveryHeaderNotForgeable is a security regression guard (#288,
// CodeQL go/log-injection). The X-GitHub-Delivery header is the one fully
// attacker-controlled input that reaches a log line WITHOUT knowing the secret:
// verifySignature covers the request BODY only, so a replayed-but-validly-signed
// body carrying a newline-bearing delivery header lands on the duplicate-delivery
// Debug sink. logSafeStr must strip the newline (#321) so the forged record never
// appears on its own line while the diagnostic survives.
//
// Like TestHandlePush_RevertLogNotForgeable, this PINS THE MECHANISM: slog would
// auto-quote a newline-bearing value on its own, so the property checks alone still
// pass with logSafeStr removed. The `delivery="\"guid...` assertion below asserts
// logSafeStr's %q-then-slog-quote signature, which fails if the sanitizer is
// stripped from the dedup-skip sink (slog alone renders `delivery="guid...`).
func TestServeHTTP_DeliveryHeaderNotForgeable(t *testing.T) {
	st := newFakeStore()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := New(st, testSecret, logger)

	body, err := json.Marshal(map[string]any{"commits": []map[string]any{}})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	sig := sign(testSecret, body)

	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	forgedDelivery := "guid\ntime=2026-07-12T00:00:00Z " + forgedMarker

	// The first delivery seeds the in-memory dedup ring; the second, identical
	// delivery is skipped - that skip is the path that logs the (forged) delivery id.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", forgedDelivery)
		req.Header.Set("X-Hub-Signature-256", sig)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("delivery %d: status = %d, want 204; body=%s", i, rec.Code, rec.Body.String())
		}
	}

	logs := buf.String()
	// The dedup diagnostic must survive - sanitization must not destroy it.
	if !strings.Contains(logs, "duplicate webhook delivery skipped") {
		t.Fatalf("duplicate-delivery diagnostic was not logged; lost:\n%s", logs)
	}
	// The forged record must never appear on its own line: the delivery-header
	// newline has to be stripped, not emitted raw.
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("delivery header forged a standalone log record - log injection:\n%s", logs)
		}
	}
	// MECHANISM PIN + STRIP PROOF: assert logSafeStr's %q-then-slog-quote rendering AND
	// that the CR/LF was STRIPPED (the CodeQL-recognized barrier, #321). A sanitized
	// value renders as `delivery="\"guidtime=...`; slog's own quoting of a bare value
	// would render `delivery="guid\ntime=...` on a wrapped line. The joined "guidtime="
	// text (the forged "\ntime=" rider glued onto the field) proves the newline was
	// removed, not escaped. Removing logSafeStr from the dedup-skip sink makes this fail.
	if !strings.Contains(logs, `delivery="\"guidtime=`) {
		t.Errorf("delivery id not stripped+rendered via logSafeStr (expected `delivery=\"\\\"guidtime=...`):\n%s", logs)
	}
}

// TestSizeWeight_DefaultsWhenUnconfigured pins that a Handler with no configured
// size_labels (the default) resolves weights via the built-in prderive default
// table — an absent config key is a no-op, so existing adopters are unchanged
// (#244).
func TestSizeWeight_DefaultsWhenUnconfigured(t *testing.T) {
	h := New(nil, testSecret, nil)
	cases := []struct {
		labels []prLabel
		want   float64
	}{
		{[]prLabel{{Name: "size/XS"}}, 0.5},
		{[]prLabel{{Name: "size/M"}}, 3.0},
		{[]prLabel{{Name: "XL"}}, 8.0},
		{[]prLabel{{Name: "enhancement"}}, 0},
	}
	for _, c := range cases {
		if got := sizeWeightVia(h, c.labels); got != c.want {
			t.Errorf("sizeWeight(%v) = %v, want %v (built-in default table)", c.labels, got, c.want)
		}
	}
}

// TestSizeWeight_ConfigOverride pins that WithSizeLabels installs the operator's
// table (#244): an org whose labels are "size: M" / "S" / "size-l" now scores on
// the fixed weights, matching is case-insensitive, and the built-in "size/m" name
// is NO LONGER recognised once a custom table is installed (the override REPLACES
// the defaults, it does not merge).
func TestSizeWeight_ConfigOverride(t *testing.T) {
	h := New(nil, testSecret, nil, WithSizeLabels(map[string]float64{
		"size: xs": 0.5,
		"s":        1,
		"size-m":   3,
		"L":        5,
		"xxl":      8,
	}))
	cases := []struct {
		name   string
		labels []prLabel
		want   float64
	}{
		{"custom exact", []prLabel{{Name: "size: xs"}}, 0.5},
		{"case-insensitive value", []prLabel{{Name: "SIZE-M"}}, 3.0},
		{"case-insensitive key", []prLabel{{Name: "l"}}, 5.0},
		{"custom xxl", []prLabel{{Name: "XXL"}}, 8.0},
		{"first matching label wins", []prLabel{{Name: "bug"}, {Name: "s"}}, 1.0},
		{"builtin name not in custom table falls through", []prLabel{{Name: "size/m"}}, 0},
		{"unknown label falls through", []prLabel{{Name: "enhancement"}}, 0},
	}
	for _, c := range cases {
		if got := sizeWeightVia(h, c.labels); got != c.want {
			t.Errorf("%s: sizeWeight(%v) = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
}

// TestWithSizeLabels_EmptyIsNoOp pins that BOTH a nil and a non-nil EMPTY override
// map leave the built-in table in place (#244). The empty case is load-bearing:
// config.Load yields a non-nil empty map for `size_labels: {}` (the shipped default),
// and the config docs promise `{}` means "use the defaults". Guarding only on nil
// would install an empty table that scores every PR by the git heuristic — the
// opposite of the documented behaviour.
func TestWithSizeLabels_EmptyIsNoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    map[string]float64
	}{
		{"nil", nil},
		{"non-nil empty", map[string]float64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(nil, testSecret, nil, WithSizeLabels(tc.m))
			if got := sizeWeightVia(h, []prLabel{{Name: "size/l"}}); got != 5.0 {
				t.Errorf("sizeWeight(size/l) = %v, want 5 (%s override must keep defaults)", got, tc.name)
			}
		})
	}
}

// TestHandlePR_ConfiguredSizeLabel_EndToEnd proves the #244 seam through the full
// handler path (not just the sizeWeight unit): a Handler built WithSizeLabels
// scores a merged PR carrying the org's custom label name and stamps
// weight_source='label' — the same provenance a built-in label produces. This is
// the behaviour cmd/tierd's config wiring must preserve.
func TestHandlePR_ConfiguredSizeLabel_EndToEnd(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger(), WithSizeLabels(map[string]float64{
		"size: l": 5, // an org convention the built-in table does NOT recognise
	}))
	body, err := json.Marshal(map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":           11,
			"merged":           true,
			"merge_commit_sha": "sha-custom-label",
			"head":             map[string]any{"ref": "feature/42-foo"},
			"user":             map[string]any{"login": "alice"},
			"labels":           []map[string]any{{"name": "Size: L"}}, // case-insensitive match
			"additions":        4,
			"deletions":        1,
			"changed_files":    1,
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if code := doPR(t, h, body, "d-custom-label", sign(testSecret, body)); code != http.StatusNoContent {
		t.Fatalf("doPR status = %d, want 204", code)
	}
	o, ok, _ := st.LatestOutcomeByIssue(context.Background(), "", "issue-42")
	if !ok {
		t.Fatal("no outcome recorded")
	}
	if o.WeightSource != store.WeightSourceLabel {
		t.Errorf("WeightSource = %q, want %q (configured label match keeps 'label' provenance)", o.WeightSource, store.WeightSourceLabel)
	}
	if o.Weight != 5.0 {
		t.Errorf("Weight = %v, want 5.0 (custom size: l)", o.Weight)
	}
}

// TestHandlePR_CustomTableShadowsBuiltinLabel_EndToEnd pins that once a custom
// table is installed, a PR carrying a BUILT-IN label name (size/m) that is NOT in
// the custom table falls through to the git-heuristic — the override REPLACES the
// defaults rather than merging, so the operator's table is the single source of
// truth for what its labels mean (#244).
func TestHandlePR_CustomTableShadowsBuiltinLabel_EndToEnd(t *testing.T) {
	st := newFakeStore()
	h := New(st, testSecret, quietLogger(), WithSizeLabels(map[string]float64{"size: l": 5}))
	body, err := json.Marshal(map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":           12,
			"merged":           true,
			"merge_commit_sha": "sha-builtin-shadowed",
			"head":             map[string]any{"ref": "feature/42-foo"},
			"user":             map[string]any{"login": "alice"},
			"labels":           []map[string]any{{"name": "size/m"}}, // built-in name, absent from custom table
			"additions":        100,
			"deletions":        20,
			"changed_files":    3,
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if code := doPR(t, h, body, "d-builtin-shadowed", sign(testSecret, body)); code != http.StatusNoContent {
		t.Fatalf("doPR status = %d, want 204", code)
	}
	o, ok, _ := st.LatestOutcomeByIssue(context.Background(), "", "issue-42")
	if !ok {
		t.Fatal("no outcome recorded")
	}
	if o.WeightSource != store.WeightSourceHeuristic {
		t.Errorf("WeightSource = %q, want %q (built-in label not in custom table must fall through)", o.WeightSource, store.WeightSourceHeuristic)
	}
}
