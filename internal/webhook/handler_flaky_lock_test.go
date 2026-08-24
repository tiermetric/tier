package webhook

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/quality"
	"github.com/tiermetric/tier/internal/store"
)

// probeWindow is how long an instrumented store call holds open while watching
// for another delivery's operation. It must be long enough that a second
// goroutine which is NOT blocked would comfortably get there.
//
// ⚠️ This is not a "wait and hope" test in the direction that matters. Under the
// fix the second delivery is blocked on qualityMu for the WHOLE window, so
// "nothing observed" is structural rather than probable — a slower machine makes
// these tests MORE certain, never flakier. Measured clean at `-race -count=20`
// and at GOMAXPROCS=1. The window only needs to be generous in the BUGGY
// direction, and there the probe closes its channel in microseconds.
const probeWindow = 250 * time.Millisecond

// lockProbeStore wraps fakeStore to observe the ORDER of the two operations
// #674 is about: the classification read, and the append it feeds.
//
// 🔴 TWO PROBES, IN BOTH DIRECTIONS, AND THE SECOND ONE IS NOT REDUNDANT.
// A read-side probe alone is much weaker than it looks: it blocks while HOLDING
// whatever lock the classification acquired, so every other delivery is parked
// at its own Lock() and cannot reach the append no matter what. "No append
// observed" is then produced identically by correct code and by wrong code.
// Measured: with the classification moved under a SEPARATE mutex — two
// deliveries can still classify against the same pre-append state — a read-only
// probe caught it 0 times in 5, and the whole package stayed green.
//
// The APPEND-side probe is what closes that: hold the first append open and
// watch for another delivery's classification read. Under the fix the other
// delivery is blocked at qualityMu before it reads, so nothing lands; under a
// separate-mutex classification its read runs freely and is caught.
//
// ⛔ AND ONE SHAPE NO STORE-LEVEL PROBE CAN SEE, stated because a reader will
// otherwise trust this guard further than the measurement supports:
//
//	h.qualityMu.Lock(); flaky := classify(); h.qualityMu.Unlock()
//	h.qualityMu.Lock(); defer h.qualityMu.Unlock(); append(flaky)
//
// — #674 respelled as a TOCTOU inside one function. The store is never called
// in the unlock/relock gap, and Go hands the mutex straight back to the
// re-acquiring goroutine, so the operation trace is contiguous and every probe
// stays silent. ⇒ ONLY STRUCTURE DEFENDS AGAINST IT: one function, one
// acquisition, the ...Locked naming contract. If you split those functions,
// no test here will tell you.
type lockProbeStore struct {
	*fakeStore

	reads   atomic.Int64
	appends atomic.Int64

	// --- read-side probe: is anyone appending while we classify? -------------
	classifying          atomic.Bool
	appendDuringClassify atomic.Bool
	appendSeen           chan struct{}
	appendOnce           sync.Once

	// --- append-side probe: is anyone classifying while we append? -----------
	appending        atomic.Bool
	readDuringAppend atomic.Bool
	readSeen         chan struct{}
	readOnce         sync.Once
	firstReadDelay   time.Duration // widens the window the bug needs, if set
	readErr          error         // injected failure for the classification read
}

func newLockProbeStore() *lockProbeStore {
	return &lockProbeStore{
		fakeStore:  newFakeStore(),
		appendSeen: make(chan struct{}),
		readSeen:   make(chan struct{}),
	}
}

func (s *lockProbeStore) QualityEventsForOutcome(ctx context.Context, outcomeID int64) ([]store.QualityEvent, error) {
	n := s.reads.Add(1)

	// Report a classification read to the append-side probe BEFORE doing any
	// work, so a read that races an in-flight append is observed even if the
	// read itself is fast.
	if s.appending.Load() {
		s.readDuringAppend.Store(true)
		s.readOnce.Do(func() { close(s.readSeen) })
	}
	if s.readErr != nil {
		return nil, s.readErr
	}

	// Arm BEFORE delegating, not after: arming afterwards leaves a window in
	// which a buggy interleave lands unobserved (a false PASS on buggy code —
	// never a false FAIL on fixed code, since the other delivery is blocked).
	first := n == 1
	if first {
		s.classifying.Store(true)
	}
	if s.firstReadDelay > 0 && first {
		time.Sleep(s.firstReadDelay)
	}
	out, err := s.fakeStore.QualityEventsForOutcome(ctx, outcomeID)
	if !first {
		return out, err
	}

	select {
	case <-s.appendSeen:
		// Another delivery appended while this classification was in flight.
	case <-time.After(probeWindow):
	}
	s.classifying.Store(false)
	return out, err
}

