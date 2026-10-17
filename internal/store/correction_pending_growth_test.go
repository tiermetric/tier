package store

import (
	"context"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/event3"
)

func TestCorrectionProjection_MultipleOriginalsUnknownGrowth(t *testing.T) {
	// Hand-adjudicated U5 D2 allocations. Unknown eligibility suppresses the
	// numeric credit placeholder at the replay seam; it is not known zero growth.
	for _, tc := range []struct {
		name, secondAuthor, childAuthor, secondWeight, rootPoints, rootQuality string
	}{
		{"distinct-foreign-roots-pending", "cy", "ben", "3", "5/1", "1/1"},
		{"mixed-own-and-foreign-roots-pending", "cy", "ada", "5", "13/4", "17/20"},
		{"least-favourable-own-root", "ada", "ada", "3", "13/4", "17/20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			root := correctionOutcome(t, db, "ada", "replay/alpha", "root101", 101)
			second := correctionOutcome(t, db, tc.secondAuthor, "replay/alpha", "root104", 104)
			child := correctionOutcome(t, db, tc.childAuthor, "replay/alpha", "child102", 102)
			outcomes := []Outcome{root, second, child}
			for i := range outcomes {
				o := &outcomes[i]
				weight := []string{"5", tc.secondWeight, "1"}[i]
				o.Weight, _ = projectionRat(weight).Float64()
				at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
				if o.ID == child.ID {
					at = at.Add(24 * time.Hour)
				}
				o.MergedAt = &at
				zero := 0
				o.SetDiffStats(&zero, &zero, &zero)
				if _, err := db.db.ExecContext(ctx, `UPDATE outcomes SET weight=?,merged_at=?,additions=0,deletions=0,changed_files=0 WHERE id=?`, o.Weight, at, o.ID); err != nil {
					t.Fatal(err)
				}
				body := "Corrects: none"
				if o.ID == child.ID {
					body = "Corrects: #101, #104"
				}
				recordCorrection(t, db, *o, body, "synthetic adjudicated D2 scope")
			}
			if err := db.ReconcileCorrectionChain(ctx, child.ID); err != nil {
				t.Fatal(err)
			}
			rows := projectionRows(t, db)
			if len(rows) != 3 {
				t.Fatalf("allocation count=%d want 3", len(rows))
			}
			want := map[int64]map[string]string{
				root.ID:   {"allocated_points": tc.rootPoints, "projected_quality": tc.rootQuality, "ranking_eligibility": "eligible"},
				second.ID: {"allocated_points": tc.secondWeight + "/1", "projected_quality": "1/1", "ranking_eligibility": "eligible"},
				child.ID:  {"allocated_points": "1/1", "projected_quality": "1/1", "share": "1/1", "ranking_eligibility": "unknown", "status": "pending", "pending_reason": "multiple-originals"},
			}
			for _, row := range rows {
				for key, value := range want[rowInt(row, "outcome_id")] {
					if rowString(row, key) != value {
						t.Errorf("outcome %d %s=%v want %s", rowInt(row, "outcome_id"), key, row[key], value)
					}
				}
				// Mirror U5's persisted-evidence adapter exactly: only known
				// eligibility exposes a numeric marginal growth value.
				credit, err := event3.DecodeRat(rowString(row, "marginal_credit"))
				if err != nil {
					t.Fatal(err)
				}
				growth := credit.RatString()
				if rowString(row, "ranking_eligibility") == "unknown" {
					growth = ""
				}
				if rowInt(row, "outcome_id") == child.ID && growth != "" {
					t.Errorf("PR102 growth=%s want unknown", growth)
				}
			}
			if err := (reader{q: db.db}).attachCorrectionProjections(ctx, outcomes); err != nil {
				t.Fatal(err)
			}
			for _, o := range outcomes {
				if o.Projection == nil || o.Projection.RankingEligibility != want[o.ID]["ranking_eligibility"] {
					t.Fatalf("PR%d reader eligibility: %+v", o.PRNumber, o.Projection)
				}
			}
		})
	}
}
