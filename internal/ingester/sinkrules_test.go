package ingester

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

func openRealStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ingester-sinkrules.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func sinkEvent(key string, age time.Duration) collector.TokenEvent {
	return collector.TokenEvent{
		Developer:      "alice",
		IssueID:        "issue-42",
		Model:          "claude-sonnet-4",
		InputTok:       100,
		OutputTok:      50,
		CostMicro:      1_000,
		Source:         collector.SourceJSONL,
		Fidelity:       "realtime",
		IdempotencyKey: key,
		Repo:           "acme/widgets",
		Timestamp:      time.Now().UTC().Add(-age).Truncate(time.Second),
	}
}

func storedByKey(t *testing.T, db *store.DB) map[string]store.TokenEvent {
	t.Helper()
	now := time.Now().UTC()
	rows, _, err := db.ListTokenEvents(context.Background(), now.Add(-24*time.Hour), now.Add(time.Minute), store.PageCursor{}, store.MaxExportPageSize)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	out := make(map[string]store.TokenEvent, len(rows))
	for _, r := range rows {
		out[r.IdempotencyKey] = r
	}
	return out
}

// TestStore_ForeignRepoBucketStoresUnqualified pins the #823 Q3 rule on the
// direct-write path: an unattributed:foreign-repo event is stored with repo
// unqualified whatever the collector resolved, including a real slug, exactly as
// the shipper's path stores it. The attributed event keeps its slug.
func TestStore_ForeignRepoBucketStoresUnqualified(t *testing.T) {
	db := openRealStore(t)
	ing := Store(db)
	ctx := context.Background()

	foreign := sinkEvent("k-foreign-slug", time.Minute)
	foreign.IssueID = collector.UnattributedForeignRepo
	foreign.Repo = "acme/foreign"
	attributed := sinkEvent("k-attributed", 2*time.Minute)
	for _, ev := range []collector.TokenEvent{foreign, attributed} {
		if err := ing.Ingest(ctx, ev); err != nil {
			t.Fatalf("Ingest(%s): %v", ev.IdempotencyKey, err)
		}
	}

	stored := storedByKey(t, db)
	if got := stored["k-foreign-slug"]; got.IssueID != collector.UnattributedForeignRepo || got.Repo != repoid.Unqualified {
		t.Errorf("foreign-repo event stored (issue %q, repo %q), want (%q, %q)",
			got.IssueID, got.Repo, collector.UnattributedForeignRepo, repoid.Unqualified)
	}
	if got := stored["k-attributed"].Repo; got != "acme/widgets" {
		t.Errorf("attributed event stored repo %q, want acme/widgets", got)
	}
}

// TestStore_InvalidAttributionRuleDroppedNotRefused pins that the direct-write path
// degrades an out-of-set rule the way the wire does: the event is stored with no
// rule (a refusal would pin the watcher's checkpoint), a valid rule beside it is
// kept, and each adapter logs exactly one WARN that carries none of the values.
func TestStore_InvalidAttributionRuleDroppedNotRefused(t *testing.T) {
	invalid := []store.AttributionRule{"Branch", " carry", "legacy", "unknown", "/Users/alice/repo/.git/worktrees/x"}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	db := openRealStore(t)
	ctx := context.Background()
	n := 0
	for adapter := 0; adapter < 2; adapter++ {
		ing := Store(db)
		for _, r := range invalid {
			n++
			ev := sinkEvent("k-invalid-"+string(rune('a'+n)), time.Duration(n)*time.Minute)
			ev.AttributionRule = r
			if err := ing.Ingest(ctx, ev); err != nil {
				t.Fatalf("Ingest(rule %q): %v — a refused event pins the watcher's checkpoint", r, err)
			}
		}
	}
	valid := sinkEvent("k-valid", 30*time.Minute)
	valid.AttributionRule = store.AttributionRuleCarry
	if err := Store(db).Ingest(ctx, valid); err != nil {
		t.Fatalf("Ingest(valid): %v", err)
	}

	stored := storedByKey(t, db)
	if len(stored) != n+1 {
		t.Fatalf("stored %d events, want %d", len(stored), n+1)
	}
	for key, ev := range stored {
		want := store.AttributionRuleNone
		if key == "k-valid" {
			want = store.AttributionRuleCarry
		}
		if ev.AttributionRule != want {
			t.Errorf("event %s stored attribution_rule %q, want %q", key, ev.AttributionRule, want)
		}
	}

	warns := 0
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if rec[slog.LevelKey] == slog.LevelWarn.String() && strings.Contains(rec[slog.MessageKey].(string), "attribution_rule") {
			warns++
		}
		for _, v := range invalid {
			if strings.Contains(line, string(v)) {
				t.Errorf("log record carries dropped rule %q: %s", v, line)
			}
		}
	}
	if warns != 2 {
		t.Errorf("attribution_rule WARN records = %d, want 2 (one per adapter)\nlogs:\n%s", warns, logs.String())
	}
}
