package proxy

// #466: X-Tier-Issue is UNAUTHENTICATED and taken verbatim. Without a guard, any
// caller could assert the server-assigned unattributed sentinel — or a case variant of
// it — about spend it is otherwise attributing to itself, moving its own dollars out of
// segment_reconciliation.no_outcome and out of the attributed side of the #234
// coverage split.
//
// 🔴 THE DELIBERATE CHOICE THESE TESTS PIN: a forged header is treated as MISSING, not
// as an error. The proxy sits on the request path; failing a provider call over
// attribution metadata would turn a reporting concern into an outage. The stored value
// is the same one the server would have assigned anyway — what changes is that the
// counter records it as unresolved, under a DISTINCT label, so an operator can tell
// "client is misconfigured" from "client is asserting the sentinel".

// ⚠️ NO t.Parallel() ANYWHERE IN THIS FILE, deliberately. httptest.Server.Close calls
// http.DefaultTransport.CloseIdleConnections(), and these tests drive the proxy through
// the shared http.DefaultClient — so a parallel test here tears connections out from
// under other tests in this package mid-flight. Same reason logsafe_test.go documents.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/collector"
)

// proxyWithForgedIssue drives one request carrying the given X-Tier-Issue through a
// real Proxy against a real upstream, and returns the stored event plus the
// unattributed-counter labels.
func proxyWithForgedIssue(t *testing.T, issueHeader string) (collector.TokenEvent, []string, int) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_forge","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	sink := newMemSink()
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, nil, nil)
	rec := &recRecorder{}
	p.SetUnattributedRecorder(rec)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tier-Developer", "mallory")
	if issueHeader != "" {
		req.Header.Set("X-Tier-Issue", issueHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if len(sink.events) != 1 {
		t.Fatalf("expected 1 stored event, got %d", len(sink.events))
	}
	return sink.events[0], rec.snapshot(), resp.StatusCode
}

// TestProxy_ForgedIssueHeaderTreatedAsMissing is the core guard, over the whole family
// including case variants.
func TestProxy_ForgedIssueHeaderTreatedAsMissing(t *testing.T) {
	for _, forged := range []string{
		collector.UnattributedIssueID,
		collector.UnattributedMain,
		collector.UnattributedDetachedHEAD,
		collector.UnattributedNoIssue,
		"UNATTRIBUTED",
		"UNATTRIBUTED:main",
		"Unattributed:Main",
		"unattributed:whatever-i-like",
		"  unattributed:main  ",
	} {
		t.Run(forged, func(t *testing.T) {
			ev, labels, status := proxyWithForgedIssue(t, forged)

			// 🔴 NEVER FAIL THE PROVIDER CALL over attribution metadata.
			if status != http.StatusOK {
				t.Errorf("provider call returned %d; a forged attribution header must "+
					"never break the request path", status)
			}
			// The stored value collapses to the BASE sentinel, not the forged text —
			// so a labeled sub-bucket cannot be asserted either.
			if ev.IssueID != collector.UnattributedIssueID {
				t.Errorf("stored issue_id = %q, want %q (a forged header must be treated "+
					"exactly as a MISSING one)", ev.IssueID, collector.UnattributedIssueID)
			}
			// Distinct label: forging must be DISTINGUISHABLE from omission, or the
			// guard makes forging exactly as effective as leaving the header off.
			if !slices.Contains(labels, "issue-forged") {
				t.Errorf("unattributed counter labels = %v, want an \"issue-forged\" "+
					"sample; an operator cannot otherwise tell a misconfigured client "+
					"from one asserting the sentinel", labels)
			}
			if slices.Contains(labels, "issue") {
				t.Errorf("unattributed counter labels = %v; a forged header must not "+
					"also count as a plain missing one (double count)", labels)
			}
		})
	}
}

// TestProxy_MissingIssueStillCountsAsPlainUnattributed is the CONTROL ARM. Without it
// the assertions above are satisfied by a proxy that labels EVERYTHING "issue-forged",
// which would destroy the very signal the distinct label exists to provide.
func TestProxy_MissingIssueStillCountsAsPlainUnattributed(t *testing.T) {
	ev, labels, status := proxyWithForgedIssue(t, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if ev.IssueID != collector.UnattributedIssueID {
		t.Errorf("stored issue_id = %q, want %q", ev.IssueID, collector.UnattributedIssueID)
	}
	if !slices.Contains(labels, "issue") {
		t.Errorf("labels = %v, want a plain \"issue\" sample for an ABSENT header", labels)
	}
	if slices.Contains(labels, "issue-forged") {
		t.Errorf("labels = %v; an absent header is not a forgery", labels)
	}
}

// TestProxy_OrdinaryIssueHeaderIsPreserved is the second control arm: the guard must
// not swallow legitimate attribution. A predicate that rejected too much would move
// every proxied dollar into the unattributed bucket — the #234 coverage split would
// collapse to 0% attributed and nobody would see an error.
func TestProxy_OrdinaryIssueHeaderIsPreserved(t *testing.T) {
	for _, ok := range []string{
		"#42", "ABC-123", "unattributed-work", "unattributedly",
		"not-unattributed", "xunattributed:main",
	} {
		t.Run(ok, func(t *testing.T) {
			ev, labels, status := proxyWithForgedIssue(t, ok)
			if status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			if ev.IssueID != ok {
				t.Errorf("stored issue_id = %q, want %q — the forge guard is rejecting "+
					"ordinary issue ids, which silently de-attributes real spend", ev.IssueID, ok)
			}
			if slices.Contains(labels, "issue") || slices.Contains(labels, "issue-forged") {
				t.Errorf("labels = %v; a resolved issue must record no unattributed sample", labels)
			}
		})
	}
}

// TestIsReservedIssueID pins the predicate itself, including the whitespace and case
// dimensions the header path depends on.
func TestIsReservedIssueID(t *testing.T) {
	reserved := []string{
		collector.UnattributedIssueID, collector.UnattributedMain,
		collector.UnattributedDetachedHEAD, collector.UnattributedNoIssue,
		"UNATTRIBUTED", "Unattributed", "UNATTRIBUTED:main", "Unattributed:Main",
		"unattributed:", "unattributed:anything", "  unattributed:main  ", "\tunattributed\n",
	}
	for _, s := range reserved {
		if !isReservedIdentifier(s) {
			t.Errorf("isReservedIdentifier(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"", "#42", "ABC-123", "unattributed-work", "unattributedly",
		"not-unattributed", "xunattributed:main", "un attributed",
	} {
		if isReservedIdentifier(s) {
			t.Errorf("isReservedIdentifier(%q) = true, want false — rejecting an ordinary "+
				"issue id de-attributes real spend", s)
		}
	}
}
