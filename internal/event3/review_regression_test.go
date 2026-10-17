package event3

import (
	"math/big"
	"testing"
)

func TestCorrectionReview_OriginalBindsBaseAndFloor(t *testing.T) {
	for _, tc := range []struct{ own, original, fixer string }{{"1", "31/20", "1"}, {"8", "3/10", "9/4"}} {
		o, f := node(1, "5", "1"), node(2, tc.own, "1", 1)
		f.Parents[0].Review.Original = rat("3")
		a := resolved(t, o, f)
		points(t, a, tc.original, tc.fixer)
		if a[1].RawWeight.Cmp(rat("5")) != 0 || a[1].Share.Cmp(rat("3")) != 0 || a[2].SourceWeight.Cmp(rat("3")) != 0 {
			t.Fatal(a)
		}
	}
}

func TestCorrectionPending_CoupledFinalChoices(t *testing.T) {
	o, g := node(1, "1/2", "1"), node(2, "1/2", "1/10")
	f, h := node(3, "1/2", "1", 1, 2), node(4, "1/2", "7/10", 1, 2)
	for _, n := range []Node{f, h} {
		for _, p := range n.Parents {
			p.Review.Original = rat("1/2")
		}
	}
	a := resolved(t, o, g, f, h)
	total := new(big.Rat)
	for _, v := range a {
		total.Add(total, v.Points)
	}
	if total.Cmp(rat("29/80")) != 0 || a[3].ParentID != 2 || a[4].ParentID != 1 {
		t.Fatalf("total=%s parents=%d,%d", total, a[3].ParentID, a[4].ParentID)
	}
}

func TestCorrectionPending_DependencyOrderAtEqualTime(t *testing.T) {
	for _, prs := range [][3]int{{30, 20, 40}, {30, 40, 20}} {
		o, f, g := node(1, "5", "1"), node(2, "1", "1", 1), node(3, "1", "1", 2)
		f.Parents[0].Review = nil
		f.Effort = nil
		f.MergedAt = o.MergedAt
		g.MergedAt = o.MergedAt
		o.PR, f.PR, g.PR = prs[0], prs[1], prs[2]
		a := resolved(t, g, f, o)
		points(t, a, "5", "1", "1")
		if a[2].Status != "pending" || a[3].Status != "pending" {
			t.Fatal(a)
		}
	}
}

func TestCorrectionSizing_EqualTimeExcludesDescendants(t *testing.T) {
	o, f, g := node(10, "1/2", "1"), node(30, "1/2", "1", 10), node(20, "1", "1", 30)
	f.Parents[0].Review = nil
	g.Parents[0].Review = nil
	f.MergedAt = o.MergedAt
	g.MergedAt = o.MergedAt
	o.Effort = &Effort{Additions: 15}
	f.Effort = &Effort{Additions: 1}
	g.Effort = &Effort{Additions: 45}
	a := resolved(t, g, f, o)
	if a[30].Before.Cmp(rat("1/2")) != 0 || a[30].After.Cmp(rat("1")) != 0 || a[20].Before.Cmp(rat("1")) != 0 || a[20].After.Cmp(rat("3")) != 0 {
		t.Fatalf("middle before/after=%s/%s last=%s/%s (effort total 61)", a[30].Before, a[30].After, a[20].Before, a[20].After)
	}
}

func TestCorrectionPending_ConfirmedForeignSiblingPaidFirst(t *testing.T) {
	o, g, m, f := node(1, "5", "1"), node(2, "5", "1"), node(3, "1", "1", 1, 2), node(4, "5", "1", 1)
	g.Developer = "foreign"
	f.Developer = "bob"
	before := resolved(t, o, g, f)
	after := resolved(t, o, g, m, f)
	if before[4].Points.Cmp(rat("15/4")) != 0 || after[4].Points.Cmp(before[4].Points) != 0 {
		t.Fatalf("confirmed sibling before=%s after=%s", before[4].Points, after[4].Points)
	}
	if after[3].ParentID != 1 || after[3].Status != "pending" {
		t.Fatal(after)
	}
}

func TestCorrectionProvisional_CrossAuthorDeclaredReviewed(t *testing.T) {
	o, f := node(1, "5", "1"), node(2, "1", "1", 1)
	f.Developer = "bob"
	f.Parents[0].State = "declared"
	a := resolved(t, o, f)
	points(t, a, "5", "1")
	if !a[2].Provisional {
		t.Fatal("review incorrectly confirms relation")
	}
}

func TestCorrectionReview_SubchainRetainsStandaloneParentShare(t *testing.T) {
	o, f, g := node(1, "5", "1"), node(2, "1", "1", 1), node(3, "1", "1", 2)
	f.Developer = "bob"
	f.Parents[0].State = "declared"
	g.Developer = "carol"
	f.MergedAt = o.MergedAt
	g.MergedAt = o.MergedAt
	o.PR, f.PR, g.PR = 20, 10, 30
	a := resolved(t, o, f, g)
	points(t, a, "5", "1/10", "3/4")
	if a[2].Share.Cmp(rat("1")) != 0 || a[3].SourceWeight.Cmp(rat("1")) != 0 {
		t.Fatal("subchain lost its incoming own share", a)
	}
}
