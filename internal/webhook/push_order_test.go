package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// #938: a push day-row's owner is the earliest entry by (GitHub's
// repository.pushed_at, commit time, SHA). The commit time is set by whoever
// made the commit; pushed_at is set by GitHub when the push lands.

// pushAt delivers one default-branch push whose repository.pushed_at is
// pushedAt. A nil pushedAt leaves the key out of the payload entirely.
func (r *reconcileHarness) pushAt(t *testing.T, pushedAt any, commits ...map[string]any) {
	t.Helper()
	repo := map[string]any{"default_branch": "main", "full_name": reconcileRepo}
	if pushedAt != nil {
		repo["pushed_at"] = pushedAt
	}
	r.deliver(t, "push", map[string]any{"ref": "refs/heads/main", "repository": repo, "commits": commits})
}

// ledgerOrder returns developer's push_outcome_commits entries from the DSAR
// export as "sha=push_order", read from the JSON the export serves, so a
// missing push_order key fails here rather than reading as 0.
func ledgerOrder(t *testing.T, db *store.DB, developer string) []string {
	t.Helper()
	exp, err := db.ExportDeveloper(context.Background(), developer)
	if err != nil {
		t.Fatalf("ExportDeveloper(%s): %v", developer, err)
	}
	raw, err := json.Marshal(exp)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	var doc struct {
		Commits []map[string]json.RawMessage `json:"push_outcome_commits"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal export: %v", err)
	}
	out := make([]string, 0, len(doc.Commits))
	for _, c := range doc.Commits {
		order, ok := c["push_order"]
		if !ok {
			t.Fatalf("export of %s: push_outcome_commits row %s has no push_order column", developer, c["commit_sha"])
		}
		var sha string
		_ = json.Unmarshal(c["commit_sha"], &sha)
		out = append(out, sha+"="+string(order))
	}
	return out
}

func unixAt(hour, minute int) int64 {
	return reconcileDay.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute).Unix()
}

func pushOwnerRow(t *testing.T, r *reconcileHarness) string {
	t.Helper()
	var push []string
	for _, row := range r.rows(t) {
		if strings.HasPrefix(row, "push ") {
			push = append(push, row)
		}
	}
	if len(push) != 1 {
		t.Fatalf("push rows = %q, want exactly one", push)
	}
	return push[0]
}

const (
	victimRow   = "push dev=alice issue=issue-42 pr=0 w=0.5 sha= ts=2026-09-10T14:00:00Z"
	attackerRow = "push dev=mallory issue=issue-42 pr=0 w=0.5 sha= ts=2026-09-10T01:00:00Z"
)

func victimCommit() map[string]any {
	return pushCommit("a1a1a1a1", "feat: the thing (closes #42)", "alice", at(14))
}

// attackerCommit is dated 01:00 the same UTC day: the committer sets that time.
func attackerCommit() map[string]any {
	return pushCommit("b0b0b0b0", "chore: touch (closes #42)", "mallory", at(1))
}

// TestPushOrder_BackdatedLaterPushDoesNotTakeTheRow is #938's receipt: the
// victim's 14:00 commit is pushed first; the attacker's commit, dated 01:00, is
// pushed later. The victim keeps the row, and the attacker's commit is still
// recorded (its credit and the double-count guard stand).
func TestPushOrder_BackdatedLaterPushDoesNotTakeTheRow(t *testing.T) {
	r := newReconcileHarness(t)
	r.pushAt(t, unixAt(14, 5), victimCommit())
	r.pushAt(t, unixAt(15, 0), attackerCommit())
	if got := pushOwnerRow(t, r); got != victimRow {
		t.Fatalf("push row = %q, want %q (a later push of a backdated commit re-owned the row)", got, victimRow)
	}
	if got := ledgerOrder(t, r.db, "mallory"); len(got) != 1 {
		t.Fatalf("mallory's ledger entries = %q, want her one commit recorded", got)
	}
}

// TestPushOrder_DeliveryOrderDoesNotDecideTheOwner: GitHub delivers webhooks in
// no fixed order. The same two pushes, delivered either way round, store the
// same rows, owned by the commit that was PUSHED first.
func TestPushOrder_DeliveryOrderDoesNotDecideTheOwner(t *testing.T) {
	victim := func(r *reconcileHarness) { r.pushAt(t, unixAt(14, 5), victimCommit()) }
	attacker := func(r *reconcileHarness) { r.pushAt(t, unixAt(15, 0), attackerCommit()) }
	inOrder := newReconcileHarness(t)
	victim(inOrder)
	attacker(inOrder)
	outOfOrder := newReconcileHarness(t)
	attacker(outOfOrder)
	victim(outOfOrder)
	a, b := inOrder.rows(t), outOfOrder.rows(t)
	if strings.Join(a, "|") != strings.Join(b, "|") {
		t.Fatalf("in-order rows %q != out-of-order rows %q", a, b)
	}
	if got := pushOwnerRow(t, outOfOrder); got != victimRow {
		t.Fatalf("push row = %q, want %q", got, victimRow)
	}
}

// TestPushOrder_RedeliveryIsIdempotent: a redelivered push writes nothing — the
// rows, the ledger (sort keys included) and the audit are unchanged.
func TestPushOrder_RedeliveryIsIdempotent(t *testing.T) {
	r := newReconcileHarness(t)
	r.pushAt(t, unixAt(14, 5), victimCommit())
	r.pushAt(t, unixAt(15, 0), attackerCommit())
	before := r.state(t) + strings.Join(ledgerOrder(t, r.db, "alice"), ",") + strings.Join(ledgerOrder(t, r.db, "mallory"), ",")
	r.pushAt(t, unixAt(15, 0), attackerCommit())
	r.pushAt(t, unixAt(14, 5), victimCommit())
	after := r.state(t) + strings.Join(ledgerOrder(t, r.db, "alice"), ",") + strings.Join(ledgerOrder(t, r.db, "mallory"), ",")
	if before != after {
		t.Fatalf("redelivery changed the store:\n before %s\n after  %s", before, after)
	}
	if got, want := strings.Join(ledgerOrder(t, r.db, "mallory"), ","), "b0b0b0b0="+jsonInt(unixAt(15, 0)); got != want {
		t.Fatalf("mallory's ledger = %s, want %s", got, want)
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestPushOrder_MissingPushedAtNeverTakesARow: a payload with no parseable
// pushed_at is recorded with the maximum key, so it never takes a row over —
// neither one that exists already, nor from a later properly-keyed push.
func TestPushOrder_MissingPushedAtNeverTakesARow(t *testing.T) {
	maxKey := jsonInt(math.MaxInt64)
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
		t.Run(tc.name+"/after", func(t *testing.T) {
			r := newReconcileHarness(t)
			r.pushAt(t, unixAt(14, 5), victimCommit())
			r.pushAt(t, tc.pushedAt, attackerCommit())
			if got := pushOwnerRow(t, r); got != victimRow {
				t.Fatalf("push row = %q, want %q", got, victimRow)
			}
			if got := strings.Join(ledgerOrder(t, r.db, "mallory"), ","); got != "b0b0b0b0="+maxKey {
				t.Fatalf("mallory's ledger = %s, want her commit recorded with key %s", got, maxKey)
			}
		})
		t.Run(tc.name+"/before", func(t *testing.T) {
			r := newReconcileHarness(t)
			r.pushAt(t, tc.pushedAt, attackerCommit())
			r.pushAt(t, unixAt(14, 5), victimCommit())
			if got := pushOwnerRow(t, r); got != victimRow {
				t.Fatalf("push row = %q, want %q", got, victimRow)
			}
		})
	}
}

// TestPushOrder_UnkeyedLaterPushDoesNotTakeAnUnkeyedRow: two pushes with no
// usable pushed_at share the maximum key, and among them the one recorded first
// keeps the row, even when the later one carries an earlier commit time.
func TestPushOrder_UnkeyedLaterPushDoesNotTakeAnUnkeyedRow(t *testing.T) {
	r := newReconcileHarness(t)
	r.pushAt(t, nil, victimCommit())
	r.pushAt(t, nil, attackerCommit())
	if got := pushOwnerRow(t, r); got != victimRow {
		t.Fatalf("push row = %q, want %q (a later unkeyed push of a backdated commit re-owned the row)", got, victimRow)
	}
}

// TestPushOrder_PushedAtUnixInteger: the documented push-event shape (an
// integer of Unix seconds) is the sort key.
func TestPushOrder_PushedAtUnixInteger(t *testing.T) {
	r := newReconcileHarness(t)
	r.pushAt(t, unixAt(14, 5), victimCommit())
	if got, want := strings.Join(ledgerOrder(t, r.db, "alice"), ","), "a1a1a1a1="+jsonInt(unixAt(14, 5)); got != want {
		t.Fatalf("alice's ledger = %s, want %s", got, want)
	}
}

// TestPushOrder_PushedAtISOString: the schema's other documented shape (an
// RFC 3339 date-time string) is the same instant.
func TestPushOrder_PushedAtISOString(t *testing.T) {
	r := newReconcileHarness(t)
	r.pushAt(t, "2026-09-10T16:05:00+02:00", victimCommit())
	if got, want := strings.Join(ledgerOrder(t, r.db, "alice"), ","), "a1a1a1a1="+jsonInt(unixAt(14, 5)); got != want {
		t.Fatalf("alice's ledger = %s, want %s", got, want)
	}
}

// TestPushOrder_UpgradeKeepsEveryOwner is the migration: a database written
// before the sort key existed — ledger entries and a pre-ledger marker — gets
// key 0 on every entry, no row's owner changes, and a later push of an earlier-
// dated commit cannot take a pre-upgrade row.
func TestPushOrder_UpgradeKeepsEveryOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	// A push row no ledger saw (pre-#849): alice, issue 42, 14:00.
	if _, err := db.UpsertPushOutcome(ctx, store.Outcome{
		Developer: "alice", IssueID: "issue-42", Weight: 0.5, WeightSource: store.WeightSourcePush,
		Quality: 1, Source: store.OutcomeSourcePush, Repo: reconcileRepo, Timestamp: at(14),
	}, "2026-09-10"); err != nil {
		t.Fatalf("UpsertPushOutcome: %v", err)
	}
	r := &reconcileHarness{db: db, h: New(db, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))}
	// bob joins issue 42 (writes alice's pre-ledger marker); carol owns issue 43.
	r.pushAt(t, unixAt(16, 5), pushCommit("b1b1b1b1", "fix: #42", "bob", at(16)))
	r.pushAt(t, unixAt(10, 5), pushCommit("c1c1c1c1", "feat: closes #43", "carol", at(10)))
	owners := strings.Join(r.rows(t), "|")
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Rewind the file to the pre-#938 shape.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE push_outcome_commits DROP COLUMN push_order`); err != nil {
		t.Fatalf("rewind to the pre-#938 schema: %v", err)
	}
	_ = raw.Close()

	db, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen (upgrade): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r = &reconcileHarness{db: db, h: New(db, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))}
	if got := strings.Join(r.rows(t), "|"); got != owners {
		t.Fatalf("upgrade moved rows:\n before %s\n after  %s", owners, got)
	}
	var entries []string
	for _, dev := range []string{"alice", "bob", "carol"} {
		entries = append(entries, ledgerOrder(t, db, dev)...)
	}
	if got := strings.Join(entries, ","); !strings.HasPrefix(got, "pre-ledger:") ||
		!strings.Contains(got, "b1b1b1b1=0") || !strings.Contains(got, "c1c1c1c1=0") || strings.Count(got, "=0") != 3 {
		t.Fatalf("ledger after upgrade = %s, want the marker and both entries at key 0", got)
	}

	// dave pushes a commit dated 09:00, before carol's 10:00, after the upgrade.
	r.pushAt(t, unixAt(18, 0), pushCommit("d1d1d1d1", "fix: #43", "dave", at(9)))
	// mallory pushes a commit dated 01:00 onto alice's pre-ledger row.
	r.pushAt(t, unixAt(18, 1), attackerCommit())
	want := strings.Join(r.rows(t), "|")
	if want != owners {
		t.Fatalf("a post-upgrade push took a pre-upgrade row:\n before %s\n after  %s", owners, want)
	}
}

