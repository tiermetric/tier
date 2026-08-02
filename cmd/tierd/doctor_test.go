package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// statusOf returns the status of the named check, or -1 if absent.
func statusOf(results []checkResult, name string) checkStatus {
	for _, r := range results {
		if r.name == name {
			return r.status
		}
	}
	return -1
}

// findCheck returns the named check, or fails the test if it is absent.
func findCheck(t *testing.T, results []checkResult, name string) checkResult {
	t.Helper()
	for _, r := range results {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("check %q absent from results", name)
	return checkResult{}
}

func TestEvalCapture(t *testing.T) {
	// mk builds feature-branch spend: n dollars attributed to an issue + m dollars on
	// a branch with no parseable issue number (UnattributedNoIssue). The named-branch
	// attribution rate is then exactly n/(n+m) — the input each band assertion turns
	// on. Exploratory main/detached spend is a SEPARATE signal (tested below), so it
	// is deliberately absent here.
	const dollar = int64(1_000_000)
	mk := func(n, m int) []collector.TokenEvent {
		ev := make([]collector.TokenEvent, 0, n+m)
		for i := 0; i < n; i++ {
			ev = append(ev, collector.TokenEvent{IssueID: "236", CostMicro: dollar})
		}
		for i := 0; i < m; i++ {
			ev = append(ev, collector.TokenEvent{IssueID: collector.UnattributedNoIssue, CostMicro: dollar})
		}
		return ev
	}

	tests := []struct {
		name       string
		events     []collector.TokenEvent
		collectErr error
		floor      float64
		wantRecent checkStatus
		wantAttr   checkStatus // -1 = check absent
	}{
		{"collect error", nil, errFake, defaultMinAttribution, statusFail, -1},
		{"no sessions", nil, nil, defaultMinAttribution, statusWarn, -1},
		// >= the healthy bar (90%): OK.
		{"healthy 100pct", mk(5, 0), nil, defaultMinAttribution, statusOK, statusOK},
		{"healthy at 90pct boundary", mk(9, 1), nil, defaultMinAttribution, statusOK, statusOK},
		// Between the floor and the healthy bar: WARN, still exit 0.
		{"warn 60pct", mk(3, 2), nil, defaultMinAttribution, statusOK, statusWarn},
		// Exactly at the floor is a strict <: not a FAIL, so WARN here.
		{"floor boundary 50pct is WARN", mk(1, 1), nil, defaultMinAttribution, statusOK, statusWarn},
		// Below the floor: FAIL.
		{"below floor 33pct FAILs", mk(1, 2), nil, defaultMinAttribution, statusOK, statusFail},
		// The floor is configurable: raising it turns a former WARN into a FAIL.
		{"raised floor 75pct FAILs 60pct", mk(3, 2), nil, 0.75, statusOK, statusFail},
		// Lowering it lets a sparse repo pass as a WARN instead of a FAIL.
		{"lowered floor 20pct WARNs 33pct", mk(1, 2), nil, 0.2, statusOK, statusWarn},
		// Floor ABOVE the 90% healthy bar closes the WARN band: only FAIL (< floor) or
		// OK (>= floor), never WARN.
		{"floor above healthy: 95pct OK", mk(19, 1), nil, 0.95, statusOK, statusOK},
		{"floor above healthy: 85pct FAILs", mk(17, 3), nil, 0.95, statusOK, statusFail},
		// #488 core: spend with NO feature-branch work — all exploratory on main — is
		// never a FAIL; there is nothing discipline could have attributed.
		{"exploratory-only main is OK", []collector.TokenEvent{{IssueID: collector.UnattributedMain, CostMicro: dollar}}, nil, defaultMinAttribution, statusOK, statusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalCapture(tt.events, tt.collectErr, doctorSinceDays, tt.floor)
			if s := statusOf(got, "recent sessions"); s != tt.wantRecent {
				t.Errorf("recent sessions status = %v, want %v", s, tt.wantRecent)
			}
			if s := statusOf(got, "issue attribution"); s != tt.wantAttr {
				t.Errorf("issue attribution status = %v, want %v", s, tt.wantAttr)
			}
		})
	}
}

