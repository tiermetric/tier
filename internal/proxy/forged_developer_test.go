package proxy

// #619: X-Tier-Developer is UNAUTHENTICATED and was taken verbatim. #466 closed the
// same hole on X-Tier-Issue and deliberately did not widen scope to this header.
//
// 🔴 THIS IS THE WORSE HALF. TIER is points / (cost/1000). A forged X-Tier-Issue moves
// a dollar between buckets INSIDE the sender's own denominator and leaves the headline
// score untouched. A forged X-Tier-Developer moves the dollar OUT of that denominator:
// it lands on the "unattributed" pseudo-developer, the sender's cost falls, and the
// sender's score RISES. One is an accounting smudge; this one pays.
//
// The DELIBERATE CHOICE these tests pin is the same one #466 made, for the same
// reason: a forged header is treated as MISSING, never as an error. The proxy sits on
// the request path and must never fail a provider call over attribution metadata. What
// changes is the counter label — "developer-forged" rather than "developer" — so an
// operator can tell a misconfigured client from one asserting the sentinel.

// ⚠️ NO t.Parallel() ANYWHERE IN THIS FILE, deliberately — same reason
// forged_issue_test.go documents: httptest.Server.Close calls
// http.DefaultTransport.CloseIdleConnections() and these tests share http.DefaultClient.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// TestProxyGuardDelegatesToTheSharedPredicate is THE CROSS-PACKAGE DRIFT PIN, and it
// guards the copy that historically actually drifted.
//
// Under #466 this package hand-restated the rule as a four-liner, justified by "the
// proxy cannot import internal/api". Two independent implementations of one security
// predicate is exactly the shape of the #466 postmortem, where a matcher diverged from
// its twin while every shared CONSTANT still matched — so constant equality is not
// enough, the behaviour has to be pinned. The rule now lives in internal/store beside
// the sentinel (no cycle: store imports only logsafe and repoid), and this asserts the
// proxy genuinely delegates rather than merely agreeing today.
func TestProxyGuardDelegatesToTheSharedPredicate(t *testing.T) {
	for _, s := range []string{
		collector.UnattributedIssueID, collector.UnattributedMain,
		collector.UnattributedDetachedHEAD, collector.UnattributedNoIssue,
		"UNATTRIBUTED", "Unattributed:Main", "  unattributed:main  ", "unattributed:",
		"mallory", "unattributed-bot", "unattributedly", "not-unattributed",
		"xunattributed:main", "unknown", "", "#42",
		// Multi-byte: neither side may treat these as the sentinel.
		"unattrıbuted", "ＵＮＡＴＴＲＩＢＵＴＥＤ", "unattributedé:main", "ünattributed:main",
	} {
		if got, want := isReservedIdentifier(s), store.ResemblesUnattributed(s); got != want {
			t.Errorf("isReservedIdentifier(%q) = %v but store.ResemblesUnattributed = %v — "+
				"the proxy has a SECOND implementation of the sentinel rule again. That is "+
				"the #466 drift shape: the two guards disagree about one value, so the same "+
				"forgery is refused on one surface and stored on the other", s, got, want)
		}
	}
}

