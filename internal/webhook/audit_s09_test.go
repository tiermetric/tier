package webhook

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// S09-1: exercise the persisted timestamp, reporting window and delivery replay.
func TestAuditS091_MergedAt(t *testing.T) {
	merged := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want time.Time // zero means receipt-time fallback
	}{
		{"valid", json.RawMessage(`"` + merged.Format(time.RFC3339) + `"`), merged},
		{"offset", json.RawMessage(`"` + merged.In(time.FixedZone("offset", -5*3600)).Format(time.RFC3339) + `"`), merged},
		{"positive_half_hour_offset", json.RawMessage(`"` + merged.In(time.FixedZone("offset", 5*3600+30*60)).Format(time.RFC3339) + `"`), merged},
		{"fractional", json.RawMessage(`"` + merged.Add(123456789*time.Nanosecond).Format(time.RFC3339Nano) + `"`), merged.Add(123456789 * time.Nanosecond)},
		{"offset_hour_out_of_range", json.RawMessage(`"` + merged.Format("2006-01-02T15:04:05") + `+24:00"`), time.Time{}},
		{"offset_minute_out_of_range", json.RawMessage(`"` + merged.Format("2006-01-02T15:04:05") + `+00:60"`), time.Time{}},
		{"negative_offset_hour_out_of_range", json.RawMessage(`"` + merged.Format("2006-01-02T15:04:05") + `-24:00"`), time.Time{}},
		{"negative_offset_minute_out_of_range", json.RawMessage(`"` + merged.Format("2006-01-02T15:04:05") + `-00:60"`), time.Time{}},
		{"absent", nil, time.Time{}},
		{"null", json.RawMessage(`null`), time.Time{}},
		{"empty", json.RawMessage(`""`), time.Time{}},
		{"invalid", json.RawMessage(`"not-a-time"`), time.Time{}},
		{"number", json.RawMessage(`123`), time.Time{}},
		{"object", json.RawMessage(`{}`), time.Time{}},
		{"zero", json.RawMessage(`"0001-01-01T00:00:00Z"`), time.Time{}},
		{"future", json.RawMessage(`"` + merged.Add(96*time.Hour).Format(time.RFC3339) + `"`), time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconcileHarness(t)
			pr := map[string]any{
				"number": 7, "merged": true, "merge_commit_sha": wfMergeSHA,
				"head": map[string]any{"ref": "feature/42-thing"},
				"user": map[string]any{"login": "alice"},
			}
			if tc.raw != nil {
				pr["merged_at"] = tc.raw
			}
			payload := map[string]any{"action": "closed", "pull_request": pr,
				"repository": map[string]any{"full_name": reconcileRepo}}
			before := time.Now().UTC()
			r.deliver(t, "pull_request", payload)
			after := time.Now().UTC()
			o, found, err := r.db.OutcomeByMergeCommit(context.Background(), wfMergeSHA)
			if err != nil || !found {
				t.Fatalf("lookup: found=%v err=%v", found, err)
			}
			if tc.want.IsZero() {
				if o.Timestamp.Before(before) || o.Timestamp.After(after) {
					t.Fatalf("fallback timestamp = %v, want receipt between %v and %v", o.Timestamp, before, after)
				}
			} else {
				if !o.Timestamp.Equal(tc.want) || o.Timestamp.Location() != time.UTC {
					t.Fatalf("timestamp = %v, want %v in UTC", o.Timestamp, tc.want)
				}
				rows, _, err := r.db.ListOutcomes(context.Background(), merged.Add(-time.Hour), merged.Add(time.Hour), store.PageCursor{}, 10)
				if err != nil || len(rows) != 1 {
					t.Fatalf("merge reporting window: rows=%v err=%v, want one outcome", rows, err)
				}
			}
			r.deliver(t, "pull_request", payload)
			replayed, _, err := r.db.OutcomeByMergeCommit(context.Background(), wfMergeSHA)
			if err != nil || !replayed.Timestamp.Equal(o.Timestamp) {
				t.Fatalf("redelivery moved timestamp: %v -> %v, err=%v", o.Timestamp, replayed.Timestamp, err)
			}
		})
	}
}