// TestEvalCapture_ExploratoryIsNotAFailure pins the #488 core: spend on main and a
// detached HEAD is EXPLORATORY — reported as a separate green "exploratory spend" row,
// never a FAIL — while a labeled unattributed bucket is still never miscounted as an
// attributed issue (#refocus family check). Every feature-branch dollar here ($0.10 on
// issue-1) is attributed, so the named-branch rate is 100% and "issue attribution" is
// OK; the $0.30 main + $0.05 detached surface as information.
func TestEvalCapture_ExploratoryIsNotAFailure(t *testing.T) {
	events := []collector.TokenEvent{
		{IssueID: "issue-1", CostMicro: 100_000},
		{IssueID: collector.UnattributedMain, CostMicro: 300_000},
		{IssueID: collector.UnattributedDetachedHEAD, CostMicro: 50_000},
	}
	got := evalCapture(events, nil, doctorSinceDays, defaultMinAttribution)

	attr := findCheck(t, got, "issue attribution")
	if attr.status != statusOK {
		t.Errorf("issue attribution = %v, want OK: all feature-branch spend is attributed; main/detached are exploratory, not a failure (#488)", attr.status)
	}

	info := findCheck(t, got, "exploratory spend")
	if info.status != statusOK {
		t.Errorf("exploratory spend must be informational (OK), got %v", info.status)
	}
	for _, want := range []string{"$0.30 on main", "$0.05 on a detached HEAD"} {
		if !strings.Contains(info.detail, want) {
			t.Errorf("exploratory detail missing %q: %q", want, info.detail)
		}
	}
	// #488 wording contract: no sentence mixes an event rate with dollars, and none
	// reads as an accusation of waste.
	for _, r := range got {
		if strings.Contains(r.detail, "% of your AI events") {
			t.Errorf("detail mixes an event-rate with dollars (#488): %q", r.detail)
		}
		low := strings.ToLower(r.detail)
		if strings.Contains(low, "overhead") || strings.Contains(low, "accomplished nothing") {
			t.Errorf("detail reads as an accusation of waste (#488): %q", r.detail)
		}
	}
}

// TestEvalCapture_FailMessageIsActionable pins the FAIL-path wording (#488): when the
// named-branch rate is below the floor, the message is all dollars, names the amount
// on unnamed branches, gives the branch-naming remedy, and does NOT fold exploratory
// main spend into the gap.
func TestEvalCapture_FailMessageIsActionable(t *testing.T) {
	events := []collector.TokenEvent{
		{IssueID: "236", CostMicro: 100_000},                         // $0.10 attributed
		{IssueID: collector.UnattributedNoIssue, CostMicro: 300_000}, // $0.30 unnamed branch
		{IssueID: collector.UnattributedMain, CostMicro: 800_000},    // $0.80 exploratory (excluded)
	}
	// named-branch rate = 0.10 / 0.40 = 25% < 50% floor -> FAIL.
	got := evalCapture(events, nil, doctorSinceDays, defaultMinAttribution)
	attr := findCheck(t, got, "issue attribution")
	if attr.status != statusFail {
		t.Fatalf("status = %v, want FAIL (25%% named-branch rate)", attr.status)
	}
	if !strings.Contains(attr.detail, "$0.30") {
		t.Errorf("FAIL detail must name the $ on unnamed branches: %q", attr.detail)
	}
	if !strings.Contains(attr.detail, "feature branches") {
		t.Errorf("FAIL detail must frame the rate as feature-branch spend: %q", attr.detail)
	}
	if !strings.Contains(attr.hint, "feature/<issue-number>-slug") {
		t.Errorf("FAIL hint must give the branch-naming remedy: %q", attr.hint)
	}
	// The $0.80 on main is exploratory; it must not appear in the attribution gap.
	if strings.Contains(attr.detail, "0.80") {
		t.Errorf("attribution FAIL must not fold exploratory main spend into the gap: %q", attr.detail)
	}
	// The FAIL path must also never mix units or read as an accusation (review follow-up).
	if strings.Contains(attr.detail, "% of your AI events") {
		t.Errorf("FAIL detail mixes an event-rate with dollars (#488): %q", attr.detail)
	}
	if low := strings.ToLower(attr.detail); strings.Contains(low, "overhead") || strings.Contains(low, "accomplished nothing") {
		t.Errorf("FAIL detail reads as an accusation of waste (#488): %q", attr.detail)
	}
}

