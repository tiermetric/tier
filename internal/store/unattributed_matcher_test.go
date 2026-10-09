package store

// #466: the Go family matcher (IsUnattributed) and THE SQL family predicate
// (unattributedFamilyPredicate) must classify every stored issue_id identically, and
// each must classify it CORRECTLY. Agreement alone is not the property — two matchers
// wrong in the same way agree perfectly — so every case below carries an independent
// `want`, and the SQL arm is executed against a live SQLite connection using the SAME
// predicate text the production queries embed, never a restatement.
//
// The bug this pins: SQLite's LIKE is ASCII case-INSENSITIVE, so the original
// `issue_id LIKE 'unattributed:%'` matched "UNATTRIBUTED:MAIN" while Go's
// strings.HasPrefix did not. A forged mixed-case sentinel was then simultaneously
// unattributed to the #234 cost-composition split and spend-on-a-real-issue to the
// #466 reconciliation — the same dollar reported as two different, non-additive things
// in one response, against a named developer.

import (
	"context"
	"testing"
	"time"
)

// unattributedCase is one issue_id and the answer BOTH engines must give.
type unattributedCase struct {
	issueID string
	want    bool
	why     string
}

// unattributedCases is shared by the agreement test and the LIKE control arm below.
// The case-variant rows are load-bearing in BOTH: deleting them would not merely
// weaken the agreement test, it would make TestUnattributedFamilyLikeWouldDisagree
// fail, so the coverage cannot be quietly removed.
var unattributedCases = []unattributedCase{
	{UnattributedIssueID, true, "the bare sentinel"},
	{UnattributedMainBucket, true, "labeled sub-bucket :main"},
	{UnattributedDetachedHEADBucket, true, "labeled sub-bucket :detached-head"},
	{UnattributedNoIssueBucket, true, "labeled sub-bucket :branch-without-issue"},
	{UnattributedIssueID + ":", true, "empty reason is still the family (prefix match)"},
	{UnattributedIssueID + ":anything-at-all", true, "unknown reason is still the family"},

	// 🔴 THE CASE VARIANTS — the forgery LIKE used to accept and Go rejected.
	{"UNATTRIBUTED:main", false, "upper-case sentinel is NOT the sentinel"},
	{"UNATTRIBUTED:MAIN", false, "upper-case sentinel and reason"},
	{"Unattributed:Main", false, "mixed-case sentinel"},
	{"UNATTRIBUTED", false, "upper-case bare sentinel"},
	{"Unattributed", false, "title-case bare sentinel"},

	// Near-misses that must not be swept into the family.
	{"unattributed-main", false, "hyphen is not the ':' separator"},
	{"unattributedx", false, "sentinel is a strict prefix, not a substring rule"},
	{"xunattributed:main", false, "prefix must be anchored at the start"},
	{"#42", false, "an ordinary GitHub issue ref"},
	{"ABC-123", false, "an ordinary Jira-style ref"},
	{"", false, "empty issue id is not the sentinel"},

	// SQL wildcard metacharacters as DATA. ':' is literal in both LIKE and GLOB, but
	// '%'/'_' (LIKE) and '*'/'?'/'[' (GLOB) are not — an id containing them must be
	// judged on its text, not treated as a pattern.
	{"unattributed:%", true, "'%' is data inside the family, not a LIKE wildcard"},
	{"unattributed:*", true, "'*' is data inside the family, not a GLOB wildcard"},
	{"unattributed:[a-z]", true, "'[' is data inside the family, not a GLOB class"},
	// '_' here is DATA, and LIKE wildcards only ever live in the PATTERN — so this id
	// could not be swept in by '_' whatever the operator. It is in the table because
	// it is a plausible near-miss id, not because of any wildcard rule.
	{"unattributed_main", false, "'_' is not the ':' separator"},
}

