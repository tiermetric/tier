package webhook

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// TestPushOrder_MissingPushedAtIsCounted: for every unusable pushed_at shape,
// each commit recorded from that payload bumps the counter once; a redelivery
// (which records nothing) and a keyed push do not.
func TestPushOrder_MissingPushedAtIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pushedAt any
	}{
		{"absent", nil},
		{"null", json.RawMessage("null")},
		{"garbage string", "yesterday"},
		{"fractional number", 1757512800.5},
		{"zero", 0},
		{"negative", -5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			missing := &fakePushCounter{}
			r := &reconcileHarness{db: db, h: New(db, testSecret, quietLogger(),
				WithPushCapture(&fakePushCounter{}), WithPushMissingPushedAtCounter(missing))}

			r.pushAt(t, unixAt(14, 5), victimCommit())
			if n := missing.count(); n != 0 {
				t.Fatalf("counter after a keyed push = %d, want 0", n)
			}
			r.pushAt(t, tc.pushedAt, attackerCommit(), pushCommit("c2c2c2c2", "fix: #42 again", "mallory", at(2)))
			if n := missing.count(); n != 2 {
				t.Fatalf("counter after an unkeyed push of two commits = %d, want 2", n)
			}
			r.pushAt(t, tc.pushedAt, attackerCommit(), pushCommit("c2c2c2c2", "fix: #42 again", "mallory", at(2)))
			if n := missing.count(); n != 2 {
				t.Fatalf("counter after the redelivery = %d, want 2 (nothing was recorded)", n)
			}
			if got := pushOwnerRow(t, r); got != victimRow {
				t.Fatalf("push row = %q, want %q", got, victimRow)
			}
		})
	}
}
