package webhook

import (
	"bytes"
	"context"
	"encoding/json"
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

// The fixture retains GitHub's actual PR fields from kubernetes/kubernetes
// #142567 (2026-10-02), projected into a closed pull_request delivery envelope.
func TestPRRevert_GitHubButtonPayload(t *testing.T) {
	body, err := os.ReadFile("testdata/github-revert-button.json")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	h := New(db, testSecret, quietLogger())
	var p prPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.PullRequest.Head.Ref != "revert-140664-pre-quota" || p.PullRequest.Number != 142567 {
		t.Fatal("real fixture changed")
	}
	for i := 0; i < 2; i++ {
		if err := h.handlePR(ctx, body); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := db.OutcomeByMergeCommit(ctx, p.PullRequest.MergeCommitSHA); err != nil || found {
		t.Fatalf("revert scored=%v %v", found, err)
	}
	exp, err := db.ExportDeveloper(ctx, p.PullRequest.User.Login)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.OutcomeExclusions) != 1 || exp.OutcomeExclusions[0].Developer != p.PullRequest.User.Login || exp.OutcomeExclusions[0].Evidence != `{"head_ref":"revert-140664-pre-quota"}` {
		t.Fatalf("author tombstone=%+v", exp)
	}
}

func TestPRRevert_NoIssueAndOrdinaryTitleControl(t *testing.T) {
	for _, tc := range []struct {
		name, head, title, body string
		exclude                 bool
	}{
		{"quoted-title-no-issue", "normal-branch", `Revert "original"`, "", true},
		{"branch-no-issue", "revert-2026-topic", "renamed", "", true},
		{"ordinary-title", "feature/42-topic", "Revert to v2 API…", "Closes #42", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			body, err := json.Marshal(map[string]any{"action": "closed", "repository": map[string]string{"full_name": "acme/app"}, "pull_request": map[string]any{"number": 43, "merged": true, "merge_commit_sha": "merge", "title": tc.title, "body": tc.body, "head": map[string]string{"ref": tc.head}, "user": map[string]string{"login": "alice", "type": "User"}, "additions": 40}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := New(db, testSecret, quietLogger()).handlePR(ctx, body); err != nil {
				t.Fatal(err)
			}
			o, found, err := db.OutcomeByMergeCommit(ctx, "merge")
			if err != nil || found == tc.exclude {
				t.Fatalf("score found=%v err=%v", found, err)
			}
			exp, err := db.ExportDeveloper(ctx, "alice")
			if err != nil {
				t.Fatal(err)
			}
			if tc.exclude {
				if len(exp.OutcomeExclusions) != 1 || exp.OutcomeExclusions[0].ID != nil || exp.OutcomeExclusions[0].Developer != "alice" {
					t.Fatalf("no-issue tombstone=%+v", exp)
				}
				st, err := db.RecordPushCommit(ctx, store.Outcome{Developer: "alice", Repo: "acme/app", IssueID: "issue-42", Timestamp: exp.OutcomeExclusions[0].Timestamp}, "2026-07-10", "merge", exp.OutcomeExclusions[0].Timestamp)
				if err != nil || st != store.PushCommitPRCaptured {
					t.Fatalf("later push credited: %v %v", st, err)
				}
			} else if o.Weight <= 0 || o.IssueID != "issue-42" {
				t.Fatalf("ordinary PR did not score: %+v", o)
			}
		})
	}
}

func TestPRRevert_MissingMergeIdentityWarnsAndSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		number int
		sha    string
	}{
		{"missing-sha", 43, ""}, {"missing-number", 0, "merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			var logs bytes.Buffer
			h := New(db, testSecret, slog.New(slog.NewTextHandler(&logs, nil)))
			body, err := json.Marshal(map[string]any{"action": "closed", "pull_request": map[string]any{"merged": true, "number": tc.number, "merge_commit_sha": tc.sha, "title": `Revert "original"`, "user": map[string]string{"login": "alice"}}})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := h.handlePR(context.Background(), body); err != nil {
					t.Errorf("delivery %d: %v", i, err)
				}
			}
			if !strings.Contains(logs.String(), "level=WARN") {
				t.Errorf("missing warning: %s", logs.String())
			}
			exp, err := db.ExportDeveloper(context.Background(), "alice")
			if err != nil || exp.RowCount() != 0 {
				t.Fatalf("stored invalid identity: %+v %v", exp, err)
			}
		})
	}
}

func TestPRRevert_LateBotMergeWindowCensus(t *testing.T) {
	r := newReconcileHarness(t)
	ctx := context.Background()
	merged := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	since := time.Date(merged.Year(), merged.Month(), merged.Day(), 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	var roster []store.HierarchyRow
	for _, dev := range []string{"alice", "bob", "release-agent"} {
		roster = append(roster, store.HierarchyRow{Developer: dev, Team: "eng", Division: "div"})
	}
	if err := r.db.SeedDemoMembershipBaseline(ctx, roster); err != nil {
		t.Fatal(err)
	}
	if err := r.db.UpsertHierarchies(ctx, roster, "test"); err != nil {
		t.Fatal(err)
	}
	for _, dev := range []string{"alice", "bob", "release-agent"} {
		if err := r.db.InsertTokenEvent(ctx, store.TokenEvent{Developer: dev, Repo: reconcileRepo,
			IssueID: "issue-42", Source: "jsonl", Fidelity: "realtime", InputTok: 10000,
			CostMicro: 10000000, Timestamp: merged}); err != nil {
			t.Fatal(err)
		}
	}
	h := api.New(r.db, quietLogger(), "", nil, "test", api.RateLimitConfig{}, api.WithUnsealedRecompute())
	h.SetAggregation(scoring.AggregationTeam, 3)
	mux := http.NewServeMux()
	h.Register(mux)
	url := "/api/v1/scores?since=" + since.Format("2006-01-02") + "&until=" + until.Format("2006-01-02")
	for delivery := 0; delivery < 3; delivery++ {
		if delivery > 0 {
			r.deliver(t, "pull_request", map[string]any{
				"action": "closed", "repository": map[string]string{"full_name": reconcileRepo},
				"pull_request": map[string]any{"number": 43, "merged": true,
					"merge_commit_sha": "late-revert", "merged_at": merged.Format(time.RFC3339),
					"title": `Revert "original"`, "head": map[string]string{"ref": "revert-42-topic"},
					"user": map[string]string{"login": "release-agent", "type": "Bot"}},
			})
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		var resp struct {
			Teams       []json.RawMessage `json:"teams"`
			Total       json.RawMessage   `json:"total"`
			DataQuality struct {
				Uncounted struct {
					Bot int `json:"bot"`
				} `json:"uncounted_active_ids"`
			} `json:"data_quality"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
			t.Fatalf("scores: %d %s", rec.Code, rec.Body.String())
		}
		if delivery == 0 {
			if len(resp.Teams) != 1 || len(resp.Total) == 0 || string(resp.Total) == "null" {
				t.Fatalf("three captured roster members must clear k=3 before bot evidence: %s", rec.Body.String())
			}
		} else {
			if len(resp.Teams) != 0 || (len(resp.Total) != 0 && string(resp.Total) != "null") || resp.DataQuality.Uncounted.Bot != 1 {
				t.Errorf("late delivery %d: merge-window census must exclude bot and withhold k=3: %s", delivery, rec.Body.String())
			}
			bots, err := r.db.BotDevelopers(ctx, since, until)
			if err != nil || len(bots) != 1 || bots[0] != "release-agent" {
				t.Errorf("merge-window bot evidence = %v, err=%v", bots, err)
			}
		}
	}
}
