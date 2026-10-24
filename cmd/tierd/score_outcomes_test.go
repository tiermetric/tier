package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
)

func personalFixture(t *testing.T) (string, string, []collector.TokenEvent) {
	t.Helper()
	loadEmbeddedPriceTable(t)
	repo, logs := initGitRepo(t), t.TempDir()
	writeSessionFixture(t, logs, repo)
	c := collector.JSONLCollector{RepoPath: repo, RepoSlug: "owner/repo", ClaudeDir: logs}
	events, err := c.Collect(context.Background(), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(events) != 1 || events[0].Developer == "token-owner" {
		t.Fatalf("fixture must have one OS-username cost, distinct from GitHub: %v, %+v", err, events)
	}
	return repo, logs, events
}

func personalGitHub(t *testing.T, fail string, inspect func(*http.Request), logins ...string) *httptest.Server {
	t.Helper()
	login := "token-owner"
	if len(logins) > 0 {
		login = logins[0]
	}
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("missing token authentication")
		}
		if inspect != nil {
			inspect(r)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == fail {
			http.Error(w, "fixture refusal", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/user":
			_, _ = fmt.Fprintf(w, `{"login":%q}`, login)
		case "/repos/owner/repo/pulls":
			_, _ = fmt.Fprint(w, `[{"number":42,"user":{"login":"token-owner"},"updated_at":"2026-05-20T12:00:00Z","merged_at":"2026-05-20T12:00:00Z"},{"number":43,"user":{"login":"token-owner"},"updated_at":"2026-05-20T12:00:00Z","merged_at":"2026-05-20T12:00:00Z"},{"number":44,"user":{"login":"someone-else"},"updated_at":"2026-05-20T12:00:00Z","merged_at":"2026-05-20T12:00:00Z"}]`)
		case "/repos/owner/repo/pulls/42", "/repos/owner/repo/pulls/43":
			login, number, sha := "token-owner", 42, strings.Repeat("a", 40)
			if strings.HasSuffix(r.URL.Path, "/43") {
				login, number, sha = "someone-else", 43, strings.Repeat("b", 40)
			}
			_, _ = fmt.Fprintf(w, `{"number":%d,"user":{"login":%q,"type":"User"},"merged_at":"2026-05-20T12:00:00Z","created_at":"2026-05-18T12:00:00Z","merge_commit_sha":%q,"body":"Closes #42","head":{"ref":"feature/42-foo"},"labels":[{"name":"size/M"}],"additions":30,"deletions":2,"changed_files":1}`, number, login, sha)
		default:
			t.Errorf("unexpected GitHub request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestPersonalScoreRefusalsAndCaveat(t *testing.T) {
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	repo, logs, _ := personalFixture(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--token", "literal-secret"}, "--token must use @/path/to/file or TIER_GITHUB_TOKEN"},
		{[]string{"--token=literal-secret"}, "--token must use @/path/to/file or TIER_GITHUB_TOKEN"},
		{[]string{"--github-api-url", "http://fixture"}, "--github-api-url requires HTTPS"},
		{[]string{"--github-api-url=http://fixture"}, "--github-api-url requires HTTPS"},
		{[]string{"--db", "/never/write.db"}, "--db is not supported with --outcomes"},
		{[]string{"--server=https://example.com"}, "--server is not supported with --outcomes"},
		{[]string{"--ship"}, "--ship is not supported with --outcomes"},
		{[]string{"--api-token", "ship-secret"}, "--api-token is not supported with --outcomes"},
		{[]string{"--codex-rollout"}, "--codex-rollout is not supported with --outcomes"},
		{[]string{"--developer", "someone-else"}, "--developer is not supported with --outcomes"},
		{[]string{"--author=someone-else"}, "--author is not supported with --outcomes"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("refusal made a request: %s", r.URL)
				http.Error(w, "unexpected request", http.StatusUnauthorized)
			})
			s := httptest.NewTLSServer(h)
			t.Cleanup(s.Close)
			args := []string{"--outcomes", "--github-api-url", s.URL, "--repo", repo, "--repo-slug", "owner/repo", "--claude-dir", logs}
			if strings.Contains(tc.want, "requires HTTPS") {
				plain := httptest.NewServer(h)
				t.Cleanup(plain.Close)
				if strings.Contains(tc.args[0], "=") {
					tc.args = []string{"--github-api-url=" + plain.URL}
				} else {
					tc.args = []string{"--github-api-url", plain.URL}
				}
			}
			var out, errs bytes.Buffer
			code := runScoreWithClient(append(args, tc.args...), &out, &errs, s.Client())
			if code == 0 || errs.String() != "score: "+tc.want+"\n" {
				t.Fatalf("want exact refusal %q: code=%d stderr=%q", tc.want, code, errs.String())
			}
			if strings.Contains(errs.String(), "literal-secret") || strings.Contains(errs.String(), "ship-secret") {
				t.Fatal("refusal disclosed token")
			}
			if strings.TrimSpace(out.String()) != personalScoreCaveat {
				t.Fatalf("missing verbatim caveat: %q", out.String())
			}
		})
	}
}

// This independently calls /scores over the same fixture, with and without the
// OS/GitHub alias. Removing the production alias must split the row and fail.
func TestPersonalScoreJoinAndAPIParity(t *testing.T) {
	repo, logs, events := personalFixture(t)
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	s := personalGitHub(t, "", nil)
	var out, errs bytes.Buffer
	args := []string{"--outcomes", "--github-api-url", s.URL, "--repo", repo, "--repo-slug", "owner/repo", "--claude-dir", logs, "--since", "2026-05-01"}
	// @file and env are both accepted; never exercise the real network.
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("fixture-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args = append(args, "--token", "@"+tokenPath)
	if code := runScoreWithClient(args, &out, &errs, s.Client()); code != 0 {
		t.Fatalf("score: %d %s", code, errs.String())
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "expected.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := ingester.Store(db).Ingest(ctx, events[0]); err != nil {
		t.Fatal(err)
	}
	merged := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	if _, err := db.RecordPROutcome(ctx, store.Outcome{Developer: "token-owner", Repo: "owner/repo", IssueID: "issue-42", PRNumber: 42, Weight: 3, Quality: 1, Timestamp: merged, MergedAt: &merged, MergeCommitSHA: strings.Repeat("a", 40), Source: outcomeSourceBackfill}); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, "test", api.RateLimitConfig{})
		mux := http.NewServeMux()
		h.RegisterReadOnly(mux)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/scores?since=2026-05-01&repo=owner/repo", nil))
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
			t.Fatalf("API: %d %s %v", w.Code, w.Body, err)
		}
		return result
	}
	before := read()["developers"].([]any)
	if len(before) != 2 || before[0].(map[string]any)["tier"].(float64) != 0 || before[1].(map[string]any)["tier"].(float64) != 0 {
		t.Fatalf("fixture must demonstrate unjoined TIER=0: %+v", before)
	}
	if err := db.UpsertDeveloperAlias(ctx, events[0].Developer, "token-owner", "test"); err != nil {
		t.Fatal(err)
	}
	expected := read()
	var actualQuality map[string]any
	_, quality, _ := strings.Cut(out.String(), "data_quality: ")
	if err := json.Unmarshal([]byte(quality), &actualQuality); err != nil || !reflect.DeepEqual(actualQuality, expected["data_quality"]) {
		t.Fatalf("data_quality differs from API: %v\n%s\n%+v", err, quality, expected["data_quality"])
	}
	d := expected["developers"].([]any)[0].(map[string]any)
	for label, field := range map[string]string{"points": "weighted_points", "cost_usd": "total_cost_usd", "TIER": "tier", "ranked": "ranked", "sample_n": "sample_n"} {
		value := fmt.Sprint(d[field])
		if n, ok := d[field].(float64); ok {
			value = strconv.FormatFloat(n, 'g', -1, 64)
		}
		want := label + "=" + value
		if !strings.Contains(out.String(), want+" ") {
			t.Errorf("missing API value %s: %s", want, out.String())
		}
	}
	if d["tier"].(float64) <= 0 || !strings.Contains(out.String(), "flagged_outcomes=0") || strings.Count(out.String(), "points=") != 1 || strings.Contains(out.String(), "someone-else") {
		t.Fatalf("must join only own outcomes: %s", out.String())
	}
	if !strings.Contains(out.String(), personalScoreCaveat+"\n") || !strings.Contains(out.String(), `"window_predates_cost_capture":true`) {
		t.Fatalf("missing caveat/horizon flags: %s", out.String())
	}
}