func (s *lockProbeStore) AppendQualityEvent(ctx context.Context, e store.QualityEvent) (bool, error) {
	n := s.appends.Add(1)
	if s.classifying.Load() {
		s.appendDuringClassify.Store(true)
		s.appendOnce.Do(func() { close(s.appendSeen) })
	}

	ok, err := s.fakeStore.AppendQualityEvent(ctx, e)
	if n != 1 {
		return ok, err
	}
	// Hold the FIRST append open and watch for another delivery's classify.
	s.appending.Store(true)
	select {
	case <-s.readSeen:
	case <-time.After(probeWindow):
	}
	s.appending.Store(false)
	return ok, err
}

// TestCIClassificationIsSerialisedWithTheAppend is the #674 regression guard.
//
// The ci_pass vs ci_fail_flaky decision reads the outcome's event set, and the
// append that immediately follows mutates it. Before the fix the read happened
// in the caller OUTSIDE qualityMu while the append took the lock, so two
// concurrent same-outcome deliveries could both classify against the same
// pre-append state — and appendAndResolve's doc promised an atomicity the code
// did not provide.
//
// ⚠️ WHAT THIS TEST DOES AND DOES NOT PROVE. It proves the classification and
// the append are not interleavable by another delivery THROUGH THE STORE. It
// does NOT prove they are one lock acquisition — see the TOCTOU note on
// lockProbeStore, which no store-level probe can reach. It also does not check
// the classification ANSWER; that is
// TestConcurrentFailureAndSuccessStaySerialisable below, which is the pair that
// actually carries the damage.
func TestCIClassificationIsSerialisedWithTheAppend(t *testing.T) {
	st := newLockProbeStore()
	seedMergedOutcome(t, st.fakeStore)
	h := newTestHandler(st)

	// A prior failure on the same merge commit, recent enough that a success is
	// classified as a flaky re-run. This is what makes the classification READ
	// load-bearing: with no ci_fail to find, the decision is trivial.
	now := time.Now().UTC()
	seedQualityEvent(t, st.fakeStore, quality.EventCIFail, wfMergeSHA+":1", now.Add(-time.Minute))

	// Two concurrent success deliveries for the SAME outcome, distinct attempts
	// so neither is swallowed by the (outcome_id, event_type, source_ref) key.
	//
	// ⚠️ DISTINCT X-GitHub-Delivery IDs ARE LOAD-BEARING, not cosmetic. The
	// handler keeps a deliverySet and skips a repeated GUID, so sending both
	// with one ID makes the second a no-op — and the test then reports "no
	// interleave" because only ONE delivery ever ran. The first draft of this
	// test shared an ID and was caught by the append-count control below
	// (`appends = 1, want 2`) rather than silently going green.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = doWorkflowRun(t, h,
				wfRunPayload(wfMergeSHA, "main", "success", i+2, now),
				"wf-concurrent-"+strconv.Itoa(i))
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusNoContent {
			t.Errorf("delivery %d: status = %d, want 204", i, c)
		}
	}

	if st.appendDuringClassify.Load() {
		t.Error("another delivery APPENDED during a classification read: the " +
			"ci_pass/ci_fail_flaky decision is not inside qualityMu (#674)")
	}
	if st.readDuringAppend.Load() {
		t.Error("another delivery CLASSIFIED during an append: the classification " +
			"is under some other lock, not the one that protects the append (#674)")
	}

	// --- controls. Without these the test could pass because nothing ran, ----
	// --- which is the shape this repo's false-green ledger is full of. -------

	// EXACTLY four: two classification reads + two post-append reads. `>= 1`
	// was vacuous and actively harmful: if the classification is ever removed,
	// read #1 becomes a POST-APPEND read, the probe arms on the wrong call, and
	// the guard reports success for code that never classifies at all. This
	// also pins appendAndResolveCISuccess's "two reads, two different
	// questions" claim against a future 'optimisation' of the pre-append read.
	if got := st.reads.Load(); got != 4 {
		t.Errorf("reads = %d, want exactly 4 (2 classify + 2 post-append) — "+
			"a different count means the probe armed on a read it was not written for", got)
	}
	if got := st.appends.Load(); got != 2 {
		t.Errorf("appends = %d, want 2 — both deliveries must reach the append, "+
			"or the absence of an interleave proves nothing", got)
	}
	// appends counts CALLS, not inserts. Assert the stored state too, or a
	// future edit that collides both deliveries on the unique key would keep
	// appends == 2 while silently halving what the test exercises.
	if got := len(st.events); got != 3 {
		t.Errorf("stored events = %d, want 3 (seeded ci_fail + two successes)", got)
	}
	for _, ref := range []string{wfMergeSHA + ":2", wfMergeSHA + ":3"} {
		if got := eventTypeFor(st.fakeStore, ref); got != quality.EventCIFailFlaky {
			t.Errorf("event %s = %q, want %q — both successes follow the seeded "+
				"ci_fail inside the window", ref, got, quality.EventCIFailFlaky)
		}
	}
}