// TestEvalCapture_BareSentinelIsExploratory pins the default-arm claim (#488): the
// BARE unattributed sentinel (collector.UnattributedIssueID — emitted by the proxy /
// org pollers when git context is absent) is NOT feature-branch work. It must be
// excluded from the named-branch denominator (so it cannot drag the rate down) and
// reported as "uncategorized" exploratory spend. Re-pins the exact regression the
// removed !=-vs-IsUnattributed test guarded.
func TestEvalCapture_BareSentinelIsExploratory(t *testing.T) {
	events := []collector.TokenEvent{
		{IssueID: "236", CostMicro: 100_000},                         // $0.10 attributed
		{IssueID: collector.UnattributedIssueID, CostMicro: 400_000}, // $0.40 bare sentinel
	}
	got := evalCapture(events, nil, doctorSinceDays, defaultMinAttribution)
	// branchWork is ONLY the $0.10 attributed (bare sentinel excluded) -> rate 100% ->
	// OK. If the bare sentinel were miscounted as branch-without-issue, branchWork would
	// be $0.50 -> 20% -> FAIL; this OK assertion catches that regression.
	attr := findCheck(t, got, "issue attribution")
	if attr.status != statusOK {
		t.Errorf("issue attribution = %v, want OK: the bare sentinel must be excluded from feature-branch work, not drag the rate down (#488)", attr.status)
	}
	info := findCheck(t, got, "exploratory spend")
	if !strings.Contains(info.detail, "$0.40 uncategorized") {
		t.Errorf("bare sentinel must be reported as uncategorized exploratory spend: %q", info.detail)
	}
}

// TestDefaultMinAttribution pins the maintainer-overturnable default floor at 0.5 (#352).
func TestDefaultMinAttribution(t *testing.T) {
	if defaultMinAttribution != 0.5 {
		t.Errorf("default attribution floor = %g, want 0.5 (#352)", defaultMinAttribution)
	}
}

func mkResp(status int, date string, body string) (*http.Response, []byte) {
	h := http.Header{}
	if date != "" {
		h.Set("Date", date)
	}
	return &http.Response{StatusCode: status, Header: h}, []byte(body)
}

