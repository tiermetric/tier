package api

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// repairRacingStore commits ONE repo repair — store's repair UPDATE shape, moving
// a token_events row from the `unqualified` sentinel to the scoped repository —
// in the middle of the report manifest's digest and exclusion reads (#1038).
//
// It fires at whichever seam the handler reaches first: after a pool read of
// ReportDigests or UnqualifiedExclusionWindow returns, in either order, or inside
// ReadSnapshot after the snapshot's first read and before the handler's own
// reads. A DEFERRED read transaction fixes its snapshot at its first read, so on
// the ReadSnapshot seam the repair is committed while the snapshot is open.
type repairRacingStore struct {
	Store
	t     *testing.T
	raw   *sql.DB
	id    int64
	slug  string
	fired int
}

func (s *repairRacingStore) repair(ctx context.Context) {
	s.t.Helper()
	if s.fired > 0 {
		return
	}
	s.fired++
	res, err := s.raw.ExecContext(ctx,
		`UPDATE token_events SET repo = ? WHERE id = ? AND repo = ?`, s.slug, s.id, repoid.Unqualified)
	if err != nil {
		s.t.Errorf("repair: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		s.t.Errorf("repair moved %d row(s), want 1", n)
	}
}

func (s *repairRacingStore) ReportDigests(ctx context.Context, since, until time.Time, scope store.RepoScope) (store.Digest, store.Digest, error) {
	events, outcomes, err := s.Store.ReportDigests(ctx, since, until, scope)
	if err == nil {
		s.repair(ctx)
	}
	return events, outcomes, err
}

func (s *repairRacingStore) UnqualifiedExclusionWindow(ctx context.Context, since, until time.Time) (store.UnqualifiedExclusion, error) {
	ex, err := s.Store.UnqualifiedExclusionWindow(ctx, since, until)
	if err == nil {
		s.repair(ctx)
	}
	return ex, err
}

func (s *repairRacingStore) ReadSnapshot(ctx context.Context, fn func(*store.Snapshot) error) error {
	return s.Store.ReadSnapshot(ctx, func(snap *store.Snapshot) error {
		if _, err := snap.UnqualifiedExclusionWindow(ctx, exclusionSinceTime, time.Time{}); err != nil {
			return err
		}
		s.repair(ctx)
		return fn(snap)
	})
}

// TestReportManifest_ScopedDigestsAndExclusionShareOneSnapshot pins that a
// scoped manifest's events_digest and repo_scope_excluded describe ONE database
// state (#1038). The fixture holds one acme/beta row and one repo-blind row, and
// a repair moves the repo-blind row into acme/beta mid-read. The only real states
// are (scoped rows, excluded rows) = (1, 1) before the repair and (2, 0) after;
// (1, 0) — the row counted in neither — is a manifest no database state matches.
func TestReportManifest_ScopedDigestsAndExclusionShareOneSnapshot(t *testing.T) {
	h, db, path := newDigestTestHandler(t)
	at := exclusionSinceTime.Add(48 * time.Hour)
	seedRepoCostAt(t, db, "acme/beta", "bob", "issue-b", 2.0, at)
	seedRepoCostAt(t, db, "", "carol", "issue-c", 9.0, at)

	raw, err := sql.Open("sqlite", rawTestDSN(path))
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	defer func() { _ = raw.Close() }()
	racing := &repairRacingStore{Store: h.store, t: t, raw: raw, slug: "acme/beta"}
	if err := raw.QueryRow(`SELECT id FROM token_events WHERE repo = ?`, repoid.Unqualified).Scan(&racing.id); err != nil {
		t.Fatalf("find the repo-blind row: %v", err)
	}
	h.store = racing

	m := decodeManifest(t, h, "/api/v1/report_manifest?since="+exclusionSince+"&repo=acme/beta")

	// Control: the repair hook ran once and its UPDATE committed. That alone does
	// not show the repair fell between the handler's two reads; the exact (1, 1)
	// assertion below does, because every wrapped read fires it after returning.
	if racing.fired != 1 {
		t.Fatalf("control: the repair fired %d times, want 1 — no interleave happened, so this proves nothing", racing.fired)
	}
	var blind int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM token_events WHERE repo = ?`, repoid.Unqualified).Scan(&blind); err != nil || blind != 0 {
		t.Fatalf("control: %d repo-blind rows remain after the repair (err %v), want 0", blind, err)
	}

	_, scoped := digestBlock(t, m, "events_digest")
	excluded, _, _ := exclusionBlock(t, m)
	// The wrapper fixes the snapshot before the repair, so one snapshot can only
	// read (1, 1). (2, 0) is a real database state but means both reads bypassed it.
	if scoped != 1 || excluded != 1 {
		t.Errorf("manifest pairs events_digest.rows = %v with repo_scope_excluded.token_events = %v, want (1, 1), "+
			"the state the snapshot fixed before the repair — the digest and the exclusion pin were not both "+
			"read in that snapshot", scoped, excluded)
	}
}