func TestPersonalScoreTempCleanup(t *testing.T) {
	repo, logs, _ := personalFixture(t)
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	for _, fail := range []string{"", "/user", "/repos/owner/repo/pulls", "SIGINT"} {
		t.Run(fail, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			var seen atomic.Bool
			var interrupt sync.Once
			cancelled := make(chan struct{})
			// Observe a real SIGINT while the command owns a private store.
			s := personalGitHub(t, fail, func(r *http.Request) {
				dirs, _ := filepath.Glob(filepath.Join(tmp, "tierd-score-*"))
				if len(dirs) != 1 {
					t.Errorf("private store missing during request: %v", dirs)
					return
				}
				seen.Store(true)
				fi, err := os.Stat(dirs[0])
				if err != nil || fi.Mode().Perm() != 0o700 {
					t.Errorf("temp directory must be 0700: %v %v", fi, err)
				}
				if fail == "SIGINT" {
					interrupt.Do(func() {
						_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
						<-r.Context().Done()
						close(cancelled)
					})
				}
			})
			var out, errs bytes.Buffer
			code := runScoreWithClient([]string{"--outcomes", "--github-api-url", s.URL, "--repo", repo, "--repo-slug", "owner/repo", "--claude-dir", logs, "--since", "2026-05-01"}, &out, &errs, s.Client())
			if (code == 0) != (fail == "") || (fail == "/user" && !strings.Contains(errs.String(), "GET /user failed")) {
				t.Fatalf("unexpected result: %d %s", code, errs.String())
			}
			if !strings.HasPrefix(out.String(), personalScoreCaveat+"\n") || (fail != "" && strings.Count(strings.TrimSpace(errs.String()), "\n") != 0) {
				t.Fatalf("missing caveat or multiline refusal: %q %q", out.String(), errs.String())
			}
			if fail == "SIGINT" {
				select {
				case <-cancelled:
				case <-time.After(5 * time.Second):
					t.Fatal("SIGINT did not cancel the request context")
				}
			}
			remaining, _ := filepath.Glob(filepath.Join(tmp, "tierd-score-*"))
			if !seen.Load() || len(remaining) != 0 {
				t.Fatalf("temp lifecycle: seen=%t remaining=%v", seen.Load(), remaining)
			}
		})
	}
}

