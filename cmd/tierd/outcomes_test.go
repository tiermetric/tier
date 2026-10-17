package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func exclusionFixture(t *testing.T) (string, string, func() []int) {
	t.Helper()
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for n := 1; n <= 10; n++ {
		o := store.Outcome{Developer: "alice", Repo: "acme/app", PRNumber: n, MergeCommitSHA: fmt.Sprint("sha-", n), IssueID: fmt.Sprint("issue-", n), Weight: 3, Quality: 1, Timestamp: time.Date(2026, 10, n, 12, 0, 0, 0, time.UTC)}
		if n == 9 {
			o.Repo = "acme/other"
		}
		if n == 10 {
			o.MergeCommitSHA = ""
		}
		if _, err := db.RecordPROutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []struct {
		n           int
		head, title string
	}{{5, "revert-24-fix", "payload revert"}, {6, "ordinary", "Revert to v2 API"}} {
		pr := map[string]any{"number": e.n, "head": map[string]string{"ref": e.head}, "title": e.title}
		if e.n != 5 {
			pr["merge_commit_sha"] = fmt.Sprint("sha-", e.n)
		}
		raw, _ := json.Marshal(map[string]any{"number": e.n, "repository": map[string]string{"full_name": "Acme/App"}, "pull_request": pr})
		if err := db.InsertWebhookPayload(ctx, "pull_request", fmt.Sprint(e.n), raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var called []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("missing API authentication")
		}
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/repos/acme/app/pulls/%d", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		called = append(called, n)
		mu.Unlock()
		if n == 4 {
			http.NotFound(w, r)
			return
		}
		head, title := "normal", "ordinary change"
		switch n {
		case 1:
			head = "revert-99-feature"
		case 2:
			title = `Revert "feature"`
		case 3:
			title = "Revert to v2 API"
		case 7:
			head = ""
			title = ""
		case 8:
			head = "revert-42-wrong-merge"
		}
		sha := fmt.Sprint("sha-", n)
		if n == 8 {
			sha = "different-merge"
		}
		pr := map[string]any{"merge_commit_sha": sha, "head": map[string]string{"ref": head}, "title": title}
		if n != 2 {
			pr["number"] = n
		}
		_ = json.NewEncoder(w).Encode(pr)
	}))
	t.Cleanup(srv.Close)
	return path, srv.URL, func() []int { mu.Lock(); defer mu.Unlock(); return append([]int(nil), called...) }
}

func runExclusionCLI(t *testing.T, args ...string) (int, []byte, string) {
	t.Helper()
	var out, errs bytes.Buffer
	rc := dispatch(append([]string{"outcomes", "exclude-reverts"}, args...), &out, &errs)
	return rc, out.Bytes(), errs.String()
}

