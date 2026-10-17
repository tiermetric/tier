package event3

import (
	"fmt"
	"math/big"
	"reflect"
	"testing"
	"time"
)

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}
func node(id int64, weight, quality string, parents ...int64) Node {
	n := Node{ID: id, Repo: "acme/app", PR: int(id), SHA: fmt.Sprint(id), Developer: "alice", MergedAt: time.Date(2026, 10, 1, 0, int(id), 0, 0, time.UTC), Weight: rat(weight), Quality: rat(quality), Effort: &Effort{Additions: 10, Files: 1}}
	for _, p := range parents {
		review := &Review{Original: rat("5"), Own: rat(weight), Before: rat("5"), After: rat("5"), PureRepair: true, Scope: "reviewed delivered scope", Rationale: "pure repair", Reviewer: "operator", ReviewedAt: n.MergedAt, Revision: 1}
		n.Parents = append(n.Parents, Parent{ID: p, State: "confirmed", Review: review})
	}
	return n
}
func resolved(t *testing.T, ns ...Node) map[int64]Allocation {
	t.Helper()
	a, err := ResolveChain(ns)
	if err != nil {
		t.Fatal(err)
	}
	m := map[int64]Allocation{}
	for _, v := range a {
		m[v.OutcomeID] = v
	}
	return m
}
func points(t *testing.T, a map[int64]Allocation, want ...string) {
	t.Helper()
	for i, w := range want {
		if a[int64(i+1)].Points.Cmp(rat(w)) != 0 {
			t.Fatalf("party %d points %s want %s (%+v)", i+1, a[int64(i+1)].Points, w, a)
		}
	}
}

