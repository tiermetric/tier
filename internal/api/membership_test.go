package api

import (
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// TestMembershipTimeline_Boundaries pins the half-open [valid_from, valid_to)
// rule (#886) the SQL sub-window reads also follow (`ts >= lo AND ts < hi`): an
// event AT the move instant belongs to the new team, one an instant earlier to
// the old, and an event before the first row to nobody ("other").
func TestMembershipTimeline_Boundaries(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	move := time.Date(2026, 4, 15, 10, 30, 0, 123456789, time.UTC)
	tl := newMembershipTimeline([]store.MembershipRow{
		{Developer: "alice", Team: "alpha", Division: "d1", ValidFrom: t0, ValidTo: move},
		{Developer: "alice", Team: "beta", Division: "d2", ValidFrom: move},
	})

	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{t0.Add(-time.Nanosecond), ""},
		{t0, "alpha"},
		{move.Add(-time.Nanosecond), "alpha"},
		{move, "beta"},
		{move.AddDate(1, 0, 0), "beta"},
	} {
		if got := tl.labelAt("alice", c.at, teamLabel); got != c.want {
			t.Errorf("labelAt(%s) = %q, want %q", c.at.Format(time.RFC3339Nano), got, c.want)
		}
	}
	if got := tl.labelAt("alice", move, divisionLabel); got != "d2" {
		t.Errorf("division at the move = %q, want d2", got)
	}
	if got := tl.labelAt("nobody", move, teamLabel); got != "" {
		t.Errorf("unknown developer = %q, want unassigned", got)
	}

	// Cuts are strictly inside the window; the open end is unbounded.
	if got := tl.cutsIn(t0, time.Time{}); len(got) != 1 || !got[0].Equal(move) {
		t.Errorf("cutsIn(t0, open) = %v, want only the move (t0 is the window start, not inside it)", got)
	}
	if got := tl.cutsIn(t0, move); len(got) != 0 {
		t.Errorf("cutsIn(t0, move) = %v, want none (until is exclusive)", got)
	}
	if got := tl.cutsIn(t0.Add(-time.Hour), move.Add(time.Hour)); len(got) != 2 {
		t.Errorf("cutsIn around both boundaries = %v, want t0 and the move", got)
	}
}
