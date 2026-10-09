package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func attributionRuleOf(t *testing.T, db *DB, key string) sql.NullString {
	t.Helper()
	var r sql.NullString
	if err := db.db.QueryRow(`SELECT attribution_rule FROM token_events WHERE idempotency_key = ?`, key).Scan(&r); err != nil {
		t.Fatalf("select attribution_rule for %q: %v", key, err)
	}
	return r
}

func ruleEvent(key string, rule AttributionRule) TokenEvent {
	return TokenEvent{
		Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4", InputTok: 10,
		Source: "jsonl", Fidelity: "realtime", IdempotencyKey: key,
		AttributionRule: rule, Timestamp: time.Now().UTC(),
	}
}

// TestAttributionRule_FirstWriterWins pins that a keyed replay never rewrites the
// stored rule — not to another rule, not to NULL, and not from NULL — so the rule
// always describes the issue_id the first writer stored.
func TestAttributionRule_FirstWriterWins(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, tc := range []struct {
		key           string
		first, second AttributionRule
		want          sql.NullString
	}{
		{"k-rule-rule", AttributionRuleBranch, AttributionRuleCarry, sql.NullString{String: "branch", Valid: true}},
		{"k-rule-none", AttributionRuleWorktreeCWD, AttributionRuleNone, sql.NullString{String: "worktree-cwd", Valid: true}},
		{"k-none-rule", AttributionRuleNone, AttributionRuleWorktreeToolPath, sql.NullString{}},
	} {
		if err := db.InsertTokenEvent(ctx, ruleEvent(tc.key, tc.first)); err != nil {
			t.Fatalf("%s: first insert: %v", tc.key, err)
		}
		if err := db.InsertTokenEvents(ctx, []TokenEvent{ruleEvent(tc.key, tc.second)}); err != nil {
			t.Fatalf("%s: replay: %v", tc.key, err)
		}
		if got := attributionRuleOf(t, db, tc.key); got != tc.want {
			t.Errorf("%s: attribution_rule = %+v, want %+v (first writer wins)", tc.key, got, tc.want)
		}
	}
}

// TestAttributionRule_MigratedRowsNull pins the migration: a database stored
// before the column existed gains it as nullable TEXT with no DEFAULT, its rows
// read NULL, and a row inserted afterwards stores its rule.
func TestAttributionRule_MigratedRowsNull(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pre823.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE token_events DROP COLUMN attribution_rule`); err != nil {
		t.Fatalf("drop attribution_rule (the column Open must add): %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO token_events (developer, issue_id, model, cost_micro, source, fidelity, idempotency_key, ts)
		VALUES ('alice', 'issue-1', 'claude-sonnet-4', 1, 'jsonl', 'realtime', 'k-old', ?)`, time.Now().UTC()); err != nil {
		t.Fatalf("seed pre-#823 row: %v", err)
	}
	_ = raw.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-#823 database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var typ string
	var notNull int
	var dflt sql.NullString
	if err := db.db.QueryRow(`SELECT type, "notnull", dflt_value FROM pragma_table_info('token_events') WHERE name = 'attribution_rule'`).Scan(&typ, &notNull, &dflt); err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	if typ != "TEXT" || notNull != 0 || dflt.Valid {
		t.Errorf("migrated column = (%s, notnull=%d, default=%+v), want (TEXT, 0, NULL)", typ, notNull, dflt)
	}
	if got := attributionRuleOf(t, db, "k-old"); got.Valid {
		t.Errorf("pre-#823 row attribution_rule = %+v, want NULL", got)
	}
	if err := db.InsertTokenEvent(ctx, ruleEvent("k-new", AttributionRuleCarry)); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
	if got := attributionRuleOf(t, db, "k-new"); got.String != "carry" {
		t.Errorf("post-migration row attribution_rule = %+v, want carry", got)
	}
}