func TestCorrectionAllocation_D1(t *testing.T) {
	for _, tc := range []struct{ name, q, fu, w, o, f string }{
		{"clean", "1", "1", "1", "13/4", "1"}, {"degraded fixer", "1", "7/10", "1", "13/4", "7/10"},
		{"strategic fixer", "1", "4/5", "1", "13/4", "4/5"}, {"quality reverted fixer", "1", "1/10", "1", "13/4", "1/10"},
		{"ci original", "7/10", "1", "1", "7/4", "1"}, {"quality reverted original", "1/10", "1", "1", "1/2", "0"},
		{"floor exhaustion", "1", "1", "8", "1/2", "15/4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := resolved(t, node(1, "5", tc.q), node(2, tc.w, tc.fu, 1))
			points(t, a, tc.o, tc.f)
			if a[2].Eligible != "ineligible" {
				t.Fatal("pure repair eligible")
			}
		})
	}
}
func TestCorrectionAllocation_ExactRationalsAndParentShare(t *testing.T) {
	a := resolved(t, node(1, "5", "1"), node(2, "1", "1", 1), node(3, "1", "1", 2))
	points(t, a, "13/4", "1/10", "3/4")
	a = resolved(t, node(1, "5", "1/10"), node(2, "1", "1", 1), node(3, "1", "1", 2))
	points(t, a, "1/2", "0", "0")
	// Half-point original, a repair, then a repair of that repair: no fixed grid.
	n := node(1, "1/2", "1")
	f, g := node(2, "1/2", "1", 1), node(3, "1/2", "1", 2)
	f.Parents[0].Review.Original = rat("1/2")
	a = resolved(t, n, f, g)
	points(t, a, "1/20", "3/80", "9/32")
	for _, s := range []string{"33/80", "63/200", "153/80"} {
		r := rat(s)
		enc, err := EncodeRat(r)
		if err != nil {
			t.Fatal(err)
		}
		back, err := DecodeRat(enc)
		if err != nil || back.Cmp(r) != 0 {
			t.Fatalf("%s roundtrip %s %v", s, enc, err)
		}
	}
}
func TestCorrectionChain_CanonicalOrderAndFloor(t *testing.T) {
	o, f, g := node(1, "5", "1"), node(2, "3", "1", 1), node(3, "3", "1", 1)
	g.MergedAt = f.MergedAt
	g.PR = 20
	f.PR = 10
	want := resolved(t, o, f, g)
	points(t, want, "1/2", "3", "3/4")
	for _, ns := range [][]Node{{g, o, f}, {f, g, o}, {f, o, g}, {o, g, f}, {g, f, o}} {
		if got := resolved(t, ns...); !reflect.DeepEqual(got, want) {
			t.Fatalf("arrival order changed projection: %+v", got)
		}
	}
	// IDs are links only: PR #10 precedes #20 even with inverted IDs.
	f.ID, g.ID = 30, 20
	got := resolved(t, g, f, o)
	if got[30].Points.Cmp(rat("3")) != 0 || got[20].Points.Cmp(rat("3/4")) != 0 {
		t.Fatal(got)
	}
}
func TestCorrectionMarginal_SecondRepairUsesPreviousChain(t *testing.T) {
	o, f, g := node(1, "5", "1"), node(2, "1", "1", 1), node(3, "1", "1", 1)
	f.Parents[0].Review.PureRepair = false
	f.Parents[0].Review.Before = rat("3")
	f.Parents[0].Review.After = rat("5")
	g.Parents[0].Review.PureRepair = false
	g.Parents[0].Review.Before = rat("5")
	g.Parents[0].Review.After = rat("5")
	a := resolved(t, o, f, g)
	if a[2].Credit.Cmp(rat("1")) != 0 || a[3].Credit.Sign() != 0 {
		t.Fatal(a)
	}
	f.Parents[0].Review = nil
	g.Parents[0].Review = nil
	o.Effort = &Effort{Additions: 5, Files: 1}
	f.Effort = &Effort{Additions: 1, Files: 0}
	g.Effort = &Effort{Additions: 1, Files: 0}
	a = resolved(t, o, f, g)
	if a[2].Credit.Cmp(rat("1/2")) != 0 || a[3].Credit.Sign() != 0 {
		t.Fatal(a)
	}
}
func TestCorrectionProvisional_SameAndCrossAuthor(t *testing.T) {
	o, f := node(1, "5", "1"), node(2, "1", "1", 1)
	f.Parents[0].State = "declared"
	f.Parents[0].Review = nil
	a := resolved(t, o, f)
	points(t, a, "13/4", "1")
	if !a[2].Provisional {
		t.Fatal(a)
	}
	f.Developer = "bob"
	a = resolved(t, o, f)
	points(t, a, "5", "1")
	if a[2].Quality.Cmp(rat("1")) != 0 || a[2].Eligible != "ineligible" {
		t.Fatal(a)
	}
}
func TestCorrectionPending_MissingEvidenceCyclesAndOffScale(t *testing.T) {
	for _, kind := range []string{"stats", "occurrence", "parent", "excluded", "cycle", "weight", "quality", "future-parent"} {
		t.Run(kind, func(t *testing.T) {
			o, f := node(1, "5", "1"), node(2, "1", "1", 1)
			f.Parents[0].Review = nil
			switch kind {
			case "stats":
				f.Effort = nil
			case "occurrence":
				f.MergedAt = time.Time{}
			case "parent":
				f.Parents[0].ID = 99
			case "excluded":
				f.Parents[0].State = "excluded"
			case "cycle":
				o.Parents = []Parent{{ID: 2, State: "declared"}}
			case "weight":
				f.Weight = rat("2")
			case "quality":
				f.Quality = rat("0")
			case "future-parent":
				o.MergedAt = f.MergedAt.Add(time.Hour)
			}
			a := resolved(t, o, f)
			if a[2].Status != "pending" || a[2].Points.Cmp(new(big.Rat).Mul(f.Weight, f.Quality)) != 0 {
				t.Fatal(a)
			}
		})
	}
}
func TestCorrectionPending_MultiRootLeastFavourableOwn(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		o, g, f := node(1, "5", "1"), node(2, "5", "1"), node(3, "1", "1", 1, 2)
		if foreign {
			g.Developer = "bob"
		}
		a := resolved(t, o, g, f)
		points(t, a, "13/4", "5", "1")
		if a[3].Status != "pending" || a[3].Eligible != "unknown" {
			t.Fatal(a)
		}
	}
	o, g, f := node(1, "5", "1"), node(2, "5", "1"), node(3, "1", "1", 1, 2)
	o.Quality = rat("1/10")
	a := resolved(t, o, g, f)
	points(t, a, "1/2", "13/4", "1")
	o.Developer = "bob"
	g.Developer = "carol"
	a = resolved(t, o, g, f)
	points(t, a, "1/2", "5", "1")
}
func TestCorrectionAllocation_MultipleParentsSharedAncestor(t *testing.T) {
	o, f, g := node(1, "5", "1"), node(2, "1", "1", 1), node(3, "1", "1", 1, 2)
	a := resolved(t, o, f, g)
	points(t, a, "13/4", "1/10", "3/4") // deepest named parent owns sub-chain, each child once
}
func TestCorrectionDispute_RetainsUntilSettlement(t *testing.T) {
	o, f := node(1, "5", "1"), node(2, "1", "1", 1)
	want := resolved(t, o, f)
	for _, state := range []string{"disputed", "withdrawn-confirmed"} {
		f.Parents[0].State = state
		got := resolved(t, o, f)
		for _, id := range []int64{1, 2} {
			if got[id].Points.Cmp(want[id].Points) != 0 {
				t.Fatal(got)
			}
		}
		if !got[2].Disputed {
			t.Fatal(got)
		}
	}
	f.Parents[0].State = "removed"
	a := resolved(t, o, f)
	points(t, a, "5", "1")
}
func TestCorrectionAllocation_QualityIsolation(t *testing.T) {
	o, f, g, s := node(1, "5", "1"), node(2, "1", "1", 1), node(3, "1", "1", 2), node(4, "1", "1", 1)
	a := resolved(t, o, f, g, s)
	g.Quality = rat("7/10")
	b := resolved(t, o, f, g, s)
	for _, id := range []int64{1, 2, 4} {
		if !reflect.DeepEqual(a[id], b[id]) {
			t.Fatalf("terminal changed %d", id)
		}
	}
	f.Quality = rat("7/10")
	c := resolved(t, o, f, g, s)
	for _, id := range []int64{1, 4} {
		if !reflect.DeepEqual(b[id], c[id]) {
			t.Fatalf("subchain changed ancestor/sibling %d", id)
		}
	}
}
func TestCorrectionReview_Validation(t *testing.T) {
	good := *node(2, "1", "1", 1).Parents[0].Review
	for _, field := range []string{"original", "own", "before", "after", "scope", "rationale", "reviewer", "time", "revision", "offscale", "negative", "repair-growth"} {
		t.Run(field, func(t *testing.T) {
			r := good
			switch field {
			case "original":
				r.Original = nil
			case "own":
				r.Own = nil
			case "before":
				r.Before = nil
			case "after":
				r.After = nil
			case "scope":
				r.Scope = " "
			case "rationale":
				r.Rationale = ""
			case "reviewer":
				r.Reviewer = ""
			case "time":
				r.ReviewedAt = time.Time{}
			case "revision":
				r.Revision = 0
			case "offscale":
				r.Own = rat("2")
			case "negative":
				r.After = rat("-1")
			case "repair-growth":
				r.After = rat("8")
			}
			if r.Validate() == nil {
				t.Fatal("invalid review accepted")
			}
		})
	}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"2/4", "01/2", "1/-2", "0/0", "1.5", "1", "-1/2"} {
		if _, err := DecodeRat(s); err == nil {
			t.Fatalf("noncanonical %s accepted", s)
		}
	}
}