func dryExclusionPlan(t *testing.T, path, api string) revertPlan {
	t.Helper()
	rc, out, errout := runExclusionCLI(t, "--db", path, "--github-api-url", api)
	if rc != 0 {
		t.Fatalf("dry-run exit=%d: %s", rc, errout)
	}
	var p revertPlan
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

func saveExclusionPlan(t *testing.T, p revertPlan) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approved.json")
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Snapshot every table's row count as well as the main database bytes. This
// catches retention, registry, migration, and audit writes during a dry run.
func exclusionRowCounts(t *testing.T, path string) map[string]int {
	t.Helper()
	db, err := sql.Open("sqlite", store.ReadOnlyURI(path, "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	counts := map[string]int{}
	for _, n := range names {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + strings.ReplaceAll(n, `"`, `""`) + `"`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[n] = count
	}
	return counts
}

func TestExcludeReverts_DryRunEvidenceDeterministicAndNoWrites(t *testing.T) {
	path, api, calls := exclusionFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	counts := exclusionRowCounts(t, path)
	p := dryExclusionPlan(t, path, api)
	want := []string{"exclude", "exclude", "keep", "unknown", "exclude", "keep", "unknown", "unknown", "unknown", "unknown"}
	// acme/other sorts after acme/app, and PR 10 has no merge SHA.
	if len(p.Candidates) != 10 || p.Counts != (revertPlanCounts{10, 3, 2, 5}) {
		t.Fatalf("plan=%+v", p)
	}
	for i, r := range p.Candidates {
		if r.Verdict != want[i] {
			t.Errorf("candidate %+v want %s", r, want[i])
		}
		if (r.PR == 5 || r.PR == 6) && r.EvidenceSource != "payload" {
			t.Errorf("payload not preferred: %+v", r)
		}
	}
	if !reflect.DeepEqual(calls(), []int{1, 2, 3, 4, 7, 8}) {
		t.Fatalf("API calls=%v", calls())
	}
	p2 := dryExclusionPlan(t, path, api)
	if !reflect.DeepEqual(p, p2) {
		t.Fatal("unchanged evidence produced a different plan")
	}
	outPath := filepath.Join(t.TempDir(), "plan.json")
	if rc, _, e := runExclusionCLI(t, "--db", path, "--github-api-url", api, "--dry-run", "--out", outPath); rc != 0 {
		t.Fatal(e)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var filePlan revertPlan
	if err := json.Unmarshal(b, &filePlan); err != nil || !reflect.DeepEqual(filePlan, p) {
		t.Fatalf("file plan mismatch: %v", err)
	}
	if rc, _, _ := runExclusionCLI(t, "--db", path, "--github-api-url", api, "--out", path); rc == 0 {
		t.Fatal("allowed overwriting database")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !reflect.DeepEqual(counts, exclusionRowCounts(t, path)) {
		t.Fatal("dry run wrote database bytes/rows")
	}
}

func TestExcludeReverts_Filters(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	rc, out, e := runExclusionCLI(t, "--db", path, "--github-api-url", api, "--repo", "Acme/App", "--since", "2026-10-05")
	if rc != 0 {
		t.Fatal(e)
	}
	var p revertPlan
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 5 || p.Repo != "acme/app" || p.Since != "2026-10-05T00:00:00Z" {
		t.Fatalf("filtered plan=%+v", p)
	}
	for _, r := range p.Candidates {
		if r.PR < 5 || r.Repo != "acme/app" {
			t.Fatal(r)
		}
	}
}

func TestExcludeReverts_ApplyOnlyExcludesAndReplay(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	p := dryExclusionPlan(t, path, api)
	planPath := saveExclusionPlan(t, p)
	args := []string{"--db", path, "--apply", planPath, "--actor", "operator", "--approved-digest", p.digest()}
	for pass := 0; pass < 2; pass++ {
		rc, out, e := runExclusionCLI(t, args...)
		if rc != 0 {
			t.Fatalf("apply exit=%d: %s", rc, e)
		}
		var result struct{ Applied, Replayed, Skipped int }
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		if result.Skipped != 7 || (pass == 0 && (result.Applied != 3 || result.Replayed != 0)) || (pass == 1 && (result.Applied != 0 || result.Replayed != 3)) {
			t.Fatalf("result=%s", out)
		}
		db, err := store.OpenReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		exp, err := db.ExportDeveloper(context.Background(), "alice")
		_ = db.Close()
		if err != nil || len(exp.Outcomes) != 7 || len(exp.OutcomeExclusions) != 3 {
			t.Fatalf("export=%+v err=%v", exp, err)
		}
		for _, x := range exp.OutcomeExclusions {
			if x.Actor != "operator" || x.ExcludedAt.IsZero() || !strings.Contains(x.Evidence, p.digest()) || x.ID == nil {
				t.Fatal(x)
			}
			if x.ExclusionPRNumber != 1 && x.ExclusionPRNumber != 2 && x.ExclusionPRNumber != 5 {
				t.Fatalf("applied keep/unknown: %+v", x)
			}
		}
	}
	p2 := dryExclusionPlan(t, path, api)
	if len(p2.Candidates) != 7 || p2.Counts.Exclude != 0 {
		t.Fatalf("excluded rows returned to dry run: %+v", p2)
	}
}

func TestExcludeReverts_RejectsTamperingAndUnapprovedPlans(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	original := dryExclusionPlan(t, path, api)
	for _, kind := range []string{"tampered", "missing-actor", "wrong-approval", "conflicting-flags", "duplicate", "counts", "version", "exclude-reason", "keep-reason", "unknown-reason", "keep-evidence", "unknown-evidence", "payload-evidence", "payload-keep-evidence"} {
		t.Run(kind, func(t *testing.T) {
			p := original
			p.Candidates = append([]revertPlanRow(nil), original.Candidates...)
			if kind == "tampered" {
				p.Candidates[0].Title = "tampered"
			}
			if kind == "duplicate" {
				p.Candidates = append(p.Candidates, p.Candidates[0])
				p.Counts.Total++
				p.Counts.Exclude++
			}
			switch kind {
			case "counts":
				p.Counts.Total++
			case "version":
				p.Version = 2
			case "exclude-reason":
				p.Candidates[0].Reason = "no recognised GitHub revert form"
			case "keep-reason":
				p.Candidates[2].Reason = "recognised GitHub revert PR"
			case "unknown-reason":
				p.Candidates[3].Reason = "recognised GitHub revert PR"
			case "keep-evidence":
				p.Candidates[2].HeadRef = "revert-99-hidden"
			case "unknown-evidence":
				p.Candidates[3].HeadRef = "revert-99-hidden"
			case "payload-keep-evidence":
				for i := range p.Candidates {
					if p.Candidates[i].PR == 6 {
						p.Candidates[i].Title = "forged ordinary title"
					}
				}
			case "payload-evidence":
				for i := range p.Candidates {
					if p.Candidates[i].PR == 5 {
						p.Candidates[i].HeadRef = "revert-999-forged"
					}
				}
			}
			args := []string{"--db", path, "--apply", saveExclusionPlan(t, p), "--actor", "operator", "--approved-digest", p.digest()}
			if kind == "tampered" {
				args[7] = original.digest()
			}
			switch kind {
			case "missing-actor":
				args[5] = ""
			case "wrong-approval":
				args[7] = "bad"
			case "conflicting-flags":
				args = append(args, "--dry-run")
			}
			before := exclusionRowCounts(t, path)
			rc, _, e := runExclusionCLI(t, args...)
			if rc == 0 || e == "" {
				t.Fatal("invalid plan/flags accepted")
			}
			for k, want := range map[string]string{"counts": "counts invalid", "version": "version invalid", "payload-evidence": "payload evidence differs"} {
				if kind == k && !strings.Contains(e, want) {
					t.Fatalf("wrong rejection: %s", e)
				}
			}
			if !reflect.DeepEqual(before, exclusionRowCounts(t, path)) {
				t.Fatal("rejection wrote rows")
			}
		})
	}
}

func TestExcludeReverts_ErasedOutcomeNeverResurrected(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	p := dryExclusionPlan(t, path, api)
	planPath := saveExclusionPlan(t, p)
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EraseDeveloper(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	before := exclusionRowCounts(t, path)
	rc, _, e := runExclusionCLI(t, "--db", path, "--apply", planPath, "--actor", "operator", "--approved-digest", p.digest())
	if rc == 0 || !strings.Contains(e, "stale") {
		t.Fatalf("erased apply=%d %s", rc, e)
	}
	if !reflect.DeepEqual(before, exclusionRowCounts(t, path)) {
		t.Fatal("erased data recreated")
	}
	if p2 := dryExclusionPlan(t, path, api); len(p2.Candidates) != 0 {
		t.Fatal("erased rows returned")
	}
}

func TestExcludeReverts_FlagValidationAndTokenSecrecy(t *testing.T) {
	t.Setenv("TIER_GITHUB_TOKEN", "secret-do-not-echo")
	for _, args := range [][]string{{"--help"}, {"--since", "bad"}, {"--repo", "../bad"}, {"--github-api-url", "http://example.com"}, {"--unexpected"}} {
		_, out, e := runExclusionCLI(t, args...)
		if strings.Contains(string(out)+e, "secret-do-not-echo") {
			t.Fatal("token leaked")
		}
	}
	t.Setenv("TIER_GITHUB_TOKEN", "")
	if rc, _, _ := runExclusionCLI(t); rc == 0 {
		t.Fatal("empty token accepted")
	}
}

func TestExcludeReverts_ScoringEvidenceFloorAndExport(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM outcomes WHERE pr_number>3`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	read := func(wantN int, wantPoints float64, wantRanked bool) {
		t.Helper()
		db, err := store.OpenReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		rows, err := db.AllOutcomesWindow(context.Background(), time.Time{}, time.Time{}, store.FleetWide)
		if err != nil {
			t.Fatal(err)
		}
		var inputs []scoring.Outcome
		for _, o := range rows {
			inputs = append(inputs, scoring.Outcome{Weight: o.Weight, Quality: o.Quality})
		}
		contribs, costs := make([]float64, len(inputs)), make([]float64, len(inputs))
		for i, o := range inputs {
			contribs[i] = o.Weight * o.Quality
		}
		lo, hi := scoring.BootstrapCI(contribs, costs, 10, 100, rand.New(rand.NewPCG(1, 2)))
		if lo != wantPoints*100 || hi != wantPoints*100 {
			t.Fatalf("bootstrap retained excluded points: %v..%v", lo, hi)
		}
		score := scoring.ComputeDeveloper("alice", inputs, 10, 10, 0)
		if score.SampleN != wantN || score.WeightedPoints != wantPoints || score.Ranked != wantRanked || score.TIER != wantPoints*100 {
			t.Fatalf("score=%+v", score)
		}
		exported, _, err := db.ListOutcomes(context.Background(), time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 100)
		if err != nil || len(exported) != wantN {
			t.Fatalf("export=%+v %v", exported, err)
		}
	}
	read(3, 9, true)
	p := dryExclusionPlan(t, path, api)
	rc, _, e := runExclusionCLI(t, "--db", path, "--apply", saveExclusionPlan(t, p), "--actor", "operator", "--approved-digest", p.digest())
	if rc != 0 {
		t.Fatal(e)
	}
	read(1, 3, false)
}

func TestExcludeReverts_PartialApplyStopsAtStaleRow(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	p := dryExclusionPlan(t, path, api)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE outcomes SET pr_number=200 WHERE pr_number=2`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	args := []string{"--db", path, "--apply", saveExclusionPlan(t, p), "--actor", "operator", "--approved-digest", p.digest()}
	rc, _, e := runExclusionCLI(t, args...)
	if rc == 0 || !strings.Contains(e, "stale") || !strings.Contains(e, "committed 1 rows") {
		t.Fatalf("stale apply=%d %s", rc, e)
	}
	counts := exclusionRowCounts(t, path)
	if counts["outcomes"] != 9 || counts["outcome_exclusions"] != 1 {
		t.Fatalf("partial apply=%v", counts)
	}
	rc, _, e = runExclusionCLI(t, args...)
	if rc == 0 || !strings.Contains(e, "committed 0 rows") || !strings.Contains(e, "replayed 1 rows") {
		t.Fatalf("partial replay=%d %s", rc, e)
	}
	if !reflect.DeepEqual(counts, exclusionRowCounts(t, path)) {
		t.Fatal("partial replay changed rows")
	}
}

func TestExcludeReverts_TokenFileAndMissingDatabase(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("fixture-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TIER_GITHUB_TOKEN", "")
	if rc, _, e := runExclusionCLI(t, "--db", path, "--github-api-url", api, "--token", "@"+tokenFile); rc != 0 {
		t.Fatal(e)
	}
	t.Setenv("TIER_GITHUB_TOKEN", "fixture-token")
	p := dryExclusionPlan(t, path, api)
	missing := filepath.Join(t.TempDir(), "missing.db")
	if rc, _, _ := runExclusionCLI(t, "--db", missing, "--apply", saveExclusionPlan(t, p), "--actor", "operator", "--approved-digest", p.digest()); rc == 0 {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing apply created database: %v", err)
	}
}

// R1: the ordinary PR 3 control must not be hidden behind JSON key aliases.
func TestExcludeReverts_ExactPlanBytes(t *testing.T) {
	for _, form := range []string{"case-variant", "duplicate-key", "whitespace", "untouched"} {
		t.Run(form, func(t *testing.T) {
			path, api, _ := exclusionFixture(t)
			p := dryExclusionPlan(t, path, api)
			if form == "case-variant" || form == "duplicate-key" {
				for i := range p.Candidates {
					r := &p.Candidates[i]
					if r.PR == 3 {
						r.HeadRef, r.Verdict, r.Reason = "revert-99-hidden", "exclude", "recognised GitHub revert PR"
						p.Counts.Keep--
						p.Counts.Exclude++
					}
				}
			}
			planPath := saveExclusionPlan(t, p)
			if form == "untouched" {
				planPath = filepath.Join(t.TempDir(), "dry-run.json")
				rc, _, e := runExclusionCLI(t, "--db", path, "--github-api-url", api, "--out", planPath)
				if rc != 0 || !strings.Contains(e, p.digest()) {
					t.Fatalf("dry-run digest exit=%d: %s", rc, e)
				}
			}
			b, err := os.ReadFile(planPath)
			if err != nil {
				t.Fatal(err)
			}
			switch form {
			case "case-variant", "duplicate-key":
				// Restrict the ambiguous keys to the PR 3 ordinary-change control.
				start := bytes.Index(b, []byte(`"pr": 3,`))
				if start < 0 {
					t.Fatal("missing PR 3 control")
				}
				replacement := []byte(`"verdict": "keep", "Verdict": "exclude"`)
				if form == "duplicate-key" {
					replacement = []byte(`"verdict": "keep", "verdict": "keep", "Verdict": "exclude"`)
				}
				b = append(b[:start], bytes.Replace(b[start:], []byte(`"verdict": "exclude"`), replacement, 1)...)
			case "whitespace":
				var compact bytes.Buffer
				if err := json.Compact(&compact, b); err != nil {
					t.Fatal(err)
				}
				b = compact.Bytes()
			}
			if err := os.WriteFile(planPath, b, 0o600); err != nil {
				t.Fatal(err)
			}
			before := exclusionRowCounts(t, path)
			approved := fmt.Sprintf("%x", sha256.Sum256(b))
			if form == "untouched" && approved != p.digest() {
				t.Fatal("printed digest differs from raw file SHA-256")
			}
			rc, _, e := runExclusionCLI(t, "--db", path, "--apply", planPath, "--actor", "operator", "--approved-digest", approved)
			if form == "untouched" {
				if rc != 0 {
					t.Fatalf("untouched plan exit=%d: %s", rc, e)
				}
			} else if rc == 0 || !strings.Contains(e, "canonical dry-run output") || !reflect.DeepEqual(before, exclusionRowCounts(t, path)) {
				t.Fatalf("%s accepted or changed rows: exit=%d stderr=%s", form, rc, e)
			}
		})
	}
}

func TestExcludeReverts_ExcludeRequiresEvidence(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	p := dryExclusionPlan(t, path, api)
	for i := range p.Candidates {
		if p.Candidates[i].PR == 3 {
			p.Candidates[i].Verdict = "exclude"
			p.Counts.Keep--
			p.Counts.Exclude++
		}
	}
	before := exclusionRowCounts(t, path)
	rc, _, e := runExclusionCLI(t, "--db", path, "--apply", saveExclusionPlan(t, p), "--actor", "operator", "--approved-digest", p.digest())
	if rc == 0 || !strings.Contains(e, "lacks recognised revert evidence") || !reflect.DeepEqual(before, exclusionRowCounts(t, path)) {
		t.Fatalf("unevidenced PR 3 exit=%d stderr=%s", rc, e)
	}
}

func TestExcludeReverts_PayloadDifferentMergeSHA(t *testing.T) {
	path, api, calls := exclusionFixture(t)
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"number":3,"repository":{"full_name":"acme/app"},"pull_request":{"number":3,"head":{"ref":"revert-99-wrong-merge"},"title":"payload revert","merge_commit_sha":"other-sha"}}`)
	if err := db.InsertWebhookPayload(context.Background(), "pull_request", "wrong-merge", raw); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	p := dryExclusionPlan(t, path, api)
	for _, r := range p.Candidates {
		if r.PR == 3 && (r.EvidenceSource != "api" || r.Verdict != "keep") {
			t.Fatalf("wrong-merge payload used: %+v", r)
		}
	}
	found := false
	for _, n := range calls() {
		if n == 3 {
			found = true
		}
	}
	if !found {
		t.Fatal("PR 3 did not fall back to API")
	}
}

func TestExcludeReverts_APIFailuresAbortWithoutPlan(t *testing.T) {
	for _, code := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			path, _, _ := exclusionFixture(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
			defer srv.Close()
			outPath := filepath.Join(t.TempDir(), "plan.json")
			before := exclusionRowCounts(t, path)
			rc, out, e := runExclusionCLI(t, "--db", path, "--github-api-url", srv.URL, "--out", outPath)
			if rc == 0 || len(out) != 0 || !strings.Contains(e, fmt.Sprint(code)) || !strings.Contains(e, "fetch revert evidence") {
				t.Fatalf("API failure exit=%d out=%s stderr=%s", rc, out, e)
			}
			if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failure wrote plan: %v", err)
			}
			if !reflect.DeepEqual(before, exclusionRowCounts(t, path)) {
				t.Fatal("failure changed rows")
			}
		})
	}
}

// Run the command in a child so SIGINT exercises signal.NotifyContext without
// cancelling the test runner. The server proves the command reached getPR first.
func TestExcludeReverts_InterruptedDryRun(t *testing.T) {
	if os.Getenv("TIER_TEST_EXCLUSION_INTERRUPT") == "1" {
		rc := runOutcomesCmd([]string{"exclude-reverts", "--db", os.Getenv("TIER_TEST_EXCLUSION_DB"), "--github-api-url", os.Getenv("TIER_TEST_EXCLUSION_API"), "--out", os.Getenv("TIER_TEST_EXCLUSION_OUT")}, os.Stdout, os.Stderr)
		os.Exit(rc)
	}
	path, _, _ := exclusionFixture(t)
	reached := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(reached) })
		<-r.Context().Done()
	}))
	defer srv.Close()
	outPath := filepath.Join(t.TempDir(), "plan.json")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExcludeReverts_InterruptedDryRun$")
	cmd.Env = append(os.Environ(), "TIER_TEST_EXCLUSION_INTERRUPT=1", "TIER_TEST_EXCLUSION_DB="+path, "TIER_TEST_EXCLUSION_API="+srv.URL, "TIER_TEST_EXCLUSION_OUT="+outPath)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("command did not reach API")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(errs.String(), "context canceled") || out.Len() != 0 {
		t.Fatalf("interrupted exit=%v out=%s stderr=%s", err, out.String(), errs.String())
	}
	if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted command wrote plan: %v", err)
	}
}

func TestExcludeReverts_PrefersMergeDeliveryOverLaterEdits(t *testing.T) {
	path, api, calls := exclusionFixture(t)
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		action, sha, head, title string
		merged                   bool
	}{
		{"closed", "sha-3", "revert-99-at-merge", "merge title", true},
		{"edited", "sha-3", "normal", "later edit", true},
		{"closed", "sha-3", "normal", "closed but unmerged", false},
		{"closed", "other-merge", "normal", "wrong merge", true},
	} {
		raw, err := json.Marshal(map[string]any{"number": 3, "action": e.action, "repository": map[string]string{"full_name": "acme/app"}, "pull_request": map[string]any{"number": 3, "merged": e.merged, "merge_commit_sha": e.sha, "title": e.title, "head": map[string]string{"ref": e.head}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.InsertWebhookPayload(context.Background(), "pull_request", e.title, raw); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	p := dryExclusionPlan(t, path, api)
	for _, r := range p.Candidates {
		if r.PR == 3 && (r.EvidenceSource != "payload" || r.Verdict != "exclude" || r.HeadRef != "revert-99-at-merge" || r.Title != "merge title") {
			t.Fatalf("merge evidence not preferred: %+v", r)
		}
	}
	for _, n := range calls() {
		if n == 3 {
			t.Fatal("merge evidence fell back to API")
		}
	}
	if rc, _, e := runExclusionCLI(t, "--db", path, "--apply", saveExclusionPlan(t, p), "--actor", "operator", "--approved-digest", p.digest()); rc != 0 {
		t.Fatal(e)
	}
}

func TestExcludeReverts_SinceFractionalSeconds(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	const since = "2026-10-05T12:00:00.123456789Z"
	rc, out, e := runExclusionCLI(t, "--db", path, "--github-api-url", api, "--since", since)
	if rc != 0 {
		t.Fatal(e)
	}
	var p revertPlan
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	if p.Since != since {
		t.Fatalf("since truncated: %s", p.Since)
	}
}

func TestExcludeReverts_MissingRetainedPayloadRefusesBeforeExclusions(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	p := dryExclusionPlan(t, path, api)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM webhook_payloads`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	before := exclusionRowCounts(t, path)
	rc, _, e := runExclusionCLI(t, "--db", path, "--apply", saveExclusionPlan(t, p), "--actor", "operator", "--approved-digest", p.digest())
	if rc == 0 || !strings.Contains(e, "payload evidence differs") || !reflect.DeepEqual(before, exclusionRowCounts(t, path)) {
		t.Fatalf("missing payload exit=%d: %s", rc, e)
	}
}

func TestExcludeReverts_OutputFailure(t *testing.T) {
	path, api, _ := exclusionFixture(t)
	var errs bytes.Buffer
	rc := runOutcomesCmd([]string{"exclude-reverts", "--db", path, "--github-api-url", api}, failingWriter{}, &errs)
	if rc == 0 || errs.Len() == 0 || strings.Contains(errs.String(), "plan SHA-256") {
		t.Fatalf("failed output exit=%d: %s", rc, errs.String())
	}
}