// TestAttributionRuleEnum_ClosedSet pins the exact recordable set and that the
// validator accepts nothing beyond it and the empty rule.
func TestAttributionRuleEnum_ClosedSet(t *testing.T) {
	want := []AttributionRule{"branch", "worktree-cwd", "worktree-toolpath", "carry"}
	if got := AttributionRules(); !slices.Equal(got, want) {
		t.Fatalf("AttributionRules() = %q, want %q", got, want)
	}
	for _, r := range append(want, AttributionRuleNone) {
		if err := ValidateAttributionRule(r); err != nil {
			t.Errorf("ValidateAttributionRule(%q) = %v, want nil", r, err)
		}
	}
	for _, r := range []AttributionRule{"legacy", "Branch", "worktree", "carry ", "unattributed:foreign-repo", "NULL"} {
		if err := ValidateAttributionRule(r); !errors.Is(err, ErrInvalidAttributionRule) {
			t.Errorf("ValidateAttributionRule(%q) = %v, want ErrInvalidAttributionRule", r, err)
		}
	}
}

// TestAttributionRule_NoPathText pins that no recordable rule is path-shaped and
// that path-bearing values are refused.
func TestAttributionRule_NoPathText(t *testing.T) {
	label := regexp.MustCompile(`^[a-z]+(-[a-z]+)*$`)
	for _, r := range AttributionRules() {
		if !label.MatchString(string(r)) {
			t.Errorf("rule %q is not a bare lowercase label", r)
		}
	}
	for _, r := range []AttributionRule{"/Users/alice/wt", "worktree-cwd:/tmp/x", "../carry", `C:\wt`, "~/.claude", "carry/x"} {
		if err := ValidateAttributionRule(r); !errors.Is(err, ErrInvalidAttributionRule) {
			t.Errorf("ValidateAttributionRule(%q) = %v, want ErrInvalidAttributionRule", r, err)
		}
	}
}

// badRule is path-shaped on purpose: a refusal must never echo it.
const badRule AttributionRule = "/Users/alice/wt"

func wantRefused(t *testing.T, path string, err error) {
	t.Helper()
	if !errors.Is(err, ErrInvalidAttributionRule) {
		t.Errorf("%s = %v, want ErrInvalidAttributionRule", path, err)
	} else if strings.Contains(err.Error(), string(badRule)) {
		t.Errorf("%s error %q echoes the refused value", path, err)
	}
}