func TestCorrectionAllocation_AmendedRationalVectors(t *testing.T) {
	o, f, g := node(1, "5", "1"), node(2, "3", "1", 1), node(3, "1", "11/20", 1)
	a := resolved(t, o, f, g)
	points(t, a, "1/2", "3", "33/80")
	o, f = node(1, "1/2", "1"), node(2, "1/2", "7/10", 1)
	f.Parents[0].Review.Original = rat("1/2")
	f.MergedAt = o.MergedAt.Add(15 * 24 * time.Hour)
	a = resolved(t, o, f)
	points(t, a, "1/20", "63/200")
	o, f = node(1, "5", "7/10"), node(2, "3", "17/20", 1)
	a = resolved(t, o, f)
	points(t, a, "1/2", "153/80")
	// A zero incoming share cannot manufacture points through growth downstream.
	o, f, g = node(1, "5", "1/10"), node(2, "1", "1", 1), node(3, "1", "1", 2)
	g.Parents[0].Review.PureRepair = false
	g.Parents[0].Review.Before = rat("3")
	g.Parents[0].Review.After = rat("5")
	a = resolved(t, o, f, g)
	points(t, a, "1/2", "0", "0")
}
func TestCorrectionReview_ExplicitPureRepairAndRawLabel(t *testing.T) {
	o, f := node(1, "5", "1"), node(2, "8", "1", 1)
	f.Parents[0].Review.Own = rat("1")
	f.Parents[0].Review.Before = nil
	f.Parents[0].Review.After = nil
	if err := f.Parents[0].Review.Validate(); err != nil {
		t.Fatal(err)
	}
	a := resolved(t, o, f)
	points(t, a, "13/4", "1")
	if a[2].Credit.Sign() != 0 {
		t.Fatal(a)
	}
}
func TestCorrectionChain_CanonicalIdentityAndSHATies(t *testing.T) {
	o, f, g := node(1, "5", "1"), node(2, "3", "1", 1), node(3, "3", "1", 1)
	f.MergedAt = g.MergedAt
	f.PR = g.PR
	f.SHA = "a"
	g.SHA = "b"
	a := resolved(t, g, o, f)
	points(t, a, "1/2", "3", "3/4")
	f.Repo = "acme/a"
	g.Repo = "acme/b"
	f.SHA = "z"
	g.SHA = "a"
	a = resolved(t, g, o, f)
	points(t, a, "1/2", "3", "3/4")
}
func TestCorrectionPending_MultiRootForeignByteIdentical(t *testing.T) {
	o, g, f := node(1, "5", "1"), node(2, "5", "1"), node(3, "1", "1", 1, 2)
	g.Developer = "foreign"
	alone := resolved(t, g)[2]
	a := resolved(t, o, g, f)
	if !reflect.DeepEqual(alone, a[2]) {
		t.Fatal("foreign root changed")
	}
	// Two same-author clean roots and a one-point repair total 9.25, not 11.
	g.Developer = "alice"
	a = resolved(t, o, g, f)
	total := new(big.Rat)
	for _, v := range a {
		total.Add(total, v.Points)
	}
	if total.Cmp(rat("37/4")) != 0 {
		t.Fatal(total)
	}
}
func TestCorrectionQuality_InclusiveWindowAndNoInputMutation(t *testing.T) {
	for _, delta := range []time.Duration{0, 14 * 24 * time.Hour, 14*24*time.Hour + time.Nanosecond} {
		o, f := node(1, "5", "1"), node(2, "1", "1", 1)
		f.MergedAt = o.MergedAt.Add(delta)
		a := resolved(t, o, f)
		want := "13/4"
		if delta > 14*24*time.Hour {
			want = "4"
		}
		points(t, a, want, "1")
		if o.Quality.Cmp(rat("1")) != 0 || o.Weight.Cmp(rat("5")) != 0 || f.Parents[0].Review.Own.Cmp(rat("1")) != 0 {
			t.Fatal("mutated caller-owned rationals")
		}
	}
}