func TestEvalServerResponse(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	nowHdr := now.Format(http.TimeFormat)
	const embeddedVer = 7

	t.Run("401 fails auth", func(t *testing.T) {
		resp, body := mkResp(http.StatusUnauthorized, nowHdr, "")
		got := evalServerResponse(resp, body, now, embeddedVer)
		if statusOf(got, "server auth") != statusFail {
			t.Errorf("want auth FAIL, got %+v", got)
		}
	})
	t.Run("403 fails auth", func(t *testing.T) {
		resp, body := mkResp(http.StatusForbidden, nowHdr, "")
		got := evalServerResponse(resp, body, now, embeddedVer)
		if statusOf(got, "server auth") != statusFail {
			t.Errorf("want auth FAIL, got %+v", got)
		}
	})
	t.Run("500 fails reachable", func(t *testing.T) {
		resp, body := mkResp(http.StatusInternalServerError, nowHdr, "")
		got := evalServerResponse(resp, body, now, embeddedVer)
		if statusOf(got, "server reachable") != statusFail {
			t.Errorf("want reachable FAIL, got %+v", got)
		}
	})
	t.Run("200 healthy", func(t *testing.T) {
		resp, body := mkResp(http.StatusOK, nowHdr, `{"price_table":{"version":7}}`)
		got := evalServerResponse(resp, body, now, embeddedVer)
		if statusOf(got, "server auth") != statusOK {
			t.Errorf("want auth OK")
		}
		if statusOf(got, "clock offset") != statusOK {
			t.Errorf("want clock OK")
		}
		if statusOf(got, "price table") != statusOK {
			t.Errorf("want price OK")
		}
	})
	t.Run("clock skew warns (local ahead)", func(t *testing.T) {
		skewed := now.Add(10 * time.Minute) // local is 10m ahead of server Date
		resp, body := mkResp(http.StatusOK, nowHdr, `{"price_table":{"version":7}}`)
		got := evalServerResponse(resp, body, skewed, embeddedVer)
		if statusOf(got, "clock offset") != statusWarn {
			t.Errorf("want clock WARN, got %+v", got)
		}
	})
	t.Run("clock skew warns (server ahead)", func(t *testing.T) {
		// localNow BEHIND the server Date exercises the skew<0 negation branch.
		behind := now.Add(-10 * time.Minute)
		resp, body := mkResp(http.StatusOK, nowHdr, `{"price_table":{"version":7}}`)
		got := evalServerResponse(resp, body, behind, embeddedVer)
		if statusOf(got, "clock offset") != statusWarn {
			t.Errorf("want clock WARN for server-ahead skew, got %+v", got)
		}
	})
	t.Run("clock boundary is OK", func(t *testing.T) {
		// Exactly at the threshold must be OK (strict > for the WARN).
		at := now.Add(doctorClockSkewWarn)
		resp, body := mkResp(http.StatusOK, nowHdr, `{"price_table":{"version":7}}`)
		got := evalServerResponse(resp, body, at, embeddedVer)
		if statusOf(got, "clock offset") != statusOK {
			t.Errorf("want clock OK at exactly the threshold, got %+v", got)
		}
	})
	t.Run("missing Date header warns", func(t *testing.T) {
		resp, body := mkResp(http.StatusOK, "", `{"price_table":{"version":7}}`)
		got := evalServerResponse(resp, body, now, embeddedVer)
		if statusOf(got, "clock offset") != statusWarn {
			t.Errorf("a missing Date must surface a visible WARN, got %+v", got)
		}
	})
	t.Run("price mismatch warns", func(t *testing.T) {
		resp, body := mkResp(http.StatusOK, nowHdr, `{"price_table":{"version":99}}`)
		got := evalServerResponse(resp, body, now, embeddedVer)
		if statusOf(got, "price table") != statusWarn {
			t.Errorf("want price WARN, got %+v", got)
		}
	})
	t.Run("non-tierd 200 warns", func(t *testing.T) {
		// A 200 with no price_table (captive portal / wrong host) must not read as
		// "all checks passed" — the false-positive doctor exists to catch.
		resp, body := mkResp(http.StatusOK, nowHdr, `<html>hello</html>`)
		got := evalServerResponse(resp, body, now, embeddedVer)
		if statusOf(got, "price table") != statusWarn {
			t.Errorf("want price WARN for a non-tierd 200, got %+v", got)
		}
	})
}