// TestAttributionRule_InvalidRefusedAtInsert pins that every insert path refuses
// a rule outside the closed set, stores nothing, and never echoes the value.
func TestAttributionRule_InvalidRefusedAtInsert(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	bad := ruleEvent("k-bad", badRule)
	wantRefused(t, "InsertTokenEvent", db.InsertTokenEvent(ctx, bad))
	wantRefused(t, "InsertTokenEvents", db.InsertTokenEvents(ctx, []TokenEvent{ruleEvent("k-ok", AttributionRuleBranch), bad}))
	bad.Source = "api"
	wantRefused(t, "InsertManualCostEvent", db.InsertManualCostEvent(ctx, bad))
	_, err = db.CorrectManualCostEvent(ctx, bad, "admin", "test")
	wantRefused(t, "CorrectManualCostEvent", err)
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM token_events`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("token_events holds %d rows after refused inserts, want 0 (the batch rolls back)", n)
	}
}

// TestAttributionRule_ManualReplayRefused pins that the manual-cost paths refuse
// an invalid rule on a key that already owns a row, not only on a fresh insert.
func TestAttributionRule_ManualReplayRefused(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ev := ruleEvent("k-manual", AttributionRuleBranch)
	ev.Source, ev.CostMicro = "api", 1_000_000
	if err := db.InsertManualCostEvent(ctx, ev); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ev.AttributionRule = badRule
	wantRefused(t, "InsertManualCostEvent replay", db.InsertManualCostEvent(ctx, ev))
	_, err = db.CorrectManualCostEvent(ctx, ev, "admin", "test")
	wantRefused(t, "CorrectManualCostEvent on an existing row", err)
	_, err = db.CorrectExistingManualCostEvent(ctx, ev, "admin", "test")
	wantRefused(t, "CorrectExistingManualCostEvent", err)
}

// TestValidateAttributionRule_AcceptsExactlySet pins the validator's accept set
// to AttributionRules() plus the empty rule, over a corpus of near-miss labels
// (each rule's case, whitespace, prefix, suffix and join variants, and the known
// neighbouring labels). A value accepted anywhere but AttributionRules() fails
// here; a value added to AttributionRules() fails TestAttributionRuleEnum_ClosedSet.
func TestValidateAttributionRule_AcceptsExactlySet(t *testing.T) {
	accepted := map[AttributionRule]bool{AttributionRuleNone: true}
	for _, r := range AttributionRules() {
		accepted[r] = true
	}
	corpus := []string{"legacy", "none", "null", "unattributed", "unattributed:foreign-repo",
		"foreign-repo", "worktree", "cwd", "toolpath", "path", "session", "subagent", "-", " "}
	for _, r := range AttributionRules() {
		s := string(r)
		corpus = append(corpus, s, strings.ToUpper(s), " "+s, s+" ", s+"\n", s[:len(s)-1], s+"s",
			"x"+s, s+"-"+s, strings.ReplaceAll(s, "-", "_"), strings.ReplaceAll(s, "-", ""))
		corpus = append(corpus, strings.Split(s, "-")...)
	}
	for _, c := range corpus {
		r := AttributionRule(c)
		err := ValidateAttributionRule(r)
		if accepted[r] != (err == nil) {
			t.Errorf("ValidateAttributionRule(%q) = %v, want accepted=%v", c, err, accepted[r])
		}
	}
}

// TestAttributionRule_DSARExportValue pins that the DSAR export carries the
// stored rule, and null for a row that recorded none.
func TestAttributionRule_DSARExportValue(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, ev := range []TokenEvent{ruleEvent("k-carry", AttributionRuleCarry), ruleEvent("k-none", AttributionRuleNone)} {
		if err := db.InsertTokenEvent(ctx, ev); err != nil {
			t.Fatalf("insert %s: %v", ev.IdempotencyKey, err)
		}
	}
	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper: %v", err)
	}
	got := map[string]*string{}
	for _, r := range exp.TokenEvents {
		if r.IdempotencyKey != nil {
			got[*r.IdempotencyKey] = r.AttributionRule
		}
	}
	if len(got) != 2 {
		t.Fatalf("export has %d keyed token rows, want 2", len(got))
	}
	if r := got["k-carry"]; r == nil || *r != "carry" {
		t.Errorf("k-carry attribution_rule = %v, want \"carry\"", r)
	}
	if r := got["k-none"]; r != nil {
		t.Errorf("k-none attribution_rule = %q, want null", *r)
	}
}

// TestAttributionRule_ListTokenEventsRoundTrip pins that ListTokenEvents reads the
// stored rule back, and "" for a row that recorded none.
func TestAttributionRule_ListTokenEventsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, ev := range []TokenEvent{ruleEvent("k-carry", AttributionRuleCarry), ruleEvent("k-none", AttributionRuleNone)} {
		if err := db.InsertTokenEvent(ctx, ev); err != nil {
			t.Fatalf("insert %s: %v", ev.IdempotencyKey, err)
		}
	}
	now := time.Now().UTC()
	events, _, err := db.ListTokenEvents(ctx, now.Add(-time.Hour), now.Add(time.Hour), PageCursor{}, 0)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	got := map[string]AttributionRule{}
	for _, e := range events {
		got[e.IdempotencyKey] = e.AttributionRule
	}
	if len(got) != 2 {
		t.Fatalf("ListTokenEvents returned %d keyed rows, want 2", len(got))
	}
	if got["k-carry"] != AttributionRuleCarry || got["k-none"] != AttributionRuleNone {
		t.Errorf("read-back rules = %q, want k-carry=carry, k-none=\"\"", got)
	}
}