// S09-1: late PR delivery must not restart the 48h CI or 60d revert window.
func TestAuditS091_MergedAtObservationWindows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		age      time.Duration
		delay    time.Duration
		want     float64
		workflow bool
	}{
		{"ci_within", 72 * time.Hour, 47 * time.Hour, 0.7, true},
		{"ci_outside", 72 * time.Hour, 49 * time.Hour, 1, true},
		{"revert_within", 61 * 24 * time.Hour, 59 * 24 * time.Hour, 0.1, false},
		{"revert_outside", 61 * 24 * time.Hour, 60*24*time.Hour + time.Second, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconcileHarness(t)
			merged := time.Now().UTC().Add(-tc.age).Truncate(time.Second)
			r.deliver(t, "pull_request", map[string]any{
				"action": "closed", "repository": map[string]any{"full_name": reconcileRepo},
				"pull_request": map[string]any{
					"number": 7, "merged": true, "merge_commit_sha": wfMergeSHA,
					"merged_at": merged.Format(time.RFC3339), "body": "closes #42",
					"user": map[string]any{"login": "alice"},
				},
			})
			if tc.workflow {
				r.deliver(t, "workflow_run", wfRunPayload(wfMergeSHA, "main", "failure", 1, merged.Add(tc.delay)))
			} else {
				r.push(t, pushCommit("revert-sha", `Revert "feat"`+"\n\nThis reverts commit "+wfMergeSHA+".", "bob", merged.Add(tc.delay)))
			}
			o, found, err := r.db.OutcomeByMergeCommit(context.Background(), wfMergeSHA)
			if err != nil || !found || o.Quality != tc.want {
				t.Fatalf("quality = %v, want %v; found=%v err=%v", o.Quality, tc.want, found, err)
			}
		})
	}
}

// #1113 / 6eaae499: an unresolved SHA must not turn a bare PR number or a
// body-only issue reference into a penalty against the latest issue outcome.
func TestAudit1113_RevertCommitSubjectFallback(t *testing.T) {
	unknown := "ffffffffffffffffffffffffffffffffffffffff"
	for _, tc := range []struct {
		name, message, targetSHA, issue string
		want                            float64
	}{
		{"bare_subject_number", `Revert "feat #42"`, unknown, "issue-42", 1},
		{"bare_body_number", `Revert "feat"` + "\n\nSee #42", unknown, "issue-42", 1},
		{"body_close_directive", `Revert "feat"` + "\n\ncloses #42", unknown, "issue-42", 1},
		{"body_tracker_key", `Revert "feat"` + "\n\nPROJ-42", unknown, "PROJ-42", 1},
		{"subject_close_directive", `Revert "feat (closes #42)"`, unknown, "issue-42", 0.1},
		{"subject_tracker_key", `Revert "feat PROJ-42"`, unknown, "PROJ-42", 0.1},
		{"folded_subject_directive", "Revert \"feat\ncloses\n#42\"", unknown, "issue-42", 0.1},
		{"subject_beats_body", `Revert "feat PROJ-42"` + "\n\ncloses #99", unknown, "PROJ-42", 0.1},
		{"resolved_sha_with_bare_number", `Revert "feat #99"`, wfMergeSHA, "issue-42", 0.1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconcileHarness(t)
			now := time.Now().UTC().Truncate(time.Second)
			_, err := r.db.RecordPROutcome(context.Background(), store.Outcome{
				Developer: "alice", IssueID: tc.issue, Repo: reconcileRepo,
				PRNumber: 7, Weight: 1, Quality: 1, MergeCommitSHA: wfMergeSHA,
				Timestamp: now.Add(-time.Hour),
			})
			if err != nil {
				t.Fatal(err)
			}
			r.push(t, pushCommit("revert-sha", tc.message+"\n\nThis reverts commit "+tc.targetSHA+".", "bob", now))
			o, found, err := r.db.OutcomeByMergeCommit(context.Background(), wfMergeSHA)
			if err != nil || !found || o.Quality != tc.want {
				t.Fatalf("quality = %v, want %v; found=%v err=%v", o.Quality, tc.want, found, err)
			}
			events, err := r.db.QualityEventsForOutcome(context.Background(), o.ID)
			wantEvents := 0
			if tc.want != 1 {
				wantEvents = 1
			}
			if err != nil || len(events) != wantEvents {
				t.Fatalf("quality events = %v, want %d; err=%v", events, wantEvents, err)
			}
		})
	}
}
