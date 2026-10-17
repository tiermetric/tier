// Package event3 contains the dormant correction projection core. It has no
// reporting-window, cost, lifecycle-command or database dependencies.
package event3

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

const (
	ProvisionalRule = "event3-v1"
	ReviewedRule    = "event3-v2"
	MaxRatBytes     = 4096
)

// EncodeRat persists lowest-term numerator/denominator even for integers. All
// arithmetic stays rational until a reader explicitly converts its final value.
func EncodeRat(r *big.Rat) (string, error) {
	if r == nil || r.Sign() < 0 {
		return "", errors.New("nonnegative rational required")
	}
	s := r.Num().String() + "/" + r.Denom().String()
	if len(s) > MaxRatBytes {
		return "", errors.New("rational exceeds storage bound")
	}
	return s, nil
}
func DecodeRat(s string) (*big.Rat, error) {
	if len(s) > MaxRatBytes {
		return nil, errors.New("rational exceeds storage bound")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, errors.New("invalid rational")
	}
	canonical, err := EncodeRat(r)
	if err != nil || canonical != s {
		return nil, errors.New("noncanonical rational")
	}
	return r, nil
}
func r(n, d int64) *big.Rat { return big.NewRat(n, d) }
func copyRat(x *big.Rat) *big.Rat {
	if x == nil {
		return new(big.Rat)
	}
	return new(big.Rat).Set(x)
}
func sub(a, b *big.Rat) *big.Rat { return new(big.Rat).Sub(a, b) }
func mul(a, b *big.Rat) *big.Rat { return new(big.Rat).Mul(a, b) }
func min(a, b *big.Rat) *big.Rat {
	if a.Cmp(b) < 0 {
		return copyRat(a)
	}
	return copyRat(b)
}
func max(a, b *big.Rat) *big.Rat {
	if a.Cmp(b) > 0 {
		return copyRat(a)
	}
	return copyRat(b)
}
func onScale(x *big.Rat) bool {
	for _, s := range []*big.Rat{r(1, 2), r(1, 1), r(3, 1), r(5, 1), r(8, 1)} {
		if x != nil && x.Cmp(s) == 0 {
			return true
		}
	}
	return false
}
func qualityOnScale(x *big.Rat) bool {
	for _, s := range []*big.Rat{r(1, 1), r(17, 20), r(7, 10), r(11, 20), r(4, 5), r(1, 10)} {
		if x != nil && x.Cmp(s) == 0 {
			return true
		}
	}
	return false
}

// Review is sizing evidence, not a confirmation. U2 supplies audited lifecycle
// transitions. A raw size label cannot stand in for this reviewed scope.
type Review struct {
	Original, Own, Before, After                      *big.Rat
	PureRepair                                        bool
	Scope, Rationale, Reviewer                        string
	ReviewedAt                                        time.Time
	Revision                                          int64
	ComparisonBase, ComparisonBefore, ComparisonAfter string
}

func (v Review) Validate() error {
	for _, x := range []*big.Rat{v.Original, v.Own} {
		if !onScale(x) {
			return errors.New("reviewed original and own sizes must be on the 0.5/1/3/5/8 scale")
		}
	}
	if !v.PureRepair || v.Before != nil || v.After != nil {
		if !onScale(v.Before) || !onScale(v.After) {
			return errors.New("reviewed before/after cumulative sizes required unless explicitly pure repair")
		}
		if v.PureRepair && v.Before.Cmp(v.After) != 0 {
			return errors.New("pure repair requires identical before/after scope")
		}
	}
	if strings.TrimSpace(v.Scope) == "" || strings.TrimSpace(v.Rationale) == "" || strings.TrimSpace(v.Reviewer) == "" || v.ReviewedAt.IsZero() || v.Revision <= 0 {
		return errors.New("review scope, rationale, reviewer, time and revision required")
	}
	for _, s := range []string{v.Scope, v.Rationale} {
		if len(s) > 4096 {
			return errors.New("review text exceeds 4096 bytes")
		}
	}
	for _, s := range []string{v.Reviewer, v.ComparisonBase, v.ComparisonBefore, v.ComparisonAfter} {
		if len(s) > 128 {
			return errors.New("review identity exceeds 128 bytes")
		}
	}
	return nil
}

// Effort is recorded churn plus summed file counts, NOT a file-union estimate.
// big.Int accumulation avoids overflow before the provisional size is bucketed.
type Effort struct{ Additions, Deletions, Files int64 }

