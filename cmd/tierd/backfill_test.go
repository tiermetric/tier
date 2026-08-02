package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// fakePR is the subset of the GitHub pull-request shape the backfill reads. The
// list endpoint omits the diff stats (additions/deletions/changed_files) — GitHub
// serves those only on the single-PR detail endpoint — so the fake mirrors that
// split: mergedAt/updatedAt drive list-side windowing, the rest lands on detail.
type fakePR struct {
	number    int
	updatedAt string // RFC3339; "" ⇒ omitted
	mergedAt  string // RFC3339; "" ⇒ null (closed-unmerged)
	mergeSHA  string
	branch    string
	body      string
	login     string
	labels    []string
	adds      int
	dels      int
	changed   int
}

// newFakeGitHub serves the two endpoints backfill calls: the closed-PR list
// (state=closed, sorted updated desc) and the per-PR detail. Everything except
// updatedAt/mergedAt is served only on detail, matching real GitHub.
func newFakeGitHub(t *testing.T, prs []fakePR) *httptest.Server {
	t.Helper()
	byNum := map[int]fakePR{}
	for _, p := range prs {
		byNum[p.number] = p
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Detail: /repos/owner/repo/pulls/<n>
		if strings.HasPrefix(r.URL.Path, "/repos/owner/repo/pulls/") {
			numStr := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/pulls/")
			var n int
			_, _ = fmt.Sscanf(numStr, "%d", &n)
			p, ok := byNum[n]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var labels []map[string]string
			for _, l := range p.labels {
				labels = append(labels, map[string]string{"name": l})
			}
			var mergedAt any
			if p.mergedAt != "" {
				mergedAt = p.mergedAt
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":           p.number,
				"body":             p.body,
				"merged_at":        mergedAt,
				"merge_commit_sha": p.mergeSHA,
				"head":             map[string]any{"ref": p.branch},
				"user":             map[string]any{"login": p.login},
				"labels":           labels,
				"additions":        p.adds,
				"deletions":        p.dels,
				"changed_files":    p.changed,
			})
			return
		}
		// List: /repos/owner/repo/pulls?state=closed&page=N
		if r.URL.Path == "/repos/owner/repo/pulls" {
			if r.URL.Query().Get("page") != "1" {
				_, _ = w.Write([]byte("[]"))
				return
			}
			var list []map[string]any
			for _, p := range prs {
				var mergedAt any
				if p.mergedAt != "" {
					mergedAt = p.mergedAt
				}
				list = append(list, map[string]any{
					"number":     p.number,
					"updated_at": p.updatedAt,
					"merged_at":  mergedAt,
				})
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// standardPRs is the shared fixture: one label-weighted merge, one heuristic
// merge, one closed-unmerged PR, and one merge that predates --since (which also
// stops list pagination). Ordered updated-desc as GitHub would return them.
func standardPRs() []fakePR {
	return []fakePR{
		{number: 101, updatedAt: "2026-03-01T10:00:00Z", mergedAt: "2026-03-01T10:00:00Z",
			mergeSHA: "sha101", branch: "feature/55-x", body: "Closes #55", login: "alice",
			labels: []string{"size/m"}, adds: 10, dels: 2, changed: 3},
		{number: 102, updatedAt: "2026-02-20T10:00:00Z", mergedAt: "",
			mergeSHA: "", branch: "feature/60-y", body: "Closes #60", login: "carol"},
		{number: 103, updatedAt: "2026-02-10T10:00:00Z", mergedAt: "2026-02-10T10:00:00Z",
			mergeSHA: "sha103", branch: "bugfix/x", body: "fixes #77", login: "bob",
			labels: nil, adds: 40, dels: 10, changed: 3},
		{number: 104, updatedAt: "2025-12-01T10:00:00Z", mergedAt: "2025-12-01T10:00:00Z",
			mergeSHA: "sha104", branch: "feature/88-old", body: "Closes #88", login: "dave",
			labels: []string{"size/l"}, adds: 5, dels: 1, changed: 1},
	}
}

// TestDispatch_BackfillReconstructsOutcomes drives the whole subcommand through
// dispatch against a fake GitHub and a real SQLite store, then reads the stored
// rows back: merged PRs in-window become outcomes with source='backfill' and the
// SAME weight/weight_source/issue derivation the webhook produces; unmerged and
// out-of-window PRs produce nothing.
func TestDispatch_BackfillReconstructsOutcomes(t *testing.T) {
	srv := newFakeGitHub(t, standardPRs())
	defer srv.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tier.db")

	var out, errOut bytes.Buffer
	code := dispatch([]string{
		"backfill",
		"--repo", "owner/repo",
		"--since", "2026-01-01",
		"--token", "x",
		"--db", dbPath,
		"--github-api-url", srv.URL,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "inserted=2") {
		t.Errorf("stdout = %q, want inserted=2", out.String())
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	o101, ok, err := db.OutcomeByMergeCommit(ctx, "sha101")
	if err != nil || !ok {
		t.Fatalf("sha101 lookup: ok=%v err=%v", ok, err)
	}
	if o101.Source != "backfill" {
		t.Errorf("sha101 Source = %q, want backfill", o101.Source)
	}
	if o101.WeightSource != store.WeightSourceLabel || o101.Weight != 3.0 {
		t.Errorf("sha101 weight = %v/%q, want 3.0/label", o101.Weight, o101.WeightSource)
	}
	if o101.IssueID != "issue-55" || o101.Repo != "owner/repo" || o101.Developer != "alice" {
		t.Errorf("sha101 = issue %q repo %q dev %q, want issue-55/owner/repo/alice",
			o101.IssueID, o101.Repo, o101.Developer)
	}

	o103, ok, err := db.OutcomeByMergeCommit(ctx, "sha103")
	if err != nil || !ok {
		t.Fatalf("sha103 lookup: ok=%v err=%v", ok, err)
	}
	if o103.WeightSource != store.WeightSourceHeuristic || o103.Weight != store.GitHeuristic(50, 3) {
		t.Errorf("sha103 weight = %v/%q, want %v/git-heuristic",
			o103.Weight, o103.WeightSource, store.GitHeuristic(50, 3))
	}
	if o103.IssueID != "issue-77" {
		t.Errorf("sha103 IssueID = %q, want issue-77", o103.IssueID)
	}

	// Closed-unmerged (102/#60) and out-of-window (104/#88) must NOT be recorded.
	if _, ok, _ := db.OutcomeByMergeCommit(ctx, "sha104"); ok {
		t.Errorf("sha104 (pre-since) was recorded; want skipped")
	}
}

// TestDispatch_BackfillIdempotent pins the MANDATORY re-run safety: a second
// backfill over the same history inserts nothing (ON CONFLICT DO NOTHING on
// merge_commit_sha), so overlapping with the live webhook never double-counts.
func TestDispatch_BackfillIdempotent(t *testing.T) {
	srv := newFakeGitHub(t, standardPRs())
	defer srv.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tier.db")
	args := []string{
		"backfill", "--repo", "owner/repo", "--since", "2026-01-01",
		"--token", "x", "--db", dbPath, "--github-api-url", srv.URL,
	}

	var out1, err1 bytes.Buffer
	if code := dispatch(args, &out1, &err1); code != 0 {
		t.Fatalf("first run exit = %d (stderr=%q)", code, err1.String())
	}
	if !strings.Contains(out1.String(), "inserted=2") {
		t.Fatalf("first run stdout = %q, want inserted=2", out1.String())
	}

	var out2, err2 bytes.Buffer
	if code := dispatch(args, &out2, &err2); code != 0 {
		t.Fatalf("second run exit = %d (stderr=%q)", code, err2.String())
	}
	if !strings.Contains(out2.String(), "inserted=0") {
		t.Errorf("second run stdout = %q, want inserted=0 (idempotent)", out2.String())
	}
	if !strings.Contains(out2.String(), "skipped=2") {
		t.Errorf("second run stdout = %q, want skipped=2", out2.String())
	}
}

// respWith builds a bare *http.Response carrying the given status and headers, so
// the rate-limit helpers can be exercised without a live server.
func respWith(status int, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h}
}

// TestBackfillIsRateLimited pins which responses the retry loop treats as a rate
// limit: a 429 always, a 403 only with Retry-After or an exhausted remaining count,
// and a 403 with budget remaining is a permanent error (bad token/repo), not a wait.
func TestBackfillIsRateLimited(t *testing.T) {
	cases := []struct {
		name string
		resp *http.Response
		want bool
	}{
		{"429 always", respWith(429, nil), true},
		{"403 remaining 0", respWith(403, map[string]string{"X-RateLimit-Remaining": "0"}), true},
		{"403 retry-after", respWith(403, map[string]string{"Retry-After": "30"}), true},
		{"403 budget left is permanent", respWith(403, map[string]string{"X-RateLimit-Remaining": "17"}), false},
		{"200 not limited", respWith(200, nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRateLimited(tc.resp); got != tc.want {
				t.Errorf("isRateLimited = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBackfillRateLimitWait pins the backoff duration: Retry-After wins, else
// X-RateLimit-Reset measured against a fixed now, all clamped to [1s, 5m].
func TestBackfillRateLimitWait(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name    string
		headers map[string]string
		want    time.Duration
	}{
		{"retry-after seconds", map[string]string{"Retry-After": "30"}, 30 * time.Second},
		{"reset in future +1s cushion", map[string]string{"X-RateLimit-Reset": strconv.FormatInt(now.Unix()+120, 10)}, 121 * time.Second},
		{"far-future reset clamped to max", map[string]string{"X-RateLimit-Reset": strconv.FormatInt(now.Unix()+86400, 10)}, ghMaxRateLimitWait},
		{"past reset retries promptly", map[string]string{"X-RateLimit-Reset": strconv.FormatInt(now.Unix()-999, 10)}, time.Second},
		{"no headers uses secondary floor", nil, ghSecondaryRateLimitFloor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rateLimitWait(respWith(429, tc.headers), now); got != tc.want {
				t.Errorf("rateLimitWait = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValidateAPIBaseURL pins the plaintext-token guard (#237 review Y2): https is
// always allowed, http only to a loopback host, and everything else is rejected.
func TestValidateAPIBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"https public", "https://api.github.com", false},
		{"https trailing slash trimmed", "https://ghe.example.com/api/v3/", false},
		{"http loopback ok", "http://127.0.0.1:8080", false},
		{"http localhost ok", "http://localhost:9000", false},
		{"http public rejected", "http://api.github.com", true},
		{"ftp rejected", "ftp://api.github.com", true},
		{"no host rejected", "https://", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateAPIBaseURL(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateAPIBaseURL(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if err == nil && strings.HasSuffix(got, "/") {
				t.Errorf("validateAPIBaseURL(%q) = %q, trailing slash not trimmed", tc.in, got)
			}
		})
	}
}

// TestDispatch_BackfillHonorsSizeLabelsConfig proves the #244/#301 fix: a backfill
// run with a config whose outcomes.size_labels remaps size labels scores the merged
// PR on the operator's CUSTOM weight, exactly as the live webhook would — closing the
// divergence where backfill used the built-in default table only. The fixture PR
// carries a custom label name the built-in table does NOT recognise, so a passing
// custom weight (with weight_source='label') can only come from the threaded config.
func TestDispatch_BackfillHonorsSizeLabelsConfig(t *testing.T) {
	prs := []fakePR{
		{number: 201, updatedAt: "2026-03-01T10:00:00Z", mergedAt: "2026-03-01T10:00:00Z",
			mergeSHA: "sha201", branch: "feature/70-z", body: "Closes #70", login: "erin",
			labels: []string{"Size: L"}, adds: 4, dels: 1, changed: 1},
	}
	srv := newFakeGitHub(t, prs)
	defer srv.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tier.db")
	// An org convention the built-in table does NOT know: "size: l" -> 5. Matching is
	// case-insensitive, so the PR's "Size: L" label resolves to 5.0.
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("outcomes:\n  size_labels:\n    \"size: l\": 5\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errOut bytes.Buffer
	code := dispatch([]string{
		"backfill",
		"--repo", "owner/repo",
		"--since", "2026-01-01",
		"--token", "x",
		"--db", dbPath,
		"--github-api-url", srv.URL,
		"--config", cfgPath,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errOut.String())
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = db.Close() }()

	o, ok, err := db.OutcomeByMergeCommit(context.Background(), "sha201")
	if err != nil || !ok {
		t.Fatalf("sha201 lookup: ok=%v err=%v", ok, err)
	}
	// Without the config threading this PR's "Size: L" is unknown to the built-in
	// table and would fall through to the git heuristic (source='git-heuristic').
	if o.WeightSource != store.WeightSourceLabel || o.Weight != 5.0 {
		t.Errorf("sha201 weight = %v/%q, want 5.0/label (custom size_labels must reach backfill)",
			o.Weight, o.WeightSource)
	}
}

// TestDispatch_BackfillRejectsInvalidConfig pins the fail-loud --config load path
// (#301): a config whose outcomes.size_labels carries an off-scale weight (config
// validation rejects anything outside 0.5/1/3/5/8) makes backfill exit non-zero with
// a config error BEFORE it opens the DB or calls GitHub — a malformed table must
// never be silently ignored, which would resurrect the very divergence #301 closes.
func TestDispatch_BackfillRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bad.yaml")
	// 2 is not on the fixed 0.5/1/3/5/8 outcome scale, so config.Load rejects it.
	if err := os.WriteFile(cfgPath, []byte("outcomes:\n  size_labels:\n    \"size/m\": 2\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errOut bytes.Buffer
	code := dispatch([]string{
		"backfill",
		"--repo", "owner/repo",
		"--token", "x",
		"--db", filepath.Join(dir, "tier.db"),
		"--config", cfgPath,
	}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (invalid config must fail loud)", code)
	}
	if !strings.Contains(errOut.String(), "config") {
		t.Errorf("stderr = %q, want a config error", errOut.String())
	}
}
