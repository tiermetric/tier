package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/store"
)

// Exercise the published shell payload through the signed webhook handler so
// HTTP 204 alone cannot hide a missing issue reference and a skipped outcome.
func TestWebhookSetupDocPayloadWritesPromisedOutcome(t *testing.T) {
	doc, err := os.ReadFile("../../docs/webhook-setup.md")
	if err != nil {
		t.Fatal(err)
	}
	var payloads []string
	inShell := false
	for _, line := range strings.Split(string(doc), "\n") {
		if line == "```sh" {
			inShell = true
			continue
		}
		if line == "```" {
			inShell = false
		}
		if inShell && strings.HasPrefix(line, "PAYLOAD='") {
			if !strings.HasSuffix(line, "'") {
				t.Fatal("PAYLOAD must be a single-quoted JSON line")
			}
			payloads = append(payloads, strings.TrimSuffix(strings.TrimPrefix(line, "PAYLOAD='"), "'"))
		}
	}
	if len(payloads) != 1 {
		t.Fatalf("want one PAYLOAD in a fenced sh block, got %d", len(payloads))
	}
	body := []byte(payloads[0])
	var p prPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("parse documented PAYLOAD: %v", err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	h := New(db, testSecret, quietLogger())
	if code := doPR(t, h, body, "webhook-setup-doc", sign(testSecret, body)); code != http.StatusNoContent {
		t.Fatalf("signed documented PAYLOAD: HTTP %d, want 204", code)
	}
	rows, _, err := db.ListOutcomes(context.Background(), time.Time{}, time.Now().UTC().Add(time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("signed documented PAYLOAD: HTTP 204, outcomes=%d", len(rows))
	if len(rows) != 1 {
		t.Fatalf("documented PAYLOAD wrote %d outcomes, want 1", len(rows))
	}
	o := rows[0]
	if !strings.Contains(string(doc), "`"+o.IssueID+"`") {
		t.Errorf("doc does not name stored issue %q", o.IssueID)
	}
	if !strings.Contains(string(doc), "`"+o.Developer+"`") {
		t.Errorf("doc does not name stored developer %q", o.Developer)
	}
	if o.PRNumber != p.PullRequest.Number || o.IssueRule != issueref.RuleBodyCloseDirective {
		t.Fatalf("stored outcome %+v, want PR=%d rule=%q", o, p.PullRequest.Number, issueref.RuleBodyCloseDirective)
	}
}
