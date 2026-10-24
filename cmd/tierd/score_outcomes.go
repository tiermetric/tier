package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

const personalScoreCaveat = "TIER is built for coaching, not for evaluating developers. Per-person figures are not accurate enough to judge an individual."

// Preflight before flag.Parse so forbidden flags get one line, never usage or a
// literal token echoed back. Skip values of known flags when finding --outcomes.
// It stops exactly where flag.Parse stops (the first positional argument, "-"
// or "--"), so arguments flag.Parse would not read never enable or refuse.
func personalScoreFlags(fs *flag.FlagSet, args []string) (bool, error) {
	on, forbidden := false, ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--" || args[i] == "-" || !strings.HasPrefix(args[i], "-") {
			break
		}
		name, value, equals := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if name == "outcomes" {
			on = true
			if equals {
				on, _ = strconv.ParseBool(value)
			}
		}
		f := fs.Lookup(name)
		if name != "h" && name != "help" && (f == nil || name == "developer") && forbidden == "" {
			forbidden = name
		}
		if f != nil && !equals {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
				i++
			}
		}
	}
	if on && forbidden != "" {
		return true, fmt.Errorf("--%s is not supported with --outcomes", forbidden)
	}
	return on, nil
}

func personalGitHubClient(token, apiURL string, client *http.Client) (*githubClient, error) {
	if token != "" && !strings.HasPrefix(token, "@") {
		return nil, errors.New("--token must use @/path/to/file or TIER_GITHUB_TOKEN")
	}
	// The backfill validator also checks URL shape. This command's ruling is
	// stricter: plaintext is refused even on loopback (tests use httptest TLS).
	base, err := validateAPIBaseURL(apiURL)
	if err != nil {
		return nil, errors.New("--github-api-url requires HTTPS")
	}
	if !strings.HasPrefix(base, "https://") {
		return nil, errors.New("--github-api-url requires HTTPS")
	}
	// Exactly backfill's env fallback and shared @file resolver; flag defaults
	// remain empty so help cannot disclose a token.
	if token == "" {
		token = os.Getenv("TIER_GITHUB_TOKEN")
	}
	tok, err := resolveSecretFlag("--token", token)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, errors.New("--token is required (set TIER_GITHUB_TOKEN or pass @/path/to/token)")
	}
	c := http.Client{Timeout: 30 * time.Second}
	if client != nil {
		c.Transport = client.Transport
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
			return errRedirectRefused
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &githubClient{http: &c, baseURL: base, token: tok, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, nil
}

func runPersonalScore(ctx context.Context, gh *githubClient, events []collector.TokenEvent, repo string, since time.Time, sizes map[string]float64, out, stderr io.Writer) (resultErr error) {
	repo, ok := repoid.Canonical(repo)
	if !ok {
		return errors.New("--outcomes needs a GitHub origin or --repo-slug owner/name")
	}
	// Reuse demo's private temporary directory lifecycle, never its synthetic
	// exemption. The caller registered signal cancellation before entering here;
	// all failures and SIGINT unwind Close before RemoveAll. No listener opens.
	dir, err := os.MkdirTemp("", "tierd-score-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove private database directory: %w", err))
		}
	}()
	db, err := store.Open(filepath.Join(dir, "score.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	body, err := gh.do(ctx, "/user")
	if err != nil {
		return errors.New("GET /user failed; cannot bind outcomes to the token owner")
	}
	var user struct{ Login string }
	if err := json.Unmarshal(body, &user); err != nil || user.Login == "" || store.ResemblesUnattributed(user.Login) {
		return errors.New("GET /user returned no valid GitHub login")
	}
	gh.ownerLogin = user.Login
	sink := ingester.Store(db)
	aliased := map[string]bool{}
	for _, ev := range events {
		// The collector uses only the invoking OS identity (overrides refused).
		if ev.Developer != user.Login && !aliased[ev.Developer] {
			if err := db.UpsertDeveloperAlias(ctx, ev.Developer, user.Login, "local-personal-score"); err != nil {
				return err
			}
			aliased[ev.Developer] = true
		}
		if err := sink.Ingest(ctx, ev); err != nil {
			return err
		}
	}
	stats, err := backfillOutcomes(ctx, gh, db, repo, since, sizes)
	if err != nil {
		return err
	}
	// Invoke the real /scores handler in-process. It reaches loadWindow and all
	// its quality disclosures; this command contains no scoring calculation.
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{})
	mux := http.NewServeMux()
	h.RegisterReadOnly(mux)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/scores?since="+since.Format("2006-01-02")+"&repo="+repo, nil).WithContext(ctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if err := printPersonalScores(w, user.Login, out); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "merged=%d inserted=%d unattributed=%d excluded=%d\n", stats.merged, stats.inserted, stats.unattributed, stats.excluded)
	return nil
}

func printPersonalScores(w *httptest.ResponseRecorder, login string, out io.Writer) error {
	if w.Code != http.StatusOK {
		return fmt.Errorf("in-process /api/v1/scores returned %d", w.Code)
	}
	var result struct {
		Developers []struct {
			Developer       string  `json:"developer"`
			Points          float64 `json:"weighted_points"`
			Cost            float64 `json:"total_cost_usd"`
			TIER            float64 `json:"tier"`
			Ranked          bool    `json:"ranked"`
			SampleN         int     `json:"sample_n"`
			FlaggedOutcomes int     `json:"flagged_outcomes"`
		} `json:"developers"`
		DataQuality json.RawMessage `json:"data_quality"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		return err
	}
	found := false
	for _, d := range result.Developers {
		if d.Developer == login {
			found = true
			_, _ = fmt.Fprintf(out, "%s  points=%g  cost_usd=%g  TIER=%g  ranked=%t  sample_n=%d  flagged_outcomes=%d\n", d.Developer, d.Points, d.Cost, d.TIER, d.Ranked, d.SampleN, d.FlaggedOutcomes)
		}
	}
	if !found {
		_, _ = fmt.Fprintln(out, "No local costs or own attributed merged PRs in this window.")
	}
	_, _ = fmt.Fprintf(out, "data_quality: %s\n", result.DataQuality)
	return nil
}
