package shipper

// #466 END-TO-END: the collector legitimately assigns the unattributed sentinel
// family, and the shipper forwards IssueID verbatim to POST /api/v1/events. When that
// endpoint gained a forge guard, the family had to stay writable — and nothing in the
// tree proved it, because every shipper test posts to a FAKE server that accepts
// whatever it is handed.
//
// 🔴 WHY A FAKE SERVER IS NOT ENOUGH HERE. The failure mode is not a wrong number, it
// is TOTAL, PERMANENT capture loss, and every step of it is real code:
//
//   - handlePostEvents validates all-or-nothing, so ONE exploratory event 400s the
//     whole batch;
//   - postBatch treats 4xx as terminal — no retry, and it stops the Run loop;
//   - the shipper is stateless by design, so the next cron tick re-scans the same
//     JSONL, rebuilds the identical batch, and fails identically, forever.
//
// A developer who has ever committed on main without an issue would lose 100% of their
// capture — including their well-formed, attributed events. This test therefore drives
// the shipper against the REAL api.Handler over the REAL store.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// realTierdServer starts an httptest server backed by the production api.Handler and
// a real on-disk SQLite store. Auth disabled — the gate under test is issue-id
// validation, not the bearer token.
func realTierdServer(t *testing.T) (*httptest.Server, *store.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tier-shipper-e2e.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(); _ = os.Remove(path) })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.New(db, quiet, "", nil, "test", api.RateLimitConfig{}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, db
}

// TestShipper_UnattributedFamilyRoundTrips ships one batch containing every canonical
// sentinel spelling ALONGSIDE an ordinary attributed event, against the real handler,
// and requires all of them to land.
func TestShipper_UnattributedFamilyRoundTrips(t *testing.T) {
	srv, db := realTierdServer(t)

	// Iterated from the package's own enumeration, not retyped: a bucket added to
	// collector must be covered end to end THE DAY IT LANDS, and a hardcoded copy here
	// would silently keep passing while the new bucket 400s in production.
	family := append([]string{collector.UnattributedIssueID}, collector.UnattributedBucketIDs...)
	// The attributed event is deliberately in the SAME batch: all-or-nothing
	// validation means a rejected sentinel takes this one down with it, which is the
	// half of the blast radius a family-only fixture would miss.
	want := append([]string{"issue-42"}, family...)

	c, err := New(Config{
		ServerURL:   srv.URL,
		BatchSize:   len(want) + 1, // one flush, so the batch really is mixed
		BackoffBase: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for i, issue := range want {
		e := testEvent("roundtrip-key-" + issue)
		e.IssueID = issue
		e.Timestamp = time.Now().UTC().Add(-time.Duration(i+1) * time.Minute)
		if err := c.Ingest(ctx, e); err != nil {
			t.Fatalf("Ingest(%q): %v", issue, err)
		}
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v — a 4xx here is terminal for the collector and re-fails "+
			"identically on every subsequent run (#466)", err)
	}

	// Read back through the store: what matters is that the rows EXIST with their
	// issue ids intact, not that the POST returned 2xx.
	got := map[string]bool{}
	rows, err := db.DeveloperIssueCosts(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("DeveloperIssueCosts: %v", err)
	}
	for _, r := range rows {
		got[r.IssueID] = true
	}
	for _, issue := range want {
		if !got[issue] {
			t.Errorf("issue_id %q did not reach the store; the whole batch (including "+
				"the attributed event) was rejected. Stored: %v", issue, got)
		}
	}
}

// TestShipper_ForgedSentinelIsRejectedEndToEnd is the other direction, and it is what
// makes the test above meaningful: the guard is genuinely ON, so the family passing is
// an allowlist decision rather than an absent check.
func TestShipper_ForgedSentinelIsRejectedEndToEnd(t *testing.T) {
	srv, _ := realTierdServer(t)
	c, err := New(Config{ServerURL: srv.URL, BatchSize: 100, BackoffBase: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	e := testEvent("forged-key")
	e.IssueID = "UNATTRIBUTED:main" // a case variant: never written by the collector
	if err := c.Ingest(ctx, e); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := c.Flush(ctx); err == nil {
		t.Error("a forged case-variant sentinel was accepted end to end; the /events " +
			"allowlist is not enforcing exact canonical spellings (#466)")
	}
}
