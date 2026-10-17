package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestCorrectionVerify_KnownRubricsUseTheirOwnRule(t *testing.T) {
	for _, version := range []int{1, 2} {
		d := verifyDimRubric(manifestRubric{Version: version})
		if d.Status != dimUnchanged {
			t.Fatal(d)
		}
	}
	if d := verifyDimRubric(manifestRubric{Version: 999}); d.Status != dimChanged {
		t.Fatal(d)
	}
	f := seedVerifyDB(t)
	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ConfigureCorrectionScoring(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{}, api.WithCommit(commit), api.WithStoredScoringPolicy())
	mux := http.NewServeMux()
	h.RegisterReadOnly(mux)
	read := func(path string) json.RawMessage {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		return append(json.RawMessage{}, w.Body.Bytes()...)
	}
	q := "?since=" + verifyWindowSince + "&until=" + verifyWindowUntil + "&repo=acme/tier"
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(read("/api/v1/report_manifest"+q), &doc); err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(manifestResults{Scores: read("/api/v1/scores" + q)})
	if err != nil {
		t.Fatal(err)
	}
	doc["results"] = result
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "v2.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	// R6: install-wide revision movement outside this report is informational.
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{Developer: "unrelated", Repo: "other/repo", IssueID: "unrelated", MergeCommitSHA: "unrelated-outcome", Weight: 1, Quality: 1, Timestamp: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}

	// R6: changing the live flag must not override the manifest's policy.
	if err := db.ConfigureCorrectionScoring(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	m := readFixtureManifest(t, path)
	if len(m.unknownFields) != 0 {
		t.Fatal(m.unknownFields)
	}
	rc, out, stderr := runVerify(t, path, "--db", f.dbPath)
	if rc != 0 {
		t.Fatal(rc, out, stderr)
	}
	assertDimStatus(t, out, "correction policy", "INFO")
	if !strings.Contains(out, "current stored enabled=false") {
		t.Fatal("current policy drift was not disclosed", out)
	}
	// An unresolved live policy is likewise informational: the pinned
	// replay's receipt, projections, counts and digests still must pass.
	execOnFixture(t, f.dbPath, `UPDATE correction_policy SET scoring_enabled=1,rubric_version=2,rule_version='future-rule' WHERE id=1`)
	rc, out, stderr = runVerify(t, path, "--db", f.dbPath)
	if rc != rcReproduced || !strings.Contains(out, "current stored policy unavailable") {
		t.Fatal(rc, out, stderr)
	}
	assertDimStatus(t, out, "correction policy", "INFO")
}

func TestCorrectionReview_R12VerifyPolicyDimension(t *testing.T) {
	f := seedVerifyDB(t)
	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ConfigureCorrectionScoring(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{}, api.WithStoredScoringPolicy())
	mux := http.NewServeMux()
	h.RegisterReadOnly(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/report_manifest?since="+verifyWindowSince+"&until="+verifyWindowUntil, nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var doc map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	doc["correction_rule_version"] = "wrong-rule"
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "wrong-policy.json")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	rc, out, stderr := runVerify(t, path, "--db", f.dbPath)
	if rc != rcCannotCheck || strings.Contains(out, "REPRODUCED") || !strings.Contains(stderr, "not resolved") {
		t.Fatal(rc, out, stderr)
	}
	// R6 positive control: a real window edit still diverges through the digest.
	doc["correction_rule_version"] = "event3-v2"
	body, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.UpdateQualityForOutcome(context.Background(), f.outcomeIDs["sha-alice-issue-100"], 0.7, "ci_fail", "changed"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	rc, out, stderr = runVerify(t, path, "--db", f.dbPath)
	if rc != 1 || !strings.Contains(out, "outcomes_digest:") {
		t.Fatal(rc, out, stderr)
	}
}

func TestCorrectionVerify_AnonymizedRevisionWithheld(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		t.Run(mode.String(), func(t *testing.T) {
			f := seedVerifyDB(t)
			db, err := store.Open(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.ConfigureCorrectionScoring(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			query := "?since=" + verifyWindowSince + "&until=" + verifyWindowUntil
			var doc map[string]json.RawMessage
			body := serveJSONAs(t, f.dbPath, "/api/v1/report_manifest"+query, mode, 5, api.WithStoredScoringPolicy())
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "correction_projection_revision") || strings.Contains(string(body), "correction_revision") {
				t.Fatal("anonymized revision disclosed", string(body))
			}
			scores := serveJSONAs(t, f.dbPath, "/api/v1/scores"+query, mode, 5, api.WithStoredScoringPolicy())
			doc["results"], err = json.Marshal(manifestResults{Scores: scores})
			if err != nil {
				t.Fatal(err)
			}
			body, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "anonymized.json")
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			rc, out, stderr := runVerify(t, path, "--db", f.dbPath)
			if rc != rcReproduced || !strings.Contains(out, "projection revision withheld for anonymized aggregation") {
				t.Fatal(rc, out, stderr)
			}
			assertDimStatus(t, out, "correction policy", "INFO")
			assertDimStatus(t, out, "corrective revisions", "UNCHANGED")
		})
	}
}
