package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// auditRowCount returns the number of rows in cost_correction_audit —
// the control-arm assertion every "this must NOT correct anything" test
// needs: Corrected=false alone doesn't prove no audit row was written, only
// that CorrectManualCostEvent SAID so.
func auditRowCount(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM cost_correction_audit`).Scan(&n); err != nil {
		t.Fatalf("count cost_correction_audit: %v", err)
	}
	return n
}

// auditRow reads back the single expected cost_correction_audit row's core
// fields, failing if there isn't exactly one.
func auditRow(t *testing.T, db *DB) (tokenEventID, oldMicro, newMicro int64, actor, reason string) {
	t.Helper()
	rows, err := db.db.Query(`SELECT token_event_id, old_cost_micro, new_cost_micro, actor, reason FROM cost_correction_audit`)
	if err != nil {
		t.Fatalf("query cost_correction_audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("expected 1 cost_correction_audit row, got 0")
	}
	if err := rows.Scan(&tokenEventID, &oldMicro, &newMicro, &actor, &reason); err != nil {
		t.Fatalf("scan cost_correction_audit row: %v", err)
	}
	if rows.Next() {
		t.Fatal("expected exactly 1 cost_correction_audit row, got more than 1")
	}
	return
}

// TestCorrectManualCostEvent_DivergentCostAppliesOverride is the #346 core
// contract: unlike a bare InsertManualCostEvent re-post (which 409s and
// leaves the row untouched — see TestInsertManualCostEvent_DivergentCostConflicts
// in manual_cost_conflict_test.go), a CorrectManualCostEvent override on the
// SAME divergent re-post actually LANDS the new cost.
func TestCorrectManualCostEvent_DivergentCostAppliesOverride(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 10_500)); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Control arm FIRST: the SAME divergent re-post without an override must
	// still 409 — proving the override is what changes the outcome, not some
	// other side effect of this test's setup.
	if err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 20_600)); !errors.Is(err, ErrCostConflict) {
		t.Fatalf("un-overridden divergent re-post err = %v, want ErrCostConflict", err)
	}
	if got := onlyCostMicro(t, db); got != 10_500 {
		t.Fatalf("stored cost_micro after the 409 = %d, want 10_500 unchanged", got)
	}

	res, err := db.CorrectManualCostEvent(ctx, manualCostEvent("k1", 20_600), "finance-alice", "Q3 invoice correction")
	if err != nil {
		t.Fatalf("CorrectManualCostEvent: %v", err)
	}
	if !res.Corrected {
		t.Fatal("Corrected = false, want true for a genuinely divergent override")
	}
	if res.OldCostMicro != 10_500 || res.NewCostMicro != 20_600 {
		t.Errorf("result = %+v, want old=10500 new=20600", res)
	}
	if got := onlyCostMicro(t, db); got != 20_600 {
		t.Errorf("stored cost_micro = %d, want 20_600 (the override must actually land)", got)
	}
}

// TestCorrectManualCostEvent_WritesAuditRow pins the append-only trail
// (#346's explicit design constraint): a genuine correction must record
// old→new, actor, and reason, keyed to the corrected row.
func TestCorrectManualCostEvent_WritesAuditRow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 10_500)); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	res, err := db.CorrectManualCostEvent(ctx, manualCostEvent("k1", 20_600), "finance-alice", "Q3 invoice correction")
	if err != nil {
		t.Fatalf("CorrectManualCostEvent: %v", err)
	}

	if got := auditRowCount(t, db); got != 1 {
		t.Fatalf("cost_correction_audit row count = %d, want 1", got)
	}
	tokenEventID, oldMicro, newMicro, actor, reason := auditRow(t, db)
	if tokenEventID != res.TokenEventID {
		t.Errorf("audit token_event_id = %d, want %d (the result's own id)", tokenEventID, res.TokenEventID)
	}
	if oldMicro != 10_500 || newMicro != 20_600 {
		t.Errorf("audit old/new = %d/%d, want 10500/20600", oldMicro, newMicro)
	}
	if actor != "finance-alice" || reason != "Q3 invoice correction" {
		t.Errorf("audit actor/reason = %q/%q, want finance-alice/Q3 invoice correction", actor, reason)
	}
}

// TestCorrectManualCostEvent_NoExistingRowInsertsWithoutAudit: overriding a
// BRAND NEW key is not a correction — there is nothing to correct — so it
// must behave exactly like a normal insert and must NOT write an audit row.
func TestCorrectManualCostEvent_NoExistingRowInsertsWithoutAudit(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	res, err := db.CorrectManualCostEvent(ctx, manualCostEvent("fresh", 10_500), "finance-alice", "seeding a new figure")
	if err != nil {
		t.Fatalf("CorrectManualCostEvent: %v", err)
	}
	if res.Corrected {
		t.Error("Corrected = true for a brand-new key, want false (nothing existed to correct)")
	}
	if got := onlyCostMicro(t, db); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 (the insert must still land)", got)
	}
	if got := auditRowCount(t, db); got != 0 {
		t.Errorf("cost_correction_audit row count = %d, want 0 (a fresh insert is not a correction)", got)
	}
}

// TestCorrectManualCostEvent_MatchingCostIsNoOpWithoutAudit: an override flag
// on a re-post that turns out NOT to diverge is not an error and not a
// correction — it is the ordinary idempotent path, and must not spuriously
// grow the audit ledger.
func TestCorrectManualCostEvent_MatchingCostIsNoOpWithoutAudit(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 10_500)); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	res, err := db.CorrectManualCostEvent(ctx, manualCostEvent("k1", 10_500), "finance-alice", "no-op re-post")
	if err != nil {
		t.Fatalf("CorrectManualCostEvent: %v", err)
	}
	if res.Corrected {
		t.Error("Corrected = true for a matching-cost re-post, want false")
	}
	if got := onlyCostMicro(t, db); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 unchanged", got)
	}
	if got := auditRowCount(t, db); got != 0 {
		t.Errorf("cost_correction_audit row count = %d, want 0 (nothing diverged)", got)
	}
}

// TestCorrectManualCostEvent_OnlyCostMicroChanges is #346's constraint that
// this must NOT become a last-writer-wins upsert: a correction whose request
// carries DIFFERENT token counts than the stored row must leave those counts
// untouched — only cost_micro moves.
func TestCorrectManualCostEvent_OnlyCostMicroChanges(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	original := manualCostEvent("k1", 10_500)
	original.InputTok = 1000
	original.OutputTok = 500
	if err := db.InsertManualCostEvent(ctx, original); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Read back what was ACTUALLY stored after the first insert (not the
	// input struct's zero-value PriceVersion sentinel) — InsertTokenEvent
	// stamps the active table version at write time, so comparing against
	// original.PriceVersion (always 0, unset by manualCostEvent) would
	// compare against a value that was never stored in the first place.
	preEvents, _, err := db.ListTokenEvents(ctx, original.Timestamp.Add(-1), original.Timestamp.Add(1), PageCursor{}, 10)
	if err != nil {
		t.Fatalf("ListTokenEvents (pre-correction): %v", err)
	}
	if len(preEvents) != 1 {
		t.Fatalf("expected 1 row before the correction, got %d", len(preEvents))
	}
	wantPriceVersion := preEvents[0].PriceVersion
	wantBillingMode := preEvents[0].BillingMode

	// Token counts and session_id are NOT part of the identity check (unlike
	// developer/issue_id/model/source/fidelity — see
	// TestCorrectManualCostEvent_IdentityMismatchRefused), so a correction
	// request carrying DIFFERENT values for them must still succeed while
	// leaving the stored columns untouched — proving the UPDATE really is
	// single-column, not "every non-identity field wins."
	//
	// Fidelity is deliberately NOT the vehicle for that proof any more: it is
	// client-asserted on /costs and therefore part of the identity tuple, so a
	// divergent fidelity is now a refusal, not a silently-ignored field. That
	// case is asserted in TestCorrectManualCostEvent_IdentityMismatchRefused.
	corrected := manualCostEvent("k1", 20_600)
	corrected.InputTok = 999_999 // deliberately different — must NOT land
	corrected.OutputTok = 999_999
	corrected.SessionID = "session-that-must-not-land"
	res, err := db.CorrectManualCostEvent(ctx, corrected, "finance-alice", "cost-only correction")
	if err != nil {
		t.Fatalf("CorrectManualCostEvent: %v", err)
	}
	if !res.Corrected {
		t.Fatal("Corrected = false, want true")
	}

	events, _, err := db.ListTokenEvents(ctx, original.Timestamp.Add(-1), original.Timestamp.Add(1), PageCursor{}, 10)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 row, got %d", len(events))
	}
	got := events[0]
	if got.CostMicro != 20_600 {
		t.Errorf("cost_micro = %d, want 20_600 (the correction)", got.CostMicro)
	}
	// The full "surgical one-column correction" claim from the doc comment:
	// InputTok/OutputTok/SessionID are NOT identity-gated (unlike Model/Source/
	// Fidelity, checked separately) yet must still be provably untouched by the
	// UPDATE itself; Model/Source/Fidelity/PriceVersion/BillingMode are
	// identity-gated or INSERT-only and are asserted here too so a future change
	// to the UPDATE statement's column list cannot silently widen without a test
	// noticing.
	if got.InputTok != 1000 || got.OutputTok != 500 {
		t.Errorf("token counts = input=%d output=%d, want input=1000 output=500 UNCHANGED", got.InputTok, got.OutputTok)
	}
	if got.SessionID != "" {
		t.Errorf("session_id = %q, want \"\" UNCHANGED (request claimed %q)", got.SessionID, corrected.SessionID)
	}
	if got.Fidelity != "estimated" {
		t.Errorf("fidelity = %q, want \"estimated\" UNCHANGED", got.Fidelity)
	}
	if got.Model != "claude-sonnet-4" {
		t.Errorf("model = %q, want \"claude-sonnet-4\" UNCHANGED", got.Model)
	}
	if got.Source != "api" {
		t.Errorf("source = %q, want \"api\" UNCHANGED", got.Source)
	}
	if got.PriceVersion != wantPriceVersion {
		t.Errorf("price_version = %d, want %d (the value stamped at first insert) UNCHANGED", got.PriceVersion, wantPriceVersion)
	}
	if got.BillingMode != wantBillingMode {
		t.Errorf("billing_mode = %q, want %q (the value stamped at first insert) UNCHANGED", got.BillingMode, wantBillingMode)
	}
}

// TestCorrectManualCostEvent_IdentityMismatchRefused is the #346 R1 fix: an
// idempotency_key is GLOBALLY unique with no other column in its partial
// index, so a key collision across (developer, issue_id, model, source) is
// real — a request claiming a different identity than the row that actually
// owns the key must be refused, never silently correct the OTHER identity's
// money. Both directions are asserted: a mismatched request changes NOTHING,
// and the identical request with the CORRECT identity succeeds — proving the
// refusal is about identity, not some other accidental rejection.
func TestCorrectManualCostEvent_IdentityMismatchRefused(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	owner := manualCostEvent("shared-key-1", 10_500) // developer=alice, issue=issue-42, model=claude-sonnet-4, source=api
	if err := db.InsertManualCostEvent(ctx, owner); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(e *TokenEvent)
	}{
		{"different developer", func(e *TokenEvent) { e.Developer = "mallory" }},
		{"different issue_id", func(e *TokenEvent) { e.IssueID = "issue-999" }},
		{"different model", func(e *TokenEvent) { e.Model = "totally-different-model" }},
		{"different source", func(e *TokenEvent) { e.Source = "jsonl" }},
		// fidelity is claimed by the client on /costs exactly as source is (the
		// handler validates it as a "daily"/"estimated" enum), so it belongs in
		// the identity tuple for the same reason. Before it was included, a
		// request stating the correct developer/issue/model but the WRONG
		// fidelity was accepted and corrected the row — the doc comment above
		// the check claimed it compared "every column the client's request
		// itself claims", and that claim was false.
		{"different fidelity", func(e *TokenEvent) { e.Fidelity = "daily" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempt := manualCostEvent("shared-key-1", 20_600)
			tc.mutate(&attempt)
			_, err := db.CorrectManualCostEvent(ctx, attempt, "mallory-as-finance", "trying to correct a row I don't own")
			if !errors.Is(err, ErrCostCorrectionIdentityMismatch) {
				t.Fatalf("err = %v, want ErrCostCorrectionIdentityMismatch", err)
			}
			if got := onlyCostMicro(t, db); got != 10_500 {
				t.Errorf("stored cost_micro = %d, want 10_500 unchanged (an identity-mismatched request must not mutate)", got)
			}
			if got := auditRowCount(t, db); got != 0 {
				t.Errorf("cost_correction_audit row count = %d, want 0", got)
			}
		})
	}

	// Positive control: the SAME divergent cost, correct identity, succeeds —
	// proves the refusal above is specifically about identity, not e.g. a
	// bug that rejects every override on this key.
	correct := manualCostEvent("shared-key-1", 20_600)
	res, err := db.CorrectManualCostEvent(ctx, correct, "finance-alice", "legitimate correction")
	if err != nil {
		t.Fatalf("correct-identity CorrectManualCostEvent: %v", err)
	}
	if !res.Corrected {
		t.Fatal("Corrected = false for a genuinely divergent, correctly-identified request")
	}
	if got := onlyCostMicro(t, db); got != 20_600 {
		t.Errorf("stored cost_micro = %d, want 20_600", got)
	}
}

// TestCorrectManualCostEvent_ConcurrentOverridesNeverConflict is the #346 R2
// fix, targeted at the SPECIFIC race the old implementation had: the key
// starts completely ABSENT, so every goroutine's first attempt takes the
// "no existing row" branch. Under the old rollback-then-delegate design, that
// branch released the connection and re-entered InsertManualCostEvent via a
// FRESH transaction — the exact window where a concurrent winner's committed
// row could appear between this goroutine's rollback and its delegated
// call's own SELECT, so the delegated call's OWN divergence check (comparing
// this goroutine's proposed cost against whatever just landed) would fire
// ErrCostConflict — a sentinel InsertManualCostEvent's caller-facing contract
// owns, never meant to escape CorrectManualCostEvent. Run with -race.
func TestCorrectManualCostEvent_ConcurrentOverridesNeverConflict(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const n = 32
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Every goroutine proposes a DIFFERENT cost under the SAME brand-
			// new key, so exactly one "wins" the real first insert and every
			// other goroutine's own comparison — wherever it happens to run
			// — sees a value that diverges from what it proposed.
			ev := manualCostEvent("race-key-2", int64(1000+i))
			_, err := db.CorrectManualCostEvent(ctx, ev, "finance-concurrent", "race test")
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: err = %v, want nil (CorrectManualCostEvent must never surface ErrCostConflict to its own caller)", i, err)
		}
	}
	// The final stored cost is whichever goroutine's transaction committed
	// last — not asserted (genuinely racy by design) — but it MUST be one of
	// the proposed values, and the row must still be exactly one row.
	got := onlyCostMicro(t, db)
	valid := false
	for i := 0; i < n; i++ {
		if got == int64(1000+i) {
			valid = true
			break
		}
	}
	if !valid {
		t.Errorf("final stored cost_micro = %d, not among any goroutine's proposed value", got)
	}
}

// TestCorrectManualCostEvent_RequiresActorAndReason covers the "never silent"
// contract: an override with a missing actor or reason must be refused, and
// must not mutate anything.
func TestCorrectManualCostEvent_RequiresActorAndReason(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	if err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 10_500)); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	cases := []struct {
		name, actor, reason string
	}{
		{"missing actor", "", "a reason"},
		{"missing reason", "an actor", ""},
		{"missing both", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.CorrectManualCostEvent(ctx, manualCostEvent("k1", 20_600), tc.actor, tc.reason); err == nil {
				t.Fatal("expected an error for a missing actor/reason, got nil")
			}
			if got := onlyCostMicro(t, db); got != 10_500 {
				t.Errorf("stored cost_micro = %d, want 10_500 unchanged (a rejected override must not mutate)", got)
			}
			if got := auditRowCount(t, db); got != 0 {
				t.Errorf("cost_correction_audit row count = %d, want 0", got)
			}
		})
	}
}

// TestCorrectManualCostEvent_RequiresIdempotencyKey covers the other half of
// the "never silent" contract: an unkeyed event has nothing to correct
// against (InsertManualCostEvent's own contract is that unkeyed inserts never
// conflict at all), so CorrectManualCostEvent must refuse it outright rather
// than silently falling through to a plain insert under an override banner.
func TestCorrectManualCostEvent_RequiresIdempotencyKey(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := db.CorrectManualCostEvent(ctx, manualCostEvent("", 10_500), "finance-alice", "no key"); err == nil {
		t.Fatal("expected an error for an empty idempotency_key, got nil")
	}
	if got := auditRowCount(t, db); got != 0 {
		t.Errorf("cost_correction_audit row count = %d, want 0", got)
	}
}

// TestCorrectManualCostEvent_SecondCorrectionAlsoAudited proves the ledger is
// genuinely append-only: a row corrected TWICE (a second finance revision)
// must produce TWO audit rows, not overwrite or reject the second.
func TestCorrectManualCostEvent_SecondCorrectionAlsoAudited(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 10_500)); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := db.CorrectManualCostEvent(ctx, manualCostEvent("k1", 20_600), "finance-alice", "first correction"); err != nil {
		t.Fatalf("first correction: %v", err)
	}
	res2, err := db.CorrectManualCostEvent(ctx, manualCostEvent("k1", 30_700), "finance-bob", "second correction (revised again)")
	if err != nil {
		t.Fatalf("second correction: %v", err)
	}
	if !res2.Corrected || res2.OldCostMicro != 20_600 || res2.NewCostMicro != 30_700 {
		t.Errorf("second correction result = %+v, want Corrected=true old=20600 new=30700", res2)
	}
	if got := onlyCostMicro(t, db); got != 30_700 {
		t.Errorf("stored cost_micro = %d, want 30_700", got)
	}
	if got := auditRowCount(t, db); got != 2 {
		t.Errorf("cost_correction_audit row count = %d, want 2 (append-only — a second correction must not overwrite the first)", got)
	}
}

// TestCorrectManualCostEvent_LegacyFidelityIsUncorrectable pins the one dead end
// that including fidelity in the identity tuple creates. #82 narrowed /costs to
// daily|estimated|omitted, and fidelity is INSERT-only, so a PRE-#82
// source='api' row can still carry 'realtime' — a value no current request can
// express. Every possible request therefore 409s: stating it is rejected 400 by
// the handler, and omitting it defaults to "estimated", which mismatches.
//
// This is asserted rather than fixed on purpose. The alternative — exempting
// fidelity when the stored value is outside the enum — would silently reopen the
// hole fidelity was added to close. The documented remedy is #295's: re-post
// under a new idempotency_key.
func TestCorrectManualCostEvent_LegacyFidelityIsUncorrectable(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// A pre-#82 row: source='api' with a fidelity /costs would reject today.
	legacy := manualCostEvent("legacy-key", 10_500)
	legacy.Fidelity = "realtime"
	if err := db.InsertManualCostEvent(ctx, legacy); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// Both requests an operator could actually construct through /costs.
	for _, claimed := range []string{"estimated", "daily"} {
		attempt := manualCostEvent("legacy-key", 20_600)
		attempt.Fidelity = claimed
		if _, err := db.CorrectManualCostEvent(ctx, attempt, "finance-alice", "legitimate"); !errors.Is(err, ErrCostCorrectionIdentityMismatch) {
			t.Errorf("fidelity=%q: err = %v, want ErrCostCorrectionIdentityMismatch", claimed, err)
		}
	}
	if got := onlyCostMicro(t, db); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 unchanged", got)
	}

	// Control: the store itself is not the blocker — stating the row's ACTUAL
	// fidelity works. It is the handler's #82 enum that makes this unreachable
	// over HTTP, which is precisely why the dead end is documented rather than
	// treated as a store bug.
	honest := manualCostEvent("legacy-key", 20_600)
	honest.Fidelity = "realtime"
	res, err := db.CorrectManualCostEvent(ctx, honest, "finance-alice", "legitimate")
	if err != nil {
		t.Fatalf("correct-fidelity request: %v", err)
	}
	if !res.Corrected {
		t.Error("Corrected = false for a request naming the row's actual fidelity")
	}
}
