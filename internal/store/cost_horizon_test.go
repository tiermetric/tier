package store

import (
	"context"
	"testing"
	"time"
)

// TestCostCoverageStart_EmptyStoreIsNotTheEpoch pins the distinction the whole
// signal rests on: an empty store reports ok=false, NOT the zero time.
//
// This is the control arm for the failure mode #512 exists to prevent. If a
// no-data store returned the zero time, every conceivable window would appear to
// start after the horizon and the "window predates capture" flag would never fire
// — a green meaning "never ran". The flag would then be silently useless on
// exactly the install that needs it most: a brand-new one.
func TestCostCoverageStart_EmptyStoreIsNotTheEpoch(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	got, ok, err := db.CostCoverageStart(ctx, FleetWide)
	if err != nil {
		t.Fatalf("CostCoverageStart on empty store: %v", err)
	}
	if ok {
		t.Fatalf("empty store reported ok=true (horizon %s); a store with no events has no horizon", got)
	}
	if !got.IsZero() {
		t.Fatalf("empty store returned a non-zero time %s alongside ok=false", got)
	}
}

// TestCostCoverageStart_ReturnsEarliest verifies the DIRECT column read
// (ORDER BY ts LIMIT 1) round-trips through the driver as a time and really does
// return the earliest row.
//
// Deliberately NOT MIN(ts): the driver maps a column back to time.Time only when
// the column carries a DATE/DATETIME/TIMESTAMP decltype, and an aggregate has no
// decltype — so MIN(ts) scans back as a raw string and fails. TestMinTsStillFails
// below pins that constraint so this workaround cannot be "simplified" away, and
// so it gets retired if the driver ever fixes the mapping.
func TestCostCoverageStart_ReturnsEarliest(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	earliest := time.Date(2026, 6, 23, 10, 1, 1, 0, time.UTC)
	// Insert OUT of chronological order so a bug that returns "first row
	// inserted" instead of "minimum ts" cannot pass.
	for i, ts := range []time.Time{
		earliest.Add(72 * time.Hour),
		earliest,
		earliest.Add(24 * time.Hour),
	} {
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: "alice", IssueID: "issue-1", Model: "claude-opus-4-8",
			InputTok: 10, OutputTok: 5, CostMicro: 100,
			Source: "jsonl", Fidelity: "realtime", Timestamp: ts,
			IdempotencyKey: string(rune('a' + i)),
		}); err != nil {
			t.Fatalf("InsertTokenEvent %d: %v", i, err)
		}
	}

	got, ok, err := db.CostCoverageStart(ctx, FleetWide)
	if err != nil {
		t.Fatalf("CostCoverageStart: %v", err)
	}
	if !ok {
		t.Fatal("ok=false with three events inserted")
	}
	if !got.Equal(earliest) {
		t.Fatalf("horizon = %s, want %s (the MINIMUM ts, not the first inserted)", got, earliest)
	}
}

// TestSourceCoverageStart_PerSourceHorizonsDiffer is the reason the global
// horizon alone is insufficient. Two capture paths enabled a month apart give two
// horizons; a window clearing the global MIN can still predate a source entirely
// and count that source's outcomes against none of its cost.
func TestSourceCoverageStart_PerSourceHorizonsDiffer(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	jsonlStart := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	codexStart := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	ins := func(src string, ts time.Time, key string) {
		t.Helper()
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: "alice", IssueID: "issue-1", Model: "m",
			InputTok: 1, OutputTok: 1, CostMicro: 1,
			Source: src, Fidelity: "realtime", Timestamp: ts, IdempotencyKey: key,
		}); err != nil {
			t.Fatalf("insert %s: %v", src, err)
		}
	}
	ins("jsonl", jsonlStart, "j1")
	ins("jsonl", jsonlStart.Add(48*time.Hour), "j2")
	ins("codex-rollout", codexStart, "c1")

	got, err := db.SourceCoverageStart(ctx, FleetWide)
	if err != nil {
		t.Fatalf("SourceCoverageStart: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sources, want 2: %v", len(got), got)
	}
	if !got["jsonl"].Equal(jsonlStart) {
		t.Errorf("jsonl horizon = %s, want %s", got["jsonl"], jsonlStart)
	}
	if !got["codex-rollout"].Equal(codexStart) {
		t.Errorf("codex-rollout horizon = %s, want %s", got["codex-rollout"], codexStart)
	}
	// The global horizon is the looser bound — assert it does NOT stand in for
	// the per-source one, which is the whole point of emitting both.
	global, ok, err := db.CostCoverageStart(ctx, FleetWide)
	if err != nil || !ok {
		t.Fatalf("CostCoverageStart: %v ok=%v", err, ok)
	}
	if global.Equal(got["codex-rollout"]) {
		t.Fatal("global horizon equals the codex horizon; a window clearing the global MIN would look fully covered for a source that starts a month later")
	}
}

// TestMinTsStillFails pins the driver constraint that CostCoverageStart and
// SourceCoverageStart are built around.
//
// Both deliberately use `ORDER BY ts LIMIT 1` instead of the obvious `MIN(ts)`,
// and store.go carries a "do not simplify it back" comment saying why. A comment
// is not a guard: nothing stopped a future edit from swapping in MIN(ts) and
// discovering the breakage in production instead of here.
//
// This test is two-way. It fails TODAY if someone drops the workaround while the
// driver still cannot map an aggregate back to time.Time. It also fails LATER if
// modernc.org/sqlite starts handling it — at which point the workaround and its
// comments are obsolete and should be retired rather than left to rot as folklore.
func TestMinTsStillFails(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "alice", IssueID: "issue-1", Model: "m",
		InputTok: 1, OutputTok: 1, CostMicro: 1,
		Source: "jsonl", Fidelity: "realtime",
		Timestamp: time.Date(2026, 6, 23, 10, 1, 1, 0, time.UTC), IdempotencyKey: "m1",
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}

	var got time.Time
	err := db.db.QueryRowContext(ctx, `SELECT MIN(ts) FROM token_events`).Scan(&got)
	if err == nil {
		t.Fatalf("MIN(ts) now scans into time.Time (got %s) — the ORDER BY ts LIMIT 1 workaround in "+
			"CostCoverageStart/SourceCoverageStart is obsolete. Retire it AND the comments that explain it, "+
			"rather than leaving a justification that is no longer true.", got)
	}
}