// TestConcurrentFailureAndSuccessStaySerialisable is the pair that carries the
// actual damage, and the one the ordering probe above cannot express.
//
// Two concurrent SUCCESSES can never mis-classify each other — isFlakyRerunLocked
// matches only ci_fail events and neither delivery appends one, so the final
// state is byte-identical with and without the fix. A concurrent FAILURE and
// SUCCESS is different: pre-fix, the success could classify before the failure's
// append but write after it, recording a genuine flake as ci_pass and stranding
// the outcome at 0.7 forever.
//
// The order of two concurrent deliveries is legitimately nondeterministic, so
// this asserts SERIALISABILITY rather than a fixed answer: whatever order the
// events land in, the success's type must be consistent with that order.
//
//	ci_fail stored BEFORE the success  =>  success must be ci_fail_flaky
//	ci_fail stored AFTER  the success  =>  success must be ci_pass
//
// Either is a legal outcome of a legal interleaving. A classification that
// disagrees with the stored order is not.
func TestConcurrentFailureAndSuccessStaySerialisable(t *testing.T) {
	st := newLockProbeStore()
	// Widen the window the bug needs: the first classification read is held
	// open, giving the other delivery time to append underneath it. Under the
	// fix that delivery is blocked on qualityMu and the delay changes nothing.
	st.firstReadDelay = 100 * time.Millisecond
	seedMergedOutcome(t, st.fakeStore)
	h := newTestHandler(st)

	now := time.Now().UTC()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "failure", 1, now), "wf-fail")
	}()
	go func() {
		defer wg.Done()
		doWorkflowRun(t, h, wfRunPayload(wfMergeSHA, "main", "success", 2, now), "wf-succ")
	}()
	wg.Wait()

	failIdx, succIdx, succType := -1, -1, ""
	for i, e := range st.events {
		switch {
		case e.EventType == quality.EventCIFail:
			failIdx = i
		case e.SourceRef == wfMergeSHA+":2":
			succIdx, succType = i, e.EventType
		}
	}
	if failIdx < 0 || succIdx < 0 {
		t.Fatalf("both deliveries must have stored an event; got %+v", st.events)
	}

	want := quality.EventCIPass
	if failIdx < succIdx {
		want = quality.EventCIFailFlaky
	}
	if succType != want {
		t.Errorf("ci_fail at index %d, success at index %d, success recorded as %q — "+
			"want %q. The classification disagrees with the order the events were "+
			"stored in, which means it was decided against a snapshot the append "+
			"then invalidated (#674).", failIdx, succIdx, succType, want)
	}
}