// TestCheckServer_RoundTrip exercises the real HTTP path against a fake tierd.
func TestCheckServer_RoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"price_table":{"version":` + strconv.Itoa(store.ActivePriceTableInfo().Version) + `}}`))
	}))
	defer srv.Close()

	// Good token → reachable + auth OK.
	got := checkServer(context.Background(), srv.Client(), srv.URL, "good-token", time.Now())
	if statusOf(got, "server auth") != statusOK {
		t.Errorf("good token should authorize, got %+v", got)
	}
	// Bad token → 401 → auth FAIL.
	got = checkServer(context.Background(), srv.Client(), srv.URL, "bad", time.Now())
	if statusOf(got, "server auth") != statusFail {
		t.Errorf("bad token should FAIL auth, got %+v", got)
	}
}

// TestCheckServer_Unreachable proves a dead endpoint FAILs rather than panicking.
func TestCheckServer_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing is listening
	got := checkServer(context.Background(), http.DefaultClient, url, "", time.Now())
	if statusOf(got, "server reachable") != statusFail {
		t.Errorf("want reachable FAIL for a dead server, got %+v", got)
	}
}

func TestReportDoctor(t *testing.T) {
	var buf bytes.Buffer
	// All OK → exit 0.
	if code := reportDoctor([]checkResult{{name: "a", status: statusOK}}, &buf); code != 0 {
		t.Errorf("all-OK exit = %d, want 0", code)
	}
	// A WARN alone does not fail the process.
	buf.Reset()
	if code := reportDoctor([]checkResult{{name: "a", status: statusWarn, hint: "fix me"}}, &buf); code != 0 {
		t.Errorf("WARN-only exit = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), "fix me") {
		t.Errorf("hint should print under a WARN: %q", buf.String())
	}
	// Any FAIL → exit 1.
	buf.Reset()
	if code := reportDoctor([]checkResult{{name: "a", status: statusOK}, {name: "b", status: statusFail}}, &buf); code != 1 {
		t.Errorf("with-FAIL exit = %d, want 1", code)
	}
}

// TestRunDoctor_BadRepo proves runDoctor returns non-zero when --repo is not a
// git working tree — the end-to-end fail path through dispatch.
func TestRunDoctor_BadRepo(t *testing.T) {
	dir := t.TempDir() // exists, but has no .git
	var out, errBuf bytes.Buffer
	code := runDoctor([]string{"--repo", dir, "--claude-dir", t.TempDir()}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("runDoctor on a non-git dir = %d, want 1; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Errorf("expected a FAIL line, got %q", out.String())
	}
}

// TestRunDoctor_BadMinAttribution proves an out-of-range --min-attribution fails
// fast (exit 1) with a clear message rather than silently clamping (#352).
func TestRunDoctor_BadMinAttribution(t *testing.T) {
	// "NaN" parses cleanly via strconv.ParseFloat yet every ordered comparison
	// against it is false, so without a positive-range guard it would slip past
	// validation and silently disable the FAIL floor — it MUST be rejected (#352).
	for _, v := range []string{"1.5", "-0.1", "NaN"} {
		var out, errBuf bytes.Buffer
		code := runDoctor([]string{"--min-attribution", v, "--repo", t.TempDir(), "--claude-dir", t.TempDir()}, &out, &errBuf)
		if code != 1 {
			t.Fatalf("--min-attribution %s should exit 1, got %d (out=%s)", v, code, out.String())
		}
		if !strings.Contains(errBuf.String(), "min-attribution") {
			t.Errorf("--min-attribution %s: expected a min-attribution error, got %q", v, errBuf.String())
		}
	}
}

func TestCheckGitRepo(t *testing.T) {
	t.Run("not a git repo FAILs", func(t *testing.T) {
		got := checkGitRepo(t.TempDir(), "") // no .git
		if got.status != statusFail {
			t.Errorf("want FAIL, got %+v", got)
		}
	})
	t.Run("slug override is OK", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdir(t, filepath.Join(dir, ".git"))
		got := checkGitRepo(dir, "tiermetric/tier")
		if got.status != statusOK || !strings.Contains(got.detail, "tiermetric/tier") {
			t.Errorf("want OK with slug, got %+v", got)
		}
	})
	t.Run("no remote WARNs", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdir(t, filepath.Join(dir, ".git")) // .git dir but no config/remote
		got := checkGitRepo(dir, "")
		if got.status != statusWarn {
			t.Errorf("want WARN for missing remote.origin.url, got %+v", got)
		}
	})
	t.Run("remote resolves to OK", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdir(t, filepath.Join(dir, ".git"))
		if err := writeFile(filepath.Join(dir, ".git", "config"),
			"[remote \"origin\"]\n\turl = git@github.com:tiermetric/tier.git\n"); err != nil {
			t.Fatal(err)
		}
		got := checkGitRepo(dir, "")
		if got.status != statusOK || !strings.Contains(got.detail, "tiermetric/tier") {
			t.Errorf("want OK resolving the remote, got %+v", got)
		}
	})
}

func TestCheckClaudeProjects(t *testing.T) {
	t.Run("present is OK", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdir(t, filepath.Join(dir, "projects"))
		got := checkClaudeProjects(dir)
		if got.status != statusOK {
			t.Errorf("want OK, got %+v", got)
		}
	})
	t.Run("absent FAILs", func(t *testing.T) {
		got := checkClaudeProjects(t.TempDir()) // no projects/ subdir
		if got.status != statusFail {
			t.Errorf("want FAIL, got %+v", got)
		}
	})
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// errFake is a sentinel collect error for evalCapture's failure row.
var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "boom" }

// ptr is a local float64->*float64 helper: the wire share is a pointer so absent
// (server said nothing) is distinguishable from present-0.0 (nothing joined).
func ptrF(f float64) *float64 { return &f }

// TestCheckIdentityJoin covers #496. The discriminator is unjoined_developers
// (cost-only AND outcome-only both non-empty), NOT the share — a low or zero
// share is a coverage statement, because backfill imports 90 days of outcomes
// while Claude Code retains ~30 days of JSONL.
func TestCheckIdentityJoin(t *testing.T) {
	resp := func(cost float64, share *float64, u unjoinedDevelopers) scoresResponse {
		var s scoresResponse
		s.Total.WeightedPoints = 100 // nonzero; the gate keys on cost + share-nil
		s.Total.TotalCostUSD = cost
		s.DataQuality.AttributedOutcomeShare = share
		s.DataQuality.UnjoinedDevelopers = u
		return s
	}
	split := func(costOnly, outcomeOnly []string) unjoinedDevelopers {
		return unjoinedDevelopers{
			CostOnly: costOnly, OutcomeOnly: outcomeOnly,
			CostOnlyCount: len(costOnly), OutcomeOnlyCount: len(outcomeOnly),
		}
	}

	for _, tc := range []struct {
		name       string
		in         scoresResponse
		wantN      int // number of result rows
		wantName   string
		wantStatus checkStatus
		wantIn     []string // substrings across detail+hint
		wantNotIn  []string
	}{{
		// The #496 signature: both sides populated -> FAIL, name the fix.
		name:       "genuine split fails and names the remedy",
		in:         resp(27582.84, ptrF(0), split([]string{"asmith"}, []string{"a-smith-gh"})),
		wantN:      1,
		wantName:   "identity join",
		wantStatus: statusFail,
		wantIn:     []string{"asmith", "a-smith-gh", "developer_alias", "sums cost and points independently"},
	}, {
		// A single correct developer reaches share=0 with NO split (both lists
		// empty). This is R1: it must NOT fail, and must not mention aliasing.
		name:       "zero share with no split is coverage, not identity",
		in:         resp(12.34, ptrF(0), unjoinedDevelopers{}),
		wantN:      1,
		wantName:   "outcome coverage",
		wantStatus: statusWarn,
		wantIn:     []string{"window overlap"},
		wantNotIn:  []string{"developer_alias", "alias"},
	}, {
		// R2: a partial split (one bot joins, the fleet is split) must still FAIL,
		// driven by the counts, not the share magnitude.
		name:       "partial split still fails via counts",
		in:         resp(1000, ptrF(0.2), split([]string{"a", "b"}, []string{"c", "d"})),
		wantN:      1,
		wantName:   "identity join",
		wantStatus: statusFail,
		// R3: multiple names -> placeholder remedy, NOT a fabricated pairing.
		wantIn:    []string{"<cost-side id>"},
		wantNotIn: []string{`"alias":"a"`},
	}, {
		// Team-mode: names suppressed, counts survive -> still FAIL.
		name:       "anonymized split fails on counts alone",
		in:         resp(1000, ptrF(0), unjoinedDevelopers{CostOnlyCount: 12, OutcomeOnlyCount: 9}),
		wantN:      1,
		wantName:   "identity join",
		wantStatus: statusFail,
		wantIn:     []string{"12 developer(s)", "9 have outcomes"},
	}, {
		name:       "healthy share passes",
		in:         resp(27582.84, ptrF(0.69), unjoinedDevelopers{}),
		wantN:      1,
		wantName:   "identity join",
		wantStatus: statusOK,
		wantIn:     []string{"69%", "line up"},
	}, {
		// nil share = server reported no outcomes -> nothing to join, stay silent.
		name:  "nil share is silent",
		in:    resp(27582.84, nil, unjoinedDevelopers{}),
		wantN: 0,
	}, {
		name:  "no cost is silent",
		in:    resp(0, ptrF(0), unjoinedDevelopers{}),
		wantN: 0,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkIdentityJoin(tc.in)
			if len(got) != tc.wantN {
				t.Fatalf("got %d results, want %d: %+v", len(got), tc.wantN, got)
			}
			if tc.wantN == 0 {
				return
			}
			r := got[0]
			if r.name != tc.wantName {
				t.Errorf("name = %q, want %q", r.name, tc.wantName)
			}
			if r.status != tc.wantStatus {
				t.Errorf("status = %s, want %s (detail: %s)", r.status.label(), tc.wantStatus.label(), r.detail)
			}
			joined := r.detail + " " + r.hint
			for _, w := range tc.wantIn {
				if !strings.Contains(joined, w) {
					t.Errorf("missing %q in:\n  %s\n  %s", w, r.detail, r.hint)
				}
			}
			for _, w := range tc.wantNotIn {
				if strings.Contains(joined, w) {
					t.Errorf("unexpected %q in:\n  %s\n  %s", w, r.detail, r.hint)
				}
			}
		})
	}
}

// TestCheckIdentityJoin_Boundary pins the low-coverage threshold so a later edit
// to the constant or the comparator fails a test rather than sliding silently.
func TestCheckIdentityJoin_Boundary(t *testing.T) {
	mk := func(share float64) scoresResponse {
		var s scoresResponse
		s.Total.TotalCostUSD = 1000
		s.DataQuality.AttributedOutcomeShare = &share
		return s // no split -> coverage path
	}
	if got := checkIdentityJoin(mk(attributedOutcomeLowCoverage))[0].status; got != statusOK {
		t.Errorf("at the threshold, status = %s, want OK (>= is healthy)", got.label())
	}
	if got := checkIdentityJoin(mk(attributedOutcomeLowCoverage - 0.01))[0].status; got != statusWarn {
		t.Errorf("just below the threshold, status = %s, want WARN", got.label())
	}
}

// TestJoinSafe_SanitizesTerminalEscapes is the R2-from-code-review control: an
// identity is client-controlled and printed to a terminal, so a raw ANSI escape
// must not survive to clear the screen and forge doctor's own success line.
func TestJoinSafe_SanitizesTerminalEscapes(t *testing.T) {
	got := joinSafe([]string{"alice\x1b[2J\x1b[H\nFAKE doctor: all checks passed"})
	if strings.ContainsAny(got, "\x1b\n\r") {
		t.Errorf("joinSafe leaked a control byte: %q", got)
	}
	// And it caps the count so a huge fleet can't emit a multi-megabyte line.
	many := make([]string, 50)
	for i := range many {
		many[i] = "dev"
	}
	if out := joinSafe(many); !strings.Contains(out, "+47 more") {
		t.Errorf("joinSafe did not cap the list: %q", out)
	}
}

// TestEvalServerResponse_IdentityFailIsNonZeroExit is the control arm: a genuine
// split must drive a non-zero doctor exit, not merely print a line.
func TestEvalServerResponse_IdentityFailIsNonZeroExit(t *testing.T) {
	body := []byte(`{
	  "price_table": {"version": 8},
	  "total": {"weighted_points": 3728, "total_cost_usd": 27582.84},
	  "data_quality": {
	    "attributed_outcome_share": 0,
	    "unjoined_developers": {
	      "cost_only": ["asmith"], "outcome_only": ["a-smith-gh"],
	      "cost_only_count": 1, "outcome_only_count": 1
	    }
	  }
	}`)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	results := evalServerResponse(resp, body, time.Now(), 8)

	var found *checkResult
	for i := range results {
		if results[i].name == "identity join" {
			found = &results[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no identity join row: %+v", results)
	}
	if found.status != statusFail {
		t.Errorf("status = %s, want FAIL", found.status.label())
	}
	var buf bytes.Buffer
	if code := reportDoctor(results, &buf); code == 0 {
		t.Error("doctor exited 0 with a broken identity join; must be non-zero")
	}
	if !strings.Contains(buf.String(), "developer_alias") {
		t.Error("the remedy did not reach stdout")
	}
}

// TestEvalServerResponse_LegacyServerNoDataQuality is the R1 control: a pre-#351
// tierd emits price_table + total but no data_quality. The identity check must
// stay SILENT, not read the absent share as a total split and FAIL a healthy
// older server.
func TestEvalServerResponse_LegacyServerNoDataQuality(t *testing.T) {
	body := []byte(`{"price_table":{"version":8},"total":{"weighted_points":100,"total_cost_usd":50}}`)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	results := evalServerResponse(resp, body, time.Now(), 8)
	for _, r := range results {
		if r.name == "identity join" || r.name == "outcome coverage" {
			t.Errorf("identity check ran against a legacy no-data_quality body: %+v", r)
		}
	}
}

// TestEvalServerResponse_NonTierdBody: a 200 that parses but is not a tierd
// /scores response WARNs on price_table and must NOT emit an identity finding.
func TestEvalServerResponse_NonTierdBody(t *testing.T) {
	body := []byte(`{"message":"hello from some other service"}`)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	results := evalServerResponse(resp, body, time.Now(), 8)
	for _, r := range results {
		if r.name == "identity join" || r.name == "outcome coverage" {
			t.Errorf("identity check ran against a non-tierd body: %+v", r)
		}
	}
}