// An independent integer oracle covers every supported weight, quality and
// reviewed-size input. Expected values use 1/40 units; outputs may leave that grid.
func TestCorrectionAllocation_ExhaustiveD1Scale(t *testing.T) {
	weights := []int64{20, 40, 120, 200, 320}
	for _, w := range weights {
		for _, q := range []int64{20, 17, 14, 11, 16, 2} {
			for _, own := range weights {
				for _, fuq := range []int64{20, 17, 14, 11, 16, 2} {
					for _, growth := range []bool{false, true} {
						o, f := node(1, ratInt(w, 40), ratInt(q, 20)), node(2, ratInt(own, 40), ratInt(fuq, 20), 1)
						f.Parents[0].Review.Original = new(big.Rat).Set(o.Weight)
						var credit int64
						if growth {
							f.Parents[0].Review.PureRepair = false
							f.Parents[0].Review.Before = rat("1/2")
							f.Parents[0].Review.After = rat("8")
							credit = own
							if credit > 300 {
								credit = 300
							}
						}
						corrected := q
						if q == 20 {
							corrected = 17
						}
						if q == 14 {
							corrected = 11
						}
						budget := w*corrected/20 + credit
						original := budget - own
						if original < w/10 {
							original = w / 10
						}
						share := budget - original
						if share < 0 {
							share = 0
						}
						if share > own {
							share = own
						}
						a := resolved(t, o, f)
						if a[1].Points.Cmp(big.NewRat(original, 40)) != 0 || a[2].Points.Cmp(big.NewRat(share*fuq, 800)) != 0 {
							t.Fatalf("w=%d q=%d own=%d fuq=%d growth=%v: %+v", w, q, own, fuq, growth, a)
						}
					}
				}
			}
		}
	}
}
func ratInt(n, d int64) string { return big.NewRat(n, d).RatString() }