func TestPersonalScoreRedirectGuard(t *testing.T) {
	repo, logs, _ := personalFixture(t)
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			var requests, authorized atomic.Int32
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "" {
					authorized.Add(1)
				}
				http.Error(w, "redirect target", http.StatusUnauthorized)
			})
			var target *httptest.Server
			if scheme == "http" {
				target = httptest.NewServer(h)
			} else {
				target = httptest.NewTLSServer(h)
			}
			t.Cleanup(target.Close)
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Errorf("unexpected source request: %s", r.URL)
				}
				location := target.URL
				if scheme == "http" {
					location = "http://" + r.Host + "/target"
				}
				http.Redirect(w, r, location, http.StatusFound)
			}))
			t.Cleanup(s.Close)
			transport := s.Client().Transport.(*http.Transport).Clone()
			if scheme == "http" {
				// Keep the redirect's logical host identical, while routing its
				// plaintext connection to an HTTP fixture that can observe headers.
				transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, target.Listener.Addr().String())
				}
				transport.DialTLSContext = (&tls.Dialer{Config: transport.TLSClientConfig}).DialContext
			}
			t.Cleanup(transport.CloseIdleConnections)
			var out, errs bytes.Buffer
			code := runScoreWithClient([]string{"--outcomes", "--github-api-url", s.URL, "--repo", repo, "--repo-slug", "owner/repo", "--claude-dir", logs}, &out, &errs, &http.Client{Transport: transport})
			if code == 0 || errs.String() != "score: GET /user failed; cannot bind outcomes to the token owner\n" {
				t.Errorf("want nonzero, one-line redirect refusal: code=%d stderr=%q", code, errs.String())
			}
			if requests.Load() != 0 || authorized.Load() != 0 {
				t.Fatalf("redirect target contacted: requests=%d Authorization=%d", requests.Load(), authorized.Load())
			}
		})
	}
}

