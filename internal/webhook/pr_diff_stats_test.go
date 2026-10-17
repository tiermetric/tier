package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

func TestWebhookPR_MissingStatsStayUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stats  map[string]any
		known  [3]bool
		weight float64
	}{
		{"omitted", nil, [3]bool{}, 0.5},
		{"null", map[string]any{"additions": nil, "deletions": nil, "changed_files": nil}, [3]bool{}, 0.5},
		{"zero", map[string]any{"additions": 0, "deletions": 0, "changed_files": 0}, [3]bool{true, true, true}, 0.5},
		{"partial", map[string]any{"additions": 40, "deletions": nil, "changed_files": 0}, [3]bool{true, false, true}, 1},
		{"label", nil, [3]bool{}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tier.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			pr := map[string]any{"number": 42, "body": "Closes #42", "merged": true, "merge_commit_sha": "merge", "user": map[string]string{"login": "alice"}}
			for k, v := range tc.stats {
				pr[k] = v
			}
			if tc.name == "label" {
				pr["labels"] = []map[string]string{{"name": "size/m"}}
			}
			body, err := json.Marshal(map[string]any{"action": "closed", "pull_request": pr})
			if err != nil {
				t.Fatal(err)
			}
			if err := New(db, testSecret, quietLogger()).handlePR(context.Background(), body); err != nil {
				t.Fatal(err)
			}
			o, found, err := db.OutcomeByMergeCommit(context.Background(), "merge")
			if err != nil || !found {
				t.Fatalf("read: %v, found %v", err, found)
			}
			if o.Weight != tc.weight {
				t.Errorf("weight = %v, want %v", o.Weight, tc.weight)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			var stats [3]sql.NullInt64
			if err := raw.QueryRow(`SELECT additions, deletions, changed_files FROM outcomes`).Scan(&stats[0], &stats[1], &stats[2]); err != nil {
				t.Fatal(err)
			}
			for i, known := range tc.known {
				if stats[i].Valid != known {
					t.Errorf("stat %d availability = %v, want %v", i, stats[i].Valid, known)
				}
			}
		})
	}
}
