package anthropicadmin

// Hermetic guards for the Metrics.PollComplete(ok) VALUE (#452/#451 review
// RED-1).
//
// WHY THESE EXIST. The live E2E (internal/integration) originally drove
// Poller.Run and asserted on its return value — which proves nothing, because
// Run deliberately logs and SWALLOWS a poll failure (see its doc: a provider
// outage must degrade the remainder feed, not kill serve). A revoked key
// therefore returned Run() == nil and the live test went green. The fix
// rewired that test onto this PollComplete(ok) hook, which IS the only signal
// distinguishing a successful pass from a failed one.
//
// But that fix was itself unguarded: flipping runPass's failure-path
// `p.recordPoll(false)` to `p.recordPoll(true)` compiled and left the whole
// suite green, plain and `-tags integration`, because NOTHING asserted the ok
// VALUE — the only non-production caller was the live test, which needs a real
// Admin key to run at all. These two subtests close that: they assert the
// recorded values, not merely that the hook fired, and they need no credential
// so they run under plain `make check`.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
)

// recordingMetrics captures the ok value of every PollComplete call and
// signals the first one, so a test can drive the real Run loop and stop it as
// soon as the first pass has been observed.
type recordingMetrics struct {
	mu    sync.Mutex
	oks   []bool
	fired chan struct{}
	once  sync.Once
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{fired: make(chan struct{})}
}

func (m *recordingMetrics) PollComplete(ok bool) {
	m.mu.Lock()
	m.oks = append(m.oks, ok)
	m.mu.Unlock()
	m.once.Do(func() { close(m.fired) })
}

func (m *recordingMetrics) EventsIngested(int)   {}
func (m *recordingMetrics) CostDeltasPosted(int) {}

func (m *recordingMetrics) values() []bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]bool(nil), m.oks...)
}

// runOnePassCollectingMetrics drives the REAL Poller.Run (not runPass or
// pollOnce) against srv until the first PollComplete lands, then cancels and
// waits for a clean shutdown. Going through Run is deliberate: it is the
// production entry point `tierd serve` uses, and it is the function whose
// error-swallowing is the reason PollComplete is the signal under test.
func runOnePassCollectingMetrics(t *testing.T, srv *httptest.Server) []bool {
	t.Helper()
	m := newRecordingMetrics()
	p := NewPoller(PollerConfig{
		Client:   newTestClient(t, srv, nil),
		Store:    newFakeStore(),
		Org:      "acme",
		Interval: time.Hour, // long enough that exactly one pass runs
		Metrics:  m,
		Now:      func() time.Time { return fixedNow },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, time.Time{}, &recordingIngester{}) }()

	select {
	case <-m.fired:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("PollComplete was never called within 30s — the poll pass is wedged")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on ctx cancellation", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return within 30s of ctx cancellation")
	}
	return m.values()
}

// wantPollOutcomes asserts the exact recorded ok sequence. Asserting the
// SEQUENCE (not "at least one false") is what makes the mutant-kill total: a
// value-blind assertion like `len(got) > 0` is precisely the hole this file
// exists to close.
func wantPollOutcomes(t *testing.T, got []bool, want ...bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("PollComplete called %d time(s) with %v, want exactly %d call(s) with %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PollComplete outcomes = %v, want %v (call %d differs)", got, want, i)
		}
	}
}

// TestRun_PollCompleteFalseOnRejectedCredential is the mutant-killer named in
// review RED-1: a 401 from the provider — the revoked/expired Admin key an
// operator actually hits — must be recorded as PollComplete(FALSE). Flipping
// runPass's `p.recordPoll(false)` to `true` fails here.
func TestRun_PollCompleteFalseOnRejectedCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	wantPollOutcomes(t, runOnePassCollectingMetrics(t, srv), false)
}

// TestRun_PollCompleteTrueOnSuccessfulPass is the CONTROL ARM. Without it the
// test above could be satisfied by a mutant that records false
// unconditionally — which would make the live E2E fail against a perfectly
// good key, the opposite false verdict.
func TestRun_PollCompleteTrueOnSuccessfulPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, usagePath):
			_, _ = w.Write(mustJSON(t, usageReport{}))
		case strings.HasPrefix(r.URL.Path, costPath):
			_, _ = w.Write(mustJSON(t, costReport{}))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	wantPollOutcomes(t, runOnePassCollectingMetrics(t, srv), true)
}

// compile-time proof that recordingMetrics satisfies the interface the poller
// actually consumes — so a Metrics change cannot leave this file testing a
// stale shape.
var _ Metrics = (*recordingMetrics)(nil)

// and that the ingester the harness passes is the real collector contract.
var _ collector.Ingester = (*recordingIngester)(nil)