func (e *Effort) value() (*big.Int, bool) {
	if e == nil || e.Additions < 0 || e.Deletions < 0 || e.Files < 0 {
		return nil, false
	}
	v := new(big.Int).Add(big.NewInt(e.Additions), big.NewInt(e.Deletions))
	return v.Add(v, new(big.Int).Mul(big.NewInt(e.Files), big.NewInt(10))), true
}
func effortWeight(v *big.Int) *big.Rat {
	for _, b := range []struct {
		limit  int64
		weight *big.Rat
	}{{15, r(1, 2)}, {60, r(1, 1)}, {200, r(3, 1)}, {1000, r(5, 1)}} {
		if v.Cmp(big.NewInt(b.limit)) <= 0 {
			return b.weight
		}
	}
	return r(8, 1)
}

type Parent struct {
	ID     int64
	State  string
	Review *Review
}
type Node struct {
	ID              int64
	Repo            string
	PR              int
	SHA, Developer  string
	MergedAt        time.Time
	Weight, Quality *big.Rat // legacy values, never overwritten
	Effort          *Effort
	Parents         []Parent
}

func (n Node) reviewFor(parentID int64) *Review {
	for _, p := range n.Parents {
		if p.ID == parentID && active(p.State) {
			return p.Review
		}
	}
	return nil
}

type Allocation struct {
	ParentID                                                                    int64
	SourceWeight, SourceQuality                                                 *big.Rat
	OutcomeID                                                                   int64
	Role, Status, Reason, Eligible, RuleVersion                                 string
	Provisional, Disputed                                                       bool
	RawWeight, Own, Before, After, Credit, Share, Quality, OriginalTerm, Points *big.Rat
}

func less(a, b Node) bool {
	if !a.MergedAt.Equal(b.MergedAt) {
		return a.MergedAt.Before(b.MergedAt)
	}
	if a.Repo != b.Repo {
		return a.Repo < b.Repo
	}
	if a.PR != b.PR {
		return a.PR < b.PR
	}
	return a.SHA < b.SHA
}
func retained(state string) bool {
	return state == "confirmed" || state == "disputed" || state == "withdrawn-confirmed"
}
func active(state string) bool {
	return state != "removed" && state != "unrelated" && state != "withdrawn"
}

// ResolveChain consumes complete connected components, before window filtering.
// IDs identify links only. A shared corrective outcome is allocated exactly once.
// Missing/unsupported evidence retains full legacy own values with an explicit
// pending reason. Computed fractions are never rejected for leaving the size grid.
func ResolveChain(input []Node) ([]Allocation, error) {
	return resolveChain(input, allocate)
}