func TestCorrectionPending_IncomparableParentSharesAreNotInvented(t *testing.T) {
	o, a, b, c := node(1, "5", "1"), node(2, "1", "1", 1), node(3, "1", "1", 1), node(4, "1", "1", 2, 3)
	got := resolved(t, o, a, b, c)
	if got[4].Status != "pending" || got[4].Reason != "unsupported-allocation-shape" || got[4].Points.Cmp(rat("1")) != 0 {
		t.Fatal(got)
	}
}

func TestCorrectionPending_NonConvergingChoicesReturn(t *testing.T) {
	// Inject a deterministic matching-pennies payoff through the private candidate
	// allocator: Alice prefers matching roots; Bob prefers mismatching roots.
	// Each best response changes the next one's winner, so there is no fixpoint.
	ns := []Node{node(1, "5", "1"), node(2, "5", "1"), node(3, "1", "1", 1, 2), node(4, "5", "1"), node(5, "5", "1"), node(6, "1", "1", 4, 5)}
	for i := 3; i < len(ns); i++ {
		ns[i].Developer = "bob"
	}
	type result struct {
		allocations []Allocation
		err         error
	}
	done := make(chan result, 1)
	go func() {
		a, err := resolveChain(ns, func(ns []Node, by map[int64]Node, selected map[int64]int64, bad map[int64]string, provisional, disputed map[int64]bool) []Allocation {
			matching := (selected[3] == 1) == (selected[6] == 4)
			alice, bob := r(0, 1), r(1, 1)
			if !matching {
				alice, bob = bob, alice
			}
			return []Allocation{{OutcomeID: 1, Points: alice}, {OutcomeID: 4, Points: bob}}
		})
		done <- result{a, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		for _, a := range got.allocations {
			if a.OutcomeID == 3 || a.OutcomeID == 6 {
				if a.Status != "pending" || a.Reason != "multiple-originals" || a.ParentID != 0 || a.Points.Cmp(rat("1")) != 0 {
					t.Fatal("cycling choice did not fail safe", a)
				}
			} else if a.Points.Cmp(rat("5")) != 0 {
				t.Fatal("cycling choice changed an original", a)
			}
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("ResolveChain did not return from non-converging best responses within 250ms")
	}
}
