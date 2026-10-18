package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/tiermetric/tier/internal/quality"
	"github.com/tiermetric/tier/internal/store"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQualityResolveTriggersHelp(t *testing.T) {
	var out, errs bytes.Buffer
	if rc := dispatch([]string{"quality", "resolve-triggers", "--help"}, &out, &errs); rc != 0 || !strings.Contains(errs.String(), "operator sign-off") {
		t.Fatalf("rc=%d %s %s", rc, out.String(), errs.String())
	}
}

func triggerFixture(t *testing.T, refs, repos []string) (*store.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quality.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for i, ref := range refs {
		sha := quality.ParseCIRef(ref).HeadSHA
		if _, err := db.InsertOutcome(ctx, store.Outcome{Developer: fmt.Sprintf("dev-%d", i), IssueID: fmt.Sprintf("issue-%d", i), Repo: repos[i], Weight: 5, Quality: 0.7, MergeCommitSHA: sha, Timestamp: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if repos[i] == "" {
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = raw.Exec(`UPDATE outcomes SET repo='' WHERE merge_commit_sha=?`, sha)
			cerr := raw.Close()
			if err != nil || cerr != nil {
				t.Fatalf("empty-repo fixture: %v %v", err, cerr)
			}
		}
		o, ok, err := db.OutcomeByMergeCommit(ctx, sha)
		if err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		if _, err := db.AppendQualityEvent(ctx, store.QualityEvent{OutcomeID: o.ID, Developer: o.Developer, IssueID: o.IssueID, EventType: "ci_fail", SourceRef: ref, EventTS: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	return db, path
}

func retainedRun(t *testing.T, db *store.DB, sha, trigger string, runID int64) {
	t.Helper()
	raw := fmt.Sprintf(`{"action":"completed","repository":{"full_name":"acme/app","default_branch":"main"},"workflow_run":{"id":%d,"head_sha":%q,"run_attempt":1,"workflow_id":42,"event":%q,"head_branch":"main"}}`, runID, sha, trigger)
	if err := db.InsertWebhookPayload(context.Background(), "workflow_run", fmt.Sprintf("delivery-%d-%s", runID, trigger), []byte(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestResolveTriggersSourcesAndAmbiguity(t *testing.T) {
	db, _ := triggerFixture(t, []string{"payload:1:42", "api:1:42", "ambiguous:1:42", "empty:1:42", "legacy:1", "apiambig:1:42", "unknown:1:42"}, []string{"acme/app", "acme/app", "acme/app", "", "acme/app", "acme/app", "acme/app"})
	retainedRun(t, db, "payload", "schedule", 10)
	retainedRun(t, db, "ambiguous", "push", 11)
	retainedRun(t, db, "ambiguous", "push", 12)
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sha := r.URL.Query().Get("head_sha")
		calls[sha]++
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing token")
		}
		switch sha {
		case "api":
			_, _ = io.WriteString(w, `{"workflow_runs":[{"id":20,"head_sha":"api","workflow_id":42,"run_attempt":1,"event":"push"}]}`)
		case "apiambig":
			_, _ = io.WriteString(w, `{"workflow_runs":[{"id":21,"head_sha":"apiambig","workflow_id":42,"run_attempt":1,"event":"push"},{"id":22,"head_sha":"apiambig","workflow_id":42,"run_attempt":1,"event":"schedule"}]}`)
		case "unknown":
			_, _ = io.WriteString(w, `{"workflow_runs":[]}`)
		default:
			t.Errorf("unexpected API for %q", sha)
			_, _ = io.WriteString(w, `{"workflow_runs":[]}`)
		}
	}))
	defer server.Close()
	gh := &githubClient{http: server.Client(), baseURL: server.URL, token: "secret", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p, err := buildTriggerPlan(context.Background(), db, gh)
	if err != nil {
		t.Fatal(err)
	}
	if p.Resolved != 2 || p.Unresolved != 5 {
		t.Fatalf("%+v", p)
	}
	if *p.Candidates[0].Trigger != "schedule" || p.Candidates[0].Source != "payload" || *p.Candidates[1].Trigger != "push" || p.Candidates[1].Source != "api" {
		t.Fatalf("%+v", p.Candidates)
	}
	if len(calls) != 3 {
		t.Fatal(calls)
	}
	rows, err := db.LegacyCIEvents(context.Background())
	if err != nil || len(rows) != 7 {
		t.Fatalf("dry run wrote: %d %v", len(rows), err)
	}
	hist, err := db.QualityHistoryForOutcome(context.Background(), rows[0].OutcomeID)
	if err != nil || len(hist) != 0 {
		t.Fatalf("dry run audit: %+v %v", hist, err)
	}
}

func TestResolveTriggersCLIValidationAndApply(t *testing.T) {
	db, path := triggerFixture(t, []string{"sha:1:42"}, []string{"acme/app"})
	retainedRun(t, db, "sha", "schedule", 10)
	t.Setenv("TIER_GITHUB_TOKEN", "")
	for _, args := range [][]string{{"--github-api-url", "http://example.com", "--token", "secret"}, {"--github-api-url", "http://127.0.0.1", "--token", "secret"}, {"--dry-run"}} {
		var out, errs bytes.Buffer
		rc := runQualityCmd(append([]string{"resolve-triggers", "--db", path}, args...), &out, &errs)
		if rc == 0 {
			t.Fatalf("accepted invalid arguments %v", args)
		}
	}
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if rc := runQualityCmd([]string{"resolve-triggers", "--db", path, "--dry-run", "--token", "@" + tokenPath}, &out, &errs); rc != 0 {
		t.Fatal(errs.String())
	}
	var p triggerPlan
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Resolved != 1 || p.Unresolved != 0 {
		t.Fatal(p)
	}
	if q, _, err := db.OutcomeByMergeCommit(context.Background(), "sha"); err != nil || q.Quality != 0.7 {
		t.Fatalf("dry run changed quality %+v %v", q, err)
	}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	digest := planBytesDigest(out.Bytes())
	for _, flags := range [][]string{{}, {"--actor", "operator", "--approved-digest", "bad"}} {
		var out, errs bytes.Buffer
		args := append([]string{"resolve-triggers", "--db", path, "--apply", planPath}, flags...)
		if rc := runQualityCmd(args, &out, &errs); rc == 0 {
			t.Fatal("apply without matching sign-off succeeded")
		}
	}
	out.Reset()
	errs.Reset()
	if rc := runQualityCmd([]string{"resolve-triggers", "--db", path, "--apply", planPath, "--actor", "operator", "--approved-digest", digest}, &out, &errs); rc != 0 {
		t.Fatal(errs.String())
	}
	if !strings.Contains(out.String(), "applied: 1") {
		t.Fatal(out.String())
	}
	o, _, err := db.OutcomeByMergeCommit(context.Background(), "sha")
	if err != nil || o.Quality != 1 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestResolveTriggersRefusesDuplicateCaseVariantKeys(t *testing.T) {
	db, path := triggerFixture(t, []string{"sha:1:42"}, []string{"acme/app"})
	retainedRun(t, db, "sha", "schedule", 10)
	p, err := buildTriggerPlan(context.Background(), db, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.bytes()
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"trigger_event": "schedule"`), []byte(`"trigger_event": "push", "TRIGGER_EVENT": "schedule"`), 1)
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	rc := runQualityCmd([]string{"resolve-triggers", "--db", path, "--apply", planPath, "--actor", "operator", "--approved-digest", planBytesDigest(raw)}, &out, &errs)
	if rc == 0 || !strings.Contains(errs.String(), "plan is not canonical") {
		t.Fatalf("rc=%d stdout=%s stderr=%s", rc, &out, &errs)
	}
	o, _, err := db.OutcomeByMergeCommit(context.Background(), "sha")
	if err != nil || o.Quality != 0.7 {
		t.Fatalf("refused plan changed quality: %+v %v", o, err)
	}
}

func TestResolveTriggersRechecksPayloadEvidence(t *testing.T) {
	for _, mode := range []string{"matching", "missing", "trigger changed", "ambiguous", "wrong repo"} {
		t.Run(mode, func(t *testing.T) {
			db, path := triggerFixture(t, []string{"sha:1:42"}, []string{"acme/app"})
			retainedRun(t, db, "sha", "schedule", 10)
			p, err := buildTriggerPlan(context.Background(), db, nil)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing":
				raw, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = raw.Exec(`DELETE FROM webhook_payloads`)
				cerr := raw.Close()
				if err != nil || cerr != nil {
					t.Fatalf("%v %v", err, cerr)
				}
			case "trigger changed":
				push := "push"
				p.Candidates[0].Trigger = &push
			case "ambiguous":
				retainedRun(t, db, "sha", "schedule", 11)
			case "wrong repo":
				p.Candidates[0].Repo = "acme/other"
			}
			raw, err := p.bytes()
			if err != nil {
				t.Fatal(err)
			}
			planPath := filepath.Join(t.TempDir(), "plan.json")
			if err := os.WriteFile(planPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var out, errs bytes.Buffer
			rc := runQualityCmd([]string{"resolve-triggers", "--db", path, "--apply", planPath, "--actor", "operator", "--approved-digest", planBytesDigest(raw)}, &out, &errs)
			wantQuality := 0.7
			if mode == "matching" {
				wantQuality = 1
				if rc != 0 || !strings.Contains(out.String(), "applied: 1") {
					t.Fatalf("rc=%d stdout=%s stderr=%s", rc, &out, &errs)
				}
			} else if rc == 0 || !strings.Contains(errs.String(), "payload evidence changed") {
				t.Fatalf("rc=%d stdout=%s stderr=%s", rc, &out, &errs)
			}
			o, _, err := db.OutcomeByMergeCommit(context.Background(), "sha")
			if err != nil || o.Quality != wantQuality {
				t.Fatalf("%+v %v", o, err)
			}
			hist, err := db.QualityHistoryForOutcome(context.Background(), o.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "matching" {
				if len(hist) != 1 || !strings.Contains(hist[0].Reason, `"EvidenceSource":"payload"`) {
					t.Fatal(hist)
				}
			} else if len(hist) != 0 {
				t.Fatalf("rejected plan wrote audit: %+v", hist)
			}
		})
	}
}

func TestResolveTriggersReportsPreservedQuality(t *testing.T) {
	db, path := triggerFixture(t, []string{"sha:1:42"}, []string{"acme/app"})
	retainedRun(t, db, "sha", "schedule", 10)
	p, err := buildTriggerPlan(context.Background(), db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQuality(context.Background(), "dev-0", "issue-0", 0.1); err != nil {
		t.Fatal(err)
	}
	raw, err := p.bytes()
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	rc := runQualityCmd([]string{"resolve-triggers", "--db", path, "--apply", planPath, "--actor", "operator", "--approved-digest", planBytesDigest(raw)}, &out, &errs)
	r := p.Candidates[0]
	want := fmt.Sprintf("quality event %d (outcome %d): preserved non-event quality", r.ID, r.OutcomeID)
	if rc != 0 || !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "applied: 1") {
		t.Fatalf("rc=%d stdout=%s stderr=%s", rc, &out, &errs)
	}
	o, _, err := db.OutcomeByMergeCommit(context.Background(), "sha")
	if err != nil || o.Quality != 0.1 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestResolveTriggersAPIAsReviewedAndRejectsChangedRows(t *testing.T) {
	for _, change := range []string{"unchanged", "id", "outcome", "repo", "source_ref", "event_type", "merge_sha", "same trigger", "different trigger"} {
		t.Run(change, func(t *testing.T) {
			db, path := triggerFixture(t, []string{"sha:1:42"}, []string{"acme/app"})
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = io.WriteString(w, `{"workflow_runs":[{"id":20,"head_sha":"sha","workflow_id":42,"run_attempt":1,"event":"schedule"}]}`)
			}))
			gh := &githubClient{http: server.Client(), baseURL: server.URL, token: "secret", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			p, err := buildTriggerPlan(context.Background(), db, gh)
			server.Close()
			if err != nil || calls != 1 || p.Candidates[0].Source != "api" {
				t.Fatalf("plan=%+v calls=%d err=%v", p, calls, err)
			}
			raw, err := p.bytes()
			if err != nil {
				t.Fatal(err)
			}
			planPath := filepath.Join(t.TempDir(), "plan.json")
			if err := os.WriteFile(planPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			updates := map[string]string{
				"id":                `UPDATE quality_events SET id=id+100`,
				"outcome":           `UPDATE quality_events SET outcome_id=outcome_id+100`,
				"repo":              `UPDATE outcomes SET repo='acme/other'`,
				"source_ref":        `UPDATE quality_events SET source_ref='sha:2:42'`,
				"event_type":        `UPDATE quality_events SET event_type='ci_pass'`,
				"merge_sha":         `UPDATE outcomes SET merge_commit_sha='other'`,
				"same trigger":      `UPDATE quality_events SET trigger_event='schedule'`,
				"different trigger": `UPDATE quality_events SET trigger_event='push'`,
			}
			if query := updates[change]; query != "" {
				conn, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = conn.Exec(query)
				cerr := conn.Close()
				if err != nil || cerr != nil {
					t.Fatalf("mutation: %v %v", err, cerr)
				}
			}
			var out, errs bytes.Buffer
			rc := runQualityCmd([]string{"resolve-triggers", "--db", path, "--apply", planPath, "--actor", "operator", "--approved-digest", planBytesDigest(raw)}, &out, &errs)
			if change == "unchanged" {
				if rc != 0 || !strings.Contains(out.String(), "API-sourced rows applied as reviewed, not re-fetched: 1") {
					t.Fatalf("rc=%d stdout=%s stderr=%s", rc, &out, &errs)
				}
			} else {
				if rc == 0 || !strings.Contains(errs.String(), "stale trigger plan") {
					t.Fatalf("rc=%d stdout=%s stderr=%s", rc, &out, &errs)
				}
				hist, err := db.QualityHistoryForOutcome(context.Background(), p.Candidates[0].OutcomeID)
				if err != nil || len(hist) != 0 {
					t.Fatalf("rejected plan wrote audit: %+v %v", hist, err)
				}
			}
		})
	}
}
