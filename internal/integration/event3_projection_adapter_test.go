//go:build event3replay

package integration

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/event3"
	"github.com/tiermetric/tier/internal/store"
)

// This adapter translates the replay contract to U1–U4's public interfaces.
// Exact assertions read persisted rational evidence, never reconstructed floats.
// No stubs, skips, reflection, SQL, or alternate allocation algorithm.
type replayProjection struct {
	Points, Quality, Share, Growth string
	Eligible                       bool
}

func replayProjected(t *testing.T, db *store.DB, o store.Outcome) replayProjection {
	t.Helper()
	rows, more, err := db.ListCorrectionEvidence(context.Background(), "allocations", time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 1000)
	if err != nil || more {
		t.Fatalf("allocation evidence: truncated=%v err=%v", more, err)
	}
	for _, row := range rows {
		if row["outcome_id"] != o.ID {
			continue
		}
		rat := func(key string) string {
			v, ok := row[key].(string)
			if !ok {
				t.Fatalf("PR%d missing exact %s", o.PRNumber, key)
			}
			r, err := event3.DecodeRat(v)
			if err != nil {
				t.Fatalf("PR%d %s: %v", o.PRNumber, key, err)
			}
			return r.RatString()
		}
		growth := rat("marginal_credit")
		// U1 persists a numeric zero placeholder alongside explicit unknown
		// eligibility for pending growth. Preserve that distinction at the seam.
		if row["ranking_eligibility"] == "unknown" {
			growth = ""
		}
		return replayProjection{Points: rat("allocated_points"), Quality: rat("projected_quality"), Share: rat("share"), Growth: growth, Eligible: row["ranking_eligibility"] == "eligible"}
	}
	t.Fatalf("PR%d missing persisted projection", o.PRNumber)
	return replayProjection{}
}

func replayReconcile(ctx context.Context, db *store.DB, id int64) error {
	return db.ReconcileCorrectionChain(ctx, id)
}

func replayDisposition(t *testing.T, db *store.DB, f replayFixture, a replayAction) {
	t.Helper()
	ctx := context.Background()
	p := f.pr(a.PR)
	rows, err := db.CorrectionRelationsByPR(ctx, p.repository(), a.PR)
	if err != nil {
		t.Fatal(err)
	}
	var edge map[string]any
	for _, r := range rows {
		if r["child_pr_number"] == int64(a.PR) && r["parent_pr_number"] == int64(a.Parent) {
			edge = r
			break
		}
	}
	if edge == nil {
		t.Fatalf("%s: no edge %d -> %d", f.Name, a.PR, a.Parent)
	}
	id, rev := edge["id"].(int64), edge["revision"].(int64)
	reason := fmt.Sprintf("synthetic %s: %s; %s %s#%d %s; parent %s %s#%d %s", f.Name, a.Kind, p.Author, p.repository(), p.Number, p.sha(), f.pr(a.Parent).Author, f.pr(a.Parent).repository(), a.Parent, f.pr(a.Parent).sha())
	cmd := store.CorrectionCommand{RelationID: id, Revision: rev, Actor: "operator", Role: "operator", Reason: reason}
	// Fixtures describe an audited unrelated/remove disposition. U2 exposes it
	// as dispute followed by operator settle-remove, rather than a direct
	// unrelated setter. Both transitions use fresh optimistic revisions.
	if a.Kind == "unrelated" || a.Kind == "settle-remove" {
		state := edge["state"]
		if state != "disputed" && state != "disputed-declared" && state != "withdrawn-confirmed" {
			if err := db.DisputeCorrection(ctx, cmd); err != nil {
				t.Fatalf("%s dispute before %s: %v", f.Name, a.Kind, err)
			}
			cmd.Revision++
		}
	}
	switch a.Kind {
	case "confirm":
		// U1 binds reviewed sizing to each Parent relation; U2 records it
		// atomically at the resulting confirmed revision.
		cmd.Review = &event3.Review{
			Original: replayRat(t, f.pr(a.Parent).Weight), Own: replayRat(t, a.Own),
			Before: replayRat(t, a.Before), After: replayRat(t, a.After),
			PureRepair: a.Before == a.After, Revision: rev,
			Scope: "synthetic adjudicated scope", Rationale: reason,
			Reviewer: cmd.Actor, ReviewedAt: replayTime(t, "2026-09-01T00:00:00Z"),
		}
		err = db.ConfirmCorrection(ctx, cmd)
	case "dispute":
		cmd.Actor, cmd.Role = p.Author, "declarer"
		err = db.DisputeCorrection(ctx, cmd)
	case "withdraw":
		cmd.Actor, cmd.Role = p.Author, "declarer"
		err = db.WithdrawCorrection(ctx, cmd)
	case "settle-uphold", "settle-remove", "unrelated":
		cmd.Settlement = map[string]string{"settle-uphold": "uphold", "settle-remove": "remove", "unrelated": "remove"}[a.Kind]
		err = db.SettleCorrection(ctx, cmd)
	default:
		t.Fatalf("unknown fixture action %q", a.Kind)
	}
	if err != nil {
		t.Fatalf("%s %s %d -> %d at revision %d: %v", f.Name, a.Kind, a.PR, a.Parent, rev, err)
	}
}

func replayRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.RatString() != s {
		t.Fatalf("noncanonical exact fixture rational %q", s)
	}
	return r
}

func replayTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