// TestFlakyClassificationRules pins the classification BEHAVIOUR across the #674
// refactor. Moving a decision into a critical section must not change the
// decision — and three of these arms are the filters that had no coverage at
// all: dropping the `delta >= 0` check, or the head-SHA match, left the whole
// package green.
func TestFlakyClassificationRules(t *testing.T) {
	const otherSHA = "fedcba9876543210fedcba9876543210fedcba98"
	for _, tc := range []struct {
		name    string
		failRef string
		failAgo time.Duration // negative = failure AFTER the success (out of order)
		want    string
	}{
		{"success shortly after a failure is a flaky re-run", wfMergeSHA + ":1", time.Minute, quality.EventCIFailFlaky},
		{"success long after a failure is a clean pass", wfMergeSHA + ":1", flakyRerunWindow + time.Minute, quality.EventCIPass},
		// pins `delta >= 0`: a success timestamped BEFORE the failure is not a
		// re-run of it, however close together they are.
		{"success BEFORE the failure is not a re-run", wfMergeSHA + ":1", -time.Minute, quality.EventCIPass},
		// pins the head-SHA filter: another commit's failure must not
		// neutralise this outcome.
		{"failure on a DIFFERENT commit does not make this flaky", otherSHA + ":1", time.Minute, quality.EventCIPass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			seedMergedOutcome(t, st)
			h := newTestHandler(st)

			now := time.Now().UTC()
			seedQualityEvent(t, st, quality.EventCIFail, tc.failRef, now.Add(-tc.failAgo))

			if code := doWorkflowRun(t, h,
				wfRunPayload(wfMergeSHA, "main", "success", 2, now), "wf-classify"); code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", code)
			}
			if got := eventTypeFor(st, wfMergeSHA+":2"); got != tc.want {
				t.Errorf("appended event type = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassificationReadErrorAbortsTheDelivery pins the error path added by
// #674's restructure: a store failure during classification must abort, not
// silently fall through to "not flaky". Otherwise a transient read error
// downgrades a genuine flake to a permanent 0.7.
func TestClassificationReadErrorAbortsTheDelivery(t *testing.T) {
	st := newLockProbeStore()
	seedMergedOutcome(t, st.fakeStore)
	h := newTestHandler(st)

	now := time.Now().UTC()
	seedQualityEvent(t, st.fakeStore, quality.EventCIFail, wfMergeSHA+":1", now.Add(-time.Minute))
	st.readErr = errors.New("injected read failure")

	if code := doWorkflowRun(t, h,
		wfRunPayload(wfMergeSHA, "main", "success", 2, now), "wf-readerr"); code == http.StatusNoContent {
		t.Error("status = 204 on a failed classification read: the delivery was " +
			"accepted, so GitHub will not redeliver and the flake is lost")
	}
	// The decisive assertion: nothing was written. A fallthrough would have
	// appended ci_pass and stranded the outcome at 0.7.
	if got := len(st.events); got != 1 {
		t.Errorf("stored events = %d, want 1 (only the seeded ci_fail) — the "+
			"delivery appended despite failing to classify", got)
	}
}

// seedQualityEvent inserts an event directly, bypassing any probe wrapper so
// setup is never mistaken for a delivery.
func seedQualityEvent(t *testing.T, st *fakeStore, eventType, sourceRef string, ts time.Time) {
	t.Helper()
	if _, err := st.AppendQualityEvent(context.Background(), store.QualityEvent{
		OutcomeID: 1, Developer: "alice", IssueID: "issue-42",
		EventType: eventType, SourceRef: sourceRef, EventTS: ts,
	}); err != nil {
		t.Fatalf("seed %s: %v", eventType, err)
	}
}

// eventTypeFor returns the stored event type for a source_ref, or "" if absent.
func eventTypeFor(st *fakeStore, sourceRef string) string {
	for _, e := range st.events {
		if e.SourceRef == sourceRef {
			return e.EventType
		}
	}
	return ""
}