func TestPersonalScoreInvalidOwner(t *testing.T) {
	repo, logs, _ := personalFixture(t)
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	for _, body := range []string{`{}`, `{"login":"unattributed"}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/user" {
					t.Errorf("invalid owner reached PR endpoint: %s", r.URL)
					http.Error(w, "unexpected PR request", http.StatusUnauthorized)
					return
				}
				_, _ = fmt.Fprint(w, body)
			}))
			t.Cleanup(s.Close)
			var out, errs bytes.Buffer
			code := runScoreWithClient([]string{"--outcomes", "--github-api-url", s.URL, "--repo", repo, "--repo-slug", "owner/repo", "--claude-dir", logs}, &out, &errs, s.Client())
			if code == 0 || errs.String() != "score: GET /user returned no valid GitHub login\n" {
				t.Fatalf("invalid owner: code=%d stderr=%q", code, errs.String())
			}
		})
	}
}

func TestPersonalScoreCaseFoldOwner(t *testing.T) {
	repo, logs, _ := personalFixture(t)
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	s := personalGitHub(t, "", nil, "Token-Owner")
	var out, errs bytes.Buffer
	code := runScoreWithClient([]string{"--outcomes", "--github-api-url", s.URL, "--repo", repo, "--repo-slug", "owner/repo", "--claude-dir", logs, "--since", "2026-05-01"}, &out, &errs, s.Client())
	if code != 0 || strings.Count(out.String(), "points=") != 1 || !strings.Contains(out.String(), "Token-Owner  points=3 ") || !strings.Contains(out.String(), "sample_n=1 ") {
		t.Fatalf("case-folded owner must have one joined row: code=%d\n%s\n%s", code, out.String(), errs.String())
	}
}

func TestScorePlainArguments(t *testing.T) {
	repo, logs, _ := personalFixture(t)
	baseArgs := []string{"--repo", repo, "--repo-slug", "owner/repo", "--claude-dir", logs, "--since", "2026-05-01"}
	var baseline, baselineErr bytes.Buffer
	if code := runScoreCmd(baseArgs, &baseline, &baselineErr); code != 0 {
		t.Fatalf("baseline score: %d %s", code, baselineErr.String())
	}
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--unknown-score-flag"}, 2},
		{[]string{"positional", "--unknown-score-flag"}, 0},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errs bytes.Buffer
			args := append(append([]string{}, baseArgs...), tc.args...)
			if code := runScoreCmd(args, &out, &errs); code != tc.want {
				t.Fatalf("plain score: got %d want %d; stderr=%q", code, tc.want, errs.String())
			}
			if tc.want == 0 && out.String() != baseline.String() {
				t.Fatalf("positional arguments must preserve main's cost report: %s", out.String())
			}
		})
	}
}

func TestPersonalScoreSuccessNotices(t *testing.T) {
	repo, logs, _ := personalFixture(t)
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	cfg := filepath.Join(t.TempDir(), "tier.yaml")
	if err := os.WriteFile(cfg, []byte("subscriptions:\n  - route_prefix: no-such-route\n    monthly_fee_usd: 20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := personalGitHub(t, "", nil)
	var out, errs bytes.Buffer
	code := runScoreWithClient([]string{"--outcomes", "--github-api-url", s.URL, "--repo", repo, "--repo-slug", "owner/repo", "--claude-dir", logs, "--since", "2026-05-01", "--config", cfg}, &out, &errs, s.Client())
	if code != 0 {
		t.Fatalf("score: %d %s", code, errs.String())
	}
	for _, want := range []string{"price table: ", "price table identity: table_hash=", "WARNING: config subscriptions route_prefix", "merged=2 inserted=1 unattributed=0 excluded=0\n"} {
		if strings.Count(errs.String(), want) != 1 {
			t.Errorf("want one stderr notice %q: %s", want, errs.String())
		}
	}
}

func TestPersonalScoreNoMatchingOwner(t *testing.T) {
	for _, developers := range []string{`[]`, `[{"developer":"someone-else","weighted_points":3}]`} {
		t.Run(developers, func(t *testing.T) {
			w := httptest.NewRecorder()
			_, _ = fmt.Fprintf(w, `{"developers":%s,"data_quality":{}}`, developers)
			var out bytes.Buffer
			if err := printPersonalScores(w, "token-owner", &out); err != nil {
				t.Fatal(err)
			}
			if out.String() != "No local costs or own attributed merged PRs in this window.\ndata_quality: {}\n" {
				t.Fatalf("missing owner must be explained: %s", out.String())
			}
		})
	}
}

func TestPersonalScoreNoGitHubOrigin(t *testing.T) {
	repo, logs, _ := personalFixture(t)
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("missing origin reached GitHub: %s", r.URL)
		http.Error(w, "unexpected request", http.StatusUnauthorized)
	}))
	t.Cleanup(s.Close)
	var out, errs bytes.Buffer
	code := runScoreWithClient([]string{"--outcomes", "--github-api-url", s.URL, "--repo", repo, "--claude-dir", logs}, &out, &errs, s.Client())
	if code == 0 || errs.String() != "score: --outcomes needs a GitHub origin or --repo-slug owner/name\n" {
		t.Fatalf("missing origin: code=%d stderr=%q", code, errs.String())
	}
}

func TestPersonalScoreScoresNon200(t *testing.T) {
	loadEmbeddedPriceTable(t)
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	s := personalGitHub(t, "", nil)
	gh, err := personalGitHubClient("", s.URL, s.Client())
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	// A date beyond /scores' supported date syntax makes the real in-process
	// handler return 400, without replacing the handler or breaking its store.
	err = runPersonalScore(context.Background(), gh, nil, "owner/repo", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), nil, &out, &errs)
	if err == nil || err.Error() != "in-process /api/v1/scores returned 400" {
		t.Fatalf("want in-process status refusal, got %v; stdout=%q", err, out.String())
	}
	if out.Len() != 0 || errs.Len() != 0 {
		t.Fatalf("failed /scores must not print results: %q %q", out.String(), errs.String())
	}
}

func TestScoreTokenIgnoredWarning(t *testing.T) {
	repo, logs, _ := personalFixture(t)
	var out, errs bytes.Buffer
	code := runScoreCmd([]string{"--repo", repo, "--claude-dir", logs, "--since", "2026-05-01", "--token", "never-echo-this-secret"}, &out, &errs)
	if code != 0 || strings.Count(errs.String(), "WARNING: --token is ignored without --outcomes\n") != 1 {
		t.Fatalf("missing ignored-token warning: code=%d stderr=%q", code, errs.String())
	}
	if strings.Contains(out.String()+errs.String(), "never-echo-this-secret") {
		t.Fatal("ignored token disclosed")
	}
}

func TestPersonalScoreRateLimitNotice(t *testing.T) {
	repo, logs, _ := personalFixture(t)
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	var users atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			if users.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = fmt.Fprint(w, `{"login":"token-owner"}`)
		case "/repos/owner/repo/pulls":
			_, _ = fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	var out, errs bytes.Buffer
	code := runScoreWithClient([]string{"--outcomes", "--github-api-url", s.URL, "--repo", repo, "--repo-slug", "owner/repo", "--claude-dir", logs, "--since", "2026-05-01"}, &out, &errs, s.Client())
	if code != 0 || strings.Count(errs.String(), "GitHub rate limit hit, waiting") != 1 || strings.Contains(out.String(), "rate limit") {
		t.Fatalf("rate-limit notice must reach stderr: code=%d stdout=%q stderr=%q", code, out.String(), errs.String())
	}
}


// TestScorePreflightStopsWhereFlagParseStops pins that arguments flag.Parse
// never reads (after a positional, "-" or "--") cannot turn on --outcomes or
// trigger its refusals, so plain `tierd score` keeps main's behaviour.
func TestScorePreflightStopsWhereFlagParseStops(t *testing.T) {
	repo, logs, _ := personalFixture(t)
	baseArgs := []string{"--repo", repo, "--repo-slug", "owner/repo", "--claude-dir", logs, "--since", "2026-05-01"}
	for _, extra := range [][]string{
		{"--developer", "alice", "outcomes"},
		{"positional", "--outcomes"},
		{"--", "--outcomes"},
		{"-", "--outcomes"},
	} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			var out, errs bytes.Buffer
			args := append(append([]string{}, baseArgs...), extra...)
			if code := runScoreCmd(args, &out, &errs); code != 0 {
				t.Fatalf("plain score: got %d want 0; stderr=%q", code, errs.String())
			}
			if strings.Contains(out.String(), personalScoreCaveat) || strings.Contains(errs.String(), "not supported with --outcomes") {
				t.Fatalf("an argument flag.Parse never reads enabled personal mode: stdout=%q stderr=%q", out.String(), errs.String())
			}
		})
	}
}
