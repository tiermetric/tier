package ingester

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
)

func TestStoreIssueRuleClosedSetAndSkew(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	db := openRealStore(t)
	ing := Store(db)
	for _, rule := range []string{"", "unknown", "/private/path", "branch_first_numeric"} {
		ev := sinkEvent(rule, time.Minute)
		ev.IssueRule = issueref.Rule(rule)
		if err := ing.Ingest(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	rows := storedByKey(t, db)
	for key, ev := range rows {
		want := ""
		if key == "branch_first_numeric" {
			want = key
		}
		if string(ev.IssueRule) != want {
			t.Fatal(key, ev.IssueRule)
		}
	}
	if len(rows) != 4 || strings.Count(logs.String(), "dropping an issue_rule") != 1 || strings.Contains(logs.String(), "/private/path") {
		t.Fatal(len(rows), logs.String())
	}
}