// candidateAllocation is private so tests can force a best-response cycle.
func resolveChain(input []Node, candidateAllocation func([]Node, map[int64]Node, map[int64]int64, map[int64]string, map[int64]bool, map[int64]bool) []Allocation) ([]Allocation, error) {
	ns := append([]Node(nil), input...)
	sort.Slice(ns, func(i, j int) bool { return less(ns[i], ns[j]) })
	by := map[int64]Node{}
	identity := map[string]bool{}
	for _, n := range ns {
		key := fmt.Sprintf("%s#%d@%s", n.Repo, n.PR, n.SHA)
		if n.ID <= 0 || by[n.ID].ID != 0 || identity[key] {
			return nil, errors.New("duplicate or invalid chain identity")
		}
		for _, x := range []*big.Rat{n.Weight, n.Quality} {
			if _, err := EncodeRat(x); err != nil {
				return nil, err
			}
		}
		for _, p := range n.Parents {
			if p.Review != nil {
				if err := p.Review.Validate(); err != nil {
					return nil, err
				}
			}
		}
		by[n.ID] = n
		identity[key] = true
	}
	roots := map[int64]map[int64]bool{}
	bad := map[int64]string{}
	visiting := map[int64]bool{}
	var ancestors func(int64) map[int64]bool
	ancestors = func(id int64) map[int64]bool {
		if v, ok := roots[id]; ok {
			return v
		}
		if visiting[id] {
			bad[id] = "cycle"
			return nil
		}
		n, ok := by[id]
		if !ok {
			return nil
		}
		visiting[id] = true
		v := map[int64]bool{}
		has := false
		for _, p := range n.Parents {
			if !active(p.State) {
				continue
			}
			switch p.State {
			case "pending", "excluded", "declared", "confirmed", "disputed", "disputed-declared", "withdrawn-confirmed":
			default:
				bad[id] = "unsupported-relation-state"
			}
			has = true
			pn, exists := by[p.ID]
			if !exists || p.State == "pending" || p.State == "excluded" {
				bad[id] = "missing-or-excluded-parent"
				continue
			}
			if n.MergedAt.IsZero() || pn.MergedAt.IsZero() {
				bad[id] = "missing-occurrence"
			}
			if pn.MergedAt.After(n.MergedAt) {
				bad[id] = "parent-after-child"
			}
			for root := range ancestors(p.ID) {
				v[root] = true
			}
			if bad[p.ID] != "" {
				bad[id] = bad[p.ID]
			}
		}
		if !has {
			v[id] = true
		}
		visiting[id] = false
		roots[id] = v
		return v
	}
	for _, n := range ns {
		ancestors(n.ID)
	}
	// Deduplicate comparable named parents to their deepest declared parent.
	// Incomparable repair-parent shares need a reviewed split that U1 cannot invent.
	var reaches func(int64, int64, map[int64]bool) bool
	reaches = func(id, target int64, seen map[int64]bool) bool {
		if id == target {
			return true
		}
		if seen[id] {
			return false
		}
		seen[id] = true
		for _, p := range by[id].Parents {
			if active(p.State) && reaches(p.ID, target, seen) {
				return true
			}
		}
		return false
	}
	selected := map[int64]int64{}
	multi := map[int64][]int64{}
	provisional := map[int64]bool{}
	disputed := map[int64]bool{}
	for _, n := range ns {
		// Dispute disclosure survives pending sizing/occurrence evidence too.
		for _, p := range n.Parents {
			if active(p.State) {
				disputed[n.ID] = disputed[n.ID] || p.State == "disputed" || p.State == "disputed-declared" || p.State == "withdrawn-confirmed"
			}
		}
		if bad[n.ID] != "" {
			continue
		}
		var ps []int64
		confirmed := true
		for _, p := range n.Parents {
			if active(p.State) {
				ps = append(ps, p.ID)
				confirmed = confirmed && retained(p.State)
			}
		}
		if len(ps) == 0 {
			continue
		}
		if !onScale(n.Weight) || !qualityOnScale(n.Quality) {
			bad[n.ID] = "unsupported-allocation-shape"
			continue
		}
		if n.MergedAt.IsZero() {
			bad[n.ID] = "missing-occurrence"
			continue
		}
		if len(roots[n.ID]) > 1 {
			for root := range roots[n.ID] {
				if by[root].Developer == n.Developer {
					multi[n.ID] = append(multi[n.ID], root)
				}
			}
			sort.Slice(multi[n.ID], func(i, j int) bool { return less(by[multi[n.ID][i]], by[multi[n.ID][j]]) })
			bad[n.ID] = "multiple-originals"
			provisional[n.ID] = true
			continue
		}
		chosen := int64(0)
		for _, p := range ps {
			all := true
			for _, other := range ps {
				all = all && reaches(p, other, map[int64]bool{})
			}
			if all {
				chosen = p
				break
			}
		}
		if chosen == 0 {
			// Incomparable repair parents require an unruled split of their shares.
			// Preserve own values rather than assigning their repair to an ancestor.
			bad[n.ID] = "unsupported-allocation-shape"
			continue
		}
		selected[n.ID] = chosen
		provisional[n.ID] = !confirmed || n.reviewFor(chosen) == nil
	}
	// Later follow-ups can change the least-favourable choice of earlier ones.
	// Revisit candidates against the complete own allocation until stable.
	changed := true
	seenSelections := map[string]bool{}
	for changed {
		var vector []int64
		for _, n := range ns {
			if _, ok := multi[n.ID]; ok {
				vector = append(vector, selected[n.ID])
			}
		}
		key := fmt.Sprint(vector)
		if seenSelections[key] {
			// A repeated best-response state cannot converge. Keep all multi-root
			// work pending and discard its tentative effects on the originals.
			for id := range multi {
				delete(selected, id)
			}
			return allocate(ns, by, selected, bad, provisional, disputed), nil
		}
		seenSelections[key] = true
		changed = false
		for _, n := range ns {
			candidates, ok := multi[n.ID]
			if !ok {
				continue
			}
			previous := selected[n.ID]
			var best *big.Rat
			var winner int64
			for _, root := range candidates {
				selected[n.ID] = root
				a := candidateAllocation(ns, by, selected, bad, provisional, disputed)
				total := new(big.Rat)
				for _, v := range a {
					if by[v.OutcomeID].Developer == n.Developer {
						total.Add(total, v.Points)
					}
				}
				if best == nil || total.Cmp(best) < 0 {
					best, winner = total, root
				}
			}
			if winner != 0 {
				selected[n.ID] = winner
			} else {
				delete(selected, n.ID)
			}
			changed = changed || previous != winner
		}
	}
	return allocate(ns, by, selected, bad, provisional, disputed), nil
}