// matchesFamilySQL runs THE production predicate — the exact
// unattributedFamilyPredicate constant every read embeds — against one issue id on a
// live SQLite connection. Building the statement from the shared constant rather than
// retyping the operator is the whole point: the LIKE/GLOB drift survived its first
// guard precisely because that guard shared only the CONSTANTS while the queries
// diverged on the OPERATOR.
//
// 🔴 IT EVALUATES OVER THE REAL token_events COLUMN, not over a `SELECT ? AS issue_id`
// literal, and that distinction is the same class of bug as #466 itself. SQLite takes
// `=`'s collation from the COLUMN, while a literal expression is always BINARY. The
// GLOB arm is collation-immune; the `issue_id = ?` arm is not. So with a literal
// subquery, adding `COLLATE NOCASE` to token_events.issue_id would re-open
// case-insensitive sentinel matching in the real read path while THIS TEST — written
// precisely to stop that — stayed green. Demonstrated on a scratch table: a NOCASE
// column matched 'UNATTRIBUTED' against the predicate; the literal form did not.
func matchesFamilySQL(t *testing.T, db *DB, issueID string) bool {
	t.Helper()
	ctx := context.Background()
	// Insert through the production writer so the row lands in the real column with
	// the real schema, then read it back through the real predicate.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "matcher-probe", IssueID: issueID, Model: "claude-sonnet-4",
		InputTok: 1, CostMicro: 1, Source: "jsonl", Fidelity: "realtime",
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed token_event %q: %v", issueID, err)
	}
	defer func() {
		if _, err := db.db.ExecContext(ctx,
			`DELETE FROM token_events WHERE developer = 'matcher-probe'`); err != nil {
			t.Fatalf("clean probe row: %v", err)
		}
	}()
	var matched int
	err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM token_events WHERE developer = 'matcher-probe' AND (`+
			unattributedFamilyPredicate+`)`,
		UnattributedIssueID, unattributedGlobPattern,
	).Scan(&matched)
	if err != nil {
		t.Fatalf("SQL family predicate on %q: %v", issueID, err)
	}
	return matched == 1
}

// TestUnattributedMatcherAgreesWithSQL asserts, for every case, that SQL and Go agree
// AND that the shared answer is the correct one.
func TestUnattributedMatcherAgreesWithSQL(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for _, tc := range unattributedCases {
		t.Run(tc.issueID, func(t *testing.T) {
			gotGo := IsUnattributed(tc.issueID)
			gotSQL := matchesFamilySQL(t, db, tc.issueID)
			// Correctness first: a shared wrong answer is the failure mode agreement
			// alone cannot see.
			if gotGo != tc.want {
				t.Errorf("IsUnattributed(%q) = %v, want %v (%s)", tc.issueID, gotGo, tc.want, tc.why)
			}
			if gotSQL != tc.want {
				t.Errorf("SQL family predicate on %q = %v, want %v (%s)", tc.issueID, gotSQL, tc.want, tc.why)
			}
			// Then agreement, reported separately so a divergence is unmistakable in
			// the failure output even if both correctness checks already fired.
			if gotGo != gotSQL {
				t.Errorf("MATCHER DRIFT on %q: Go=%v SQL=%v — the same row would be "+
					"counted as unattributed by one read path and attributed by the other (#466)",
					tc.issueID, gotGo, gotSQL)
			}
		})
	}
}

// TestUnattributedFamilyLikeWouldDisagree is the CONTROL ARM. It asserts that the
// pre-#466 `LIKE 'unattributed:%'` form still disagrees with Go on at least one case in
// the table, which proves two things at once:
//
//  1. The GLOB switch is load-bearing, not cosmetic. If someone reverts
//     unattributedGlobPattern to a LIKE pattern, the agreement test above fails.
//  2. The case-variant rows in unattributedCases cannot be deleted "to make the test
//     simpler". Remove them and THIS test fails for want of a disagreeing case, so the
//     coverage is pinned by the build rather than by a comment.
//
// It also demonstrates the SQLite behaviour the fix depends on, in situ, rather than
// asserting it from documentation.
func TestUnattributedFamilyLikeWouldDisagree(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	const likePredicate = `issue_id = ? OR issue_id LIKE ?`
	const likePattern = UnattributedIssueID + ":%"

	// Same discipline as matchesFamilySQL: over the real column, not a literal.
	matchesLike := func(issueID string) bool {
		ctx := context.Background()
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: "like-probe", IssueID: issueID, Model: "claude-sonnet-4",
			InputTok: 1, CostMicro: 1, Source: "jsonl", Fidelity: "realtime",
			Timestamp: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed token_event %q: %v", issueID, err)
		}
		defer func() {
			if _, err := db.db.ExecContext(ctx,
				`DELETE FROM token_events WHERE developer = 'like-probe'`); err != nil {
				t.Fatalf("clean probe row: %v", err)
			}
		}()
		var matched int
		if err := db.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM token_events WHERE developer = 'like-probe' AND (`+
				likePredicate+`)`,
			UnattributedIssueID, likePattern,
		).Scan(&matched); err != nil {
			t.Fatalf("LIKE predicate on %q: %v", issueID, err)
		}
		return matched == 1
	}

	var disagreements []string
	for _, tc := range unattributedCases {
		if matchesLike(tc.issueID) != IsUnattributed(tc.issueID) {
			disagreements = append(disagreements, tc.issueID)
		}
	}
	if len(disagreements) == 0 {
		t.Fatal("no case in unattributedCases makes LIKE disagree with IsUnattributed. " +
			"Either the case-variant rows were deleted (restore them — they are the #466 " +
			"forgery vector) or SQLite's LIKE stopped being case-insensitive, in which " +
			"case the GLOB rationale needs re-measuring, not deleting.")
	}
	// Name the canonical one explicitly so the intent survives a table edit.
	if matchesLike("UNATTRIBUTED:main") == IsUnattributed("UNATTRIBUTED:main") {
		t.Errorf(`LIKE and Go now AGREE on "UNATTRIBUTED:main"; the #466 vector this ` +
			`test documents has changed shape — re-derive it, do not delete this test`)
	}
	t.Logf("LIKE disagrees with IsUnattributed on %d case(s): %q", len(disagreements), disagreements)
}

// TestUnattributedGlobPatternHasNoAccidentalWildcard pins the pattern's own text. GLOB
// treats '*', '?' and '[' as metacharacters; the family pattern must contain exactly
// one, the trailing '*', or it silently matches a different set than its doc claims.
func TestUnattributedGlobPatternHasNoAccidentalWildcard(t *testing.T) {
	const want = UnattributedIssueID + ":*"
	if unattributedGlobPattern != want {
		t.Fatalf("unattributedGlobPattern = %q, want %q", unattributedGlobPattern, want)
	}
	body := unattributedGlobPattern[:len(unattributedGlobPattern)-1]
	for _, meta := range []rune{'*', '?', '['} {
		for _, r := range body {
			if r == meta {
				t.Errorf("GLOB metacharacter %q appears before the trailing '*' in %q; "+
					"the pattern matches more than the sentinel family",
					string(meta), unattributedGlobPattern)
			}
		}
	}
}