// proxyWithDeveloperHeader drives one request through a real Proxy against a real
// upstream carrying the given X-Tier-Developer (omitted when ""), and returns the
// stored event, the unattributed-counter labels, and the client-visible status.
//
// X-Tier-Issue is always set to a real issue, so every unattributed sample observed
// here is attributable to the developer header alone.
func proxyWithDeveloperHeader(t *testing.T, developerHeader string) (collector.TokenEvent, []string, int) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_devforge","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":5}}`)
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
	req.Header.Set("X-Tier-Issue", "#42")
	if developerHeader != "" {
		req.Header.Set("X-Tier-Developer", developerHeader)
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

// TestProxy_ForgedDeveloperHeaderTreatedAsMissing is the core guard, over the whole
// sentinel family including case variants and smuggling whitespace.
func TestProxy_ForgedDeveloperHeaderTreatedAsMissing(t *testing.T) {
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
			ev, labels, status := proxyWithDeveloperHeader(t, forged)

			// 🔴 NEVER FAIL THE PROVIDER CALL over attribution metadata.
			if status != http.StatusOK {
				t.Errorf("provider call returned %d; a forged attribution header must "+
					"never break the request path", status)
			}
			// The stored value collapses to the BASE sentinel, not the forged text, so
			// a labeled sub-bucket cannot be asserted into the developer column either.
			if ev.Developer != collector.UnattributedIssueID {
				t.Errorf("stored developer = %q, want %q (a forged header must be treated "+
					"exactly as a MISSING one)", ev.Developer, collector.UnattributedIssueID)
			}
			// The issue header was honest and must be UNTOUCHED — the developer guard
			// must not collapse an attributed issue into the sentinel as a side effect.
			if ev.IssueID != "#42" {
				t.Errorf("stored issue_id = %q, want %q; guarding the developer header "+
					"must not disturb a legitimate issue", ev.IssueID, "#42")
			}
			// Distinct label: forging must be DISTINGUISHABLE from omission, or the
			// guard makes forging exactly as effective as leaving the header off.
			if !slices.Contains(labels, "developer-forged") {
				t.Errorf("unattributed counter labels = %v, want a \"developer-forged\" "+
					"sample; this is the label an operator alerts on, because it is the "+
					"only one of the four that RAISES the sender's own score", labels)
			}
			if slices.Contains(labels, "developer") {
				t.Errorf("unattributed counter labels = %v; a forged header must not "+
					"also count as a plain missing one (double count)", labels)
			}
			// And it must not be mislabelled as the issue half.
			if slices.Contains(labels, "issue") || slices.Contains(labels, "issue-forged") {
				t.Errorf("unattributed counter labels = %v; the issue header was "+
					"legitimate and must produce no sample at all", labels)
			}
		})
	}
}

// TestProxy_MissingDeveloperStillCountsAsPlainUnattributed is the CONTROL ARM. Without
// it, every assertion above is satisfied by a proxy that labels EVERYTHING
// "developer-forged" — which would destroy the very signal the distinct label exists
// to provide, and make the long-standing #129 misconfiguration counter useless.
func TestProxy_MissingDeveloperStillCountsAsPlainUnattributed(t *testing.T) {
	ev, labels, status := proxyWithDeveloperHeader(t, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if ev.Developer != collector.UnattributedIssueID {
		t.Errorf("stored developer = %q, want %q", ev.Developer, collector.UnattributedIssueID)
	}
	if !slices.Contains(labels, "developer") {
		t.Errorf("labels = %v, want a plain \"developer\" sample for an ABSENT header", labels)
	}
	if slices.Contains(labels, "developer-forged") {
		t.Errorf("labels = %v; an absent header is not a forgery", labels)
	}
}

// TestProxy_OrdinaryDeveloperHeaderIsStoredVerbatim is the second control arm: the
// guard must reject only what RESEMBLES the sentinel. An identity that merely contains
// the word — a bot account, a hyphenated login — is an ordinary developer, and
// swallowing it would be a silent, permanent attribution outage for that person.
func TestProxy_OrdinaryDeveloperHeaderIsStoredVerbatim(t *testing.T) {
	for _, dev := range []string{
		"mallory", "unattributed-bot", "unattributedly", "not-unattributed",
		"xunattributed:main", "unknown",
	} {
		t.Run(dev, func(t *testing.T) {
			ev, labels, status := proxyWithDeveloperHeader(t, dev)
			if status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			if ev.Developer != dev {
				t.Errorf("stored developer = %q, want %q verbatim — this is an ordinary "+
					"identity and collapsing it into the sentinel loses a real person's "+
					"spend", ev.Developer, dev)
			}
			if slices.Contains(labels, "developer") || slices.Contains(labels, "developer-forged") {
				t.Errorf("labels = %v; an ordinary developer header must produce no "+
					"unattributed sample at all", labels)
			}
		})
	}
}