func allocate(ns []Node, by map[int64]Node, selected map[int64]int64, bad map[int64]string, provisional, disputed map[int64]bool) []Allocation {
	a := map[int64]Allocation{}
	children := map[int64][]int64{}
	enabled := map[int64]bool{}
	for _, n := range ns {
		v := Allocation{OutcomeID: n.ID, ParentID: selected[n.ID], SourceWeight: copyRat(n.Weight), SourceQuality: copyRat(n.Quality), Role: "original", Status: "resolved", Eligible: "eligible", RuleVersion: ProvisionalRule, RawWeight: copyRat(n.Weight), Own: copyRat(n.Weight), Before: new(big.Rat), After: new(big.Rat), Credit: new(big.Rat), Share: copyRat(n.Weight), Quality: copyRat(n.Quality), OriginalTerm: mul(n.Weight, n.Quality), Points: mul(n.Weight, n.Quality), Disputed: disputed[n.ID]}
		has := false
		for _, p := range n.Parents {
			has = has || active(p.State)
		}
		if has {
			v.Role = "follow-up"
			v.Provisional = provisional[n.ID]
			v.Eligible = "unknown"
		}
		if bad[n.ID] != "" {
			v.Status = "pending"
			v.Reason = bad[n.ID]
		}
		a[n.ID] = v
		if parent := selected[n.ID]; parent != 0 {
			children[parent] = append(children[parent], n.ID)
		}
	}
	// Provisional effort includes each earlier participant in the selected root
	// exactly once, plus all ancestors even at equal occurrence timestamps.
	rootOf := func(id int64) int64 {
		seen := map[int64]bool{}
		for selected[id] != 0 && !seen[id] {
			seen[id] = true
			id = selected[id]
		}
		return id
	}
	sizing := func(id, parent int64) (*big.Rat, *big.Rat, bool) {
		total := new(big.Int)
		ok := true
		seen := map[int64]bool{}
		var include func(int64)
		include = func(candidate int64) {
			path := map[int64]bool{}
			for p := candidate; p != 0 && !path[p]; p = selected[p] {
				if p == id {
					return
				}
				path[p] = true
			}
			if seen[candidate] {
				return
			}
			seen[candidate] = true
			e, valid := by[candidate].Effort.value()
			ok = ok && valid
			if valid {
				total.Add(total, e)
			}
			if p := selected[candidate]; p != 0 {
				include(p)
			}
		}
		include(parent)
		for _, n := range ns {
			if n.ID != id && less(n, by[id]) && rootOf(n.ID) == rootOf(parent) {
				include(n.ID)
			}
		}
		before := effortWeight(total)
		e, valid := by[id].Effort.value()
		ok = ok && valid
		if valid {
			total.Add(total, e)
		}
		return before, effortWeight(total), ok
	}
	var dependencyOrder []Node
	visited := map[int64]bool{}
	var visit func(int64)
	visit = func(id int64) {
		if visited[id] {
			return
		}
		visited[id] = true
		if parent := selected[id]; parent != 0 {
			visit(parent)
		}
		dependencyOrder = append(dependencyOrder, by[id])
	}
	for _, n := range ns {
		visit(n.ID)
	}
	for _, parent := range dependencyOrder {
		ids := children[parent.ID]
		if len(ids) == 0 {
			continue
		}
		evidence := bad[parent.ID] == "" && onScale(parent.Weight) && qualityOnScale(parent.Quality) && !parent.MergedAt.IsZero()
		for _, id := range ids {
			n := by[id]
			v := a[id]
			v.Provisional = provisional[id]
			own := copyRat(n.Weight)
			before, after, statsOK := sizing(id, parent.ID)
			valid := evidence && a[parent.ID].Status != "pending"
			review := n.reviewFor(parent.ID)
			if review != nil {
				own = copyRat(review.Own)
				before = copyRat(review.Before)
				after = copyRat(review.After)
				v.RuleVersion = ReviewedRule
			} else {
				valid = valid && statsOK
			}
			if !valid {
				v.Status = "pending"
				v.Reason = "missing-sizing-or-parent-evidence"
				a[id] = v
				continue
			}
			v.Own = own
			v.Before = before
			v.After = after
			v.Credit = min(own, max(new(big.Rat), sub(after, before)))
			if review != nil && review.PureRepair {
				v.Credit = new(big.Rat)
			}
			// Multi-original sizing supports the least-favourable allocation,
			// but ranking growth stays unknown until the originals are resolved.
			if v.Reason != "multiple-originals" {
				v.Eligible = "ineligible"
				if v.Credit.Sign() > 0 {
					v.Eligible = "eligible"
				}
			}
			enabled[id] = !v.Provisional || n.Developer == parent.Developer
			if v.Provisional && v.Status != "pending" {
				v.Status = "provisional"
			}
			// A cross-author declaration computes growth but moves neither contribution.
			a[id] = v
		}
	}
	// Corrections change the parent's own quality once, in the inclusive 14-day
	// occurrence window. A sub-chain then uses the incoming share as its W_O.
	for _, n := range ns {
		v := a[n.ID]
		for _, id := range children[n.ID] {
			delta := by[id].MergedAt.Sub(n.MergedAt)
			if enabled[id] {
				if a[id].RuleVersion == ReviewedRule {
					v.RuleVersion = ReviewedRule
				}
				v.Provisional = v.Provisional || a[id].Provisional
			}
			if enabled[id] && delta >= 0 && delta <= 14*24*time.Hour {
				switch {
				case v.Quality.Cmp(r(1, 1)) == 0:
					v.Quality = r(17, 20)
				case v.Quality.Cmp(r(7, 10)) == 0:
					v.Quality = r(11, 20)
				}
			}
		}
		a[n.ID] = v
	}
	for _, n := range ns {
		v := a[n.ID]
		if parent := selected[n.ID]; parent != 0 {
			v.SourceWeight = copyRat(by[parent].Weight)
			v.SourceQuality = copyRat(a[parent].Quality)
		} else {
			v.SourceQuality = copyRat(v.Quality)
		}
		a[n.ID] = v
	}
	done := map[int64]bool{}
	var distribute func(int64, *big.Rat)
	distribute = func(id int64, share *big.Rat) {
		if done[id] {
			return
		}
		done[id] = true
		v := a[id]
		v.Share = copyRat(share)
		v.OriginalTerm = mul(share, v.Quality)
		v.Points = copyRat(v.OriginalTerm)
		budget := copyRat(v.OriginalTerm)
		sumOwn := new(big.Rat)
		for _, child := range children[id] {
			if enabled[child] && share.Sign() > 0 {
				budget.Add(budget, a[child].Credit)
				sumOwn.Add(sumOwn, a[child].Own)
			}
		}
		floor := mul(r(1, 10), share)
		v.Points = max(floor, sub(budget, sumOwn))
		if sumOwn.Sign() == 0 {
			v.Points = copyRat(v.OriginalTerm)
		}
		remaining := max(new(big.Rat), sub(budget, v.Points))
		a[id] = v
		// Pending multi-root work cannot take a confirmed sibling's allocation.
		queue := append([]int64(nil), children[id]...)
		sort.SliceStable(queue, func(i, j int) bool {
			return a[queue[i]].Reason != "multiple-originals" && a[queue[j]].Reason == "multiple-originals"
		})
		for _, child := range queue {
			if enabled[child] {
				source := a[child]
				source.SourceWeight = copyRat(share)
				source.SourceQuality = copyRat(v.Quality)
				a[child] = source
				childShare := min(a[child].Own, remaining)
				remaining.Sub(remaining, childShare)
				distribute(child, childShare)
			} else {
				distribute(child, by[child].Weight)
			}
		}
	}
	for _, n := range ns {
		if selected[n.ID] == 0 || !enabled[n.ID] {
			weight := n.Weight
			if selected[n.ID] == 0 {
				for _, child := range children[n.ID] {
					if review := by[child].reviewFor(n.ID); enabled[child] && review != nil {
						weight = review.Original
						break
					}
				}
				v := a[n.ID]
				v.SourceWeight = copyRat(weight)
				a[n.ID] = v
			}
			distribute(n.ID, weight)
		}
	}
	out := make([]Allocation, 0, len(ns))
	for _, n := range ns {
		out = append(out, a[n.ID])
	}
	return out
}