// TestPushOrder_RepushedCommitKeepsItsEarliestPushTime: a commit can reach the
// default branch in several pushes (removed, then reintroduced). alice's A is
// pushed at 14:05, bob's B at 15:00, A again at 16:00. The owner must not depend
// on which delivery of A arrived first: A keeps its earliest push time, so alice
// owns the row delivered in push order and in reverse.
func TestPushOrder_RepushedCommitKeepsItsEarliestPushTime(t *testing.T) {
	bob := pushCommit("b2b2b2b2", "fix: #42", "bob", at(12))
	deliveries := []func(r *reconcileHarness){
		func(r *reconcileHarness) { r.pushAt(t, unixAt(14, 5), victimCommit()) },
		func(r *reconcileHarness) { r.pushAt(t, unixAt(15, 0), bob) },
		func(r *reconcileHarness) { r.pushAt(t, unixAt(16, 0), victimCommit()) },
	}
	inOrder, reversed := newReconcileHarness(t), newReconcileHarness(t)
	for i := range deliveries {
		deliveries[i](inOrder)
		deliveries[len(deliveries)-1-i](reversed)
	}
	for name, r := range map[string]*reconcileHarness{"in push order": inOrder, "reversed": reversed} {
		if got := pushOwnerRow(t, r); got != victimRow {
			t.Errorf("%s: push row = %q, want %q (a re-push's later time decided the owner)", name, got, victimRow)
		}
		if got, want := strings.Join(ledgerOrder(t, r.db, "alice"), ","), "a1a1a1a1="+jsonInt(unixAt(14, 5)); got != want {
			t.Errorf("%s: alice's ledger = %s, want %s (the earliest push time)", name, got, want)
		}
	}
}
