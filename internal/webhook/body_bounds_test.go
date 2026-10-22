package webhook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/store"
)

func boundedRequest(event string, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/github", body)
	r.Header.Set("X-GitHub-Event", event)
	return r
}

func TestWebhook_OversizedBodyIs413NotSignatureMismatch(t *testing.T) {
	for _, event := range []string{"pull_request", "workflow_run", "push", "unknown\nforged"} {
		limit := 1 << 20
		if event == "push" {
			limit = 25 << 20
		}
		for _, chunked := range []bool{false, true} {
			t.Run(event+map[bool]string{false: "/known", true: "/chunked"}[chunked], func(t *testing.T) {
				s := newFakeStore()
				h := New(s, testSecret, quietLogger())
				body := strings.Repeat(" ", limit+1)
				reader := &countedBody{Reader: strings.NewReader(body)}
				r := boundedRequest(event, reader)
				if chunked {
					r.ContentLength = -1
					r.TransferEncoding = []string{"chunked"}
				} else {
					r.ContentLength = int64(len(body))
				}
				// No signature: resource limits must run before authentication.
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 413 {
					t.Errorf("status = %d, want 413", w.Code)
				}
				if reader.n != limit+1 {
					t.Errorf("read %d bytes, want overflow probe at %d", reader.n, limit+1)
				}
				if len(s.payloads)+len(s.outcomes)+len(s.events) != 0 {
					t.Fatal("oversized delivery wrote state")
				}
			})
		}
	}
}

type countedBody struct {
	io.Reader
	n int
}

type spaceBody struct{}

func (spaceBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestWebhook_PushReadStopsAtCap(t *testing.T) {
	reader := &countedBody{Reader: spaceBody{}}
	r := boundedRequest("push", reader)
	// Even a false small Content-Length must not disarm the read cap.
	r.ContentLength = 1
	w := httptest.NewRecorder()
	New(newFakeStore(), testSecret, quietLogger()).ServeHTTP(w, r)
	if w.Code != 413 || reader.n != (25<<20)+1 {
		t.Fatalf("status=%d bytes=%d, want 413 and cap+1", w.Code, reader.n)
	}
}

func (r *countedBody) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += n
	return n, err
}

func TestWebhook_BodyBoundary(t *testing.T) {
	for _, event := range []string{"pull_request", "workflow_run", "push"} {
		limit := 1 << 20
		if event == "push" {
			limit = 25 << 20
		}
		for _, validSignature := range []bool{false, true} {
			s := newFakeStore()
			body := []byte("{}" + strings.Repeat(" ", limit-2))
			r := boundedRequest(event, bytes.NewReader(body))
			want := 403
			if validSignature {
				r.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
				want = 204
			}
			w := httptest.NewRecorder()
			New(s, testSecret, quietLogger()).ServeHTTP(w, r)
			if w.Code != want {
				t.Errorf("%s signed=%v: status=%d, want %d", event, validSignature, w.Code, want)
			}
			if validSignature && (len(s.payloads) != 1 || !bytes.Equal(s.payloads[0].Body, body)) {
				t.Errorf("%s: full at-limit body not persisted", event)
			}
			if !validSignature && len(s.payloads) != 0 {
				t.Fatal("bad signature persisted")
			}
		}
	}
}

// Pause just beyond the small-body threshold, while the large read owns a slot.
type pausedLargeBody struct {
	remaining int
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (r *pausedLargeBody) Read(p []byte) (int, error) {
	if r.remaining > 0 {
		n := min(len(p), r.remaining)
		clear(p[:n])
		r.remaining -= n
		return n, nil
	}
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return 0, io.EOF
}

func TestWebhook_LargeBodySemaphore(t *testing.T) {
	h := New(newFakeStore(), testSecret, quietLogger())
	first := &pausedLargeBody{remaining: (1 << 20) + 1, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), boundedRequest("push", first)) }()
	defer func() { close(first.release); <-done }()
	select {
	case <-first.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("large read never crossed threshold")
	}
	var competitors sync.WaitGroup
	for i := 0; i < 8; i++ {
		competitors.Add(1)
		go func() {
			defer competitors.Done()
			reader := &countedBody{Reader: io.LimitReader(spaceBody{}, (25<<20)+100)}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, boundedRequest("push", reader))
			if w.Code != 503 || w.Header().Get("Retry-After") == "" {
				t.Errorf("contended request: status=%d retry=%q", w.Code, w.Header().Get("Retry-After"))
			}
			if reader.n > (1<<20)+1 {
				t.Errorf("contended request read %d bytes", reader.n)
			}
		}()
	}
	competitors.Wait()
	// A small push can still authenticate while the slot is occupied.
	r := boundedRequest("push", strings.NewReader("{}"))
	r.Header.Set("X-Hub-Signature-256", sign(testSecret, []byte("{}")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Errorf("small request status=%d", w.Code)
	}
}

func TestWebhook_LargeBodyReadTimeoutDefault(t *testing.T) {
	if largeBodyReadTimeout != 10*time.Second {
		t.Fatalf("large-body read timeout = %v, want GitHub's 10s delivery timeout", largeBodyReadTimeout)
	}
}

func TestWebhook_LargeBodyTrickleDeadlineReleasesSlot(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked=%v", chunked), func(t *testing.T) {
			t.Parallel()
			h := New(newFakeStore(), testSecret, quietLogger())
			srv := httptest.NewUnstartedServer(h)
			srv.Config.ReadTimeout = 30 * time.Second // Same as tierd; must not be the bound.
			srv.Start()
			defer srv.Close()
			conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(largeBodyReadTimeout + 3*time.Second)); err != nil {
				t.Fatal(err)
			}
			framing := fmt.Sprintf("Content-Length: %d", smallBodyLimit+10000)
			if chunked {
				framing = "Transfer-Encoding: chunked"
			}
			prefix := strings.Repeat(" ", smallBodyLimit+1)
			if chunked {
				// Leave this chunk unfinished throughout the trickle.
				prefix = fmt.Sprintf("%x\r\n%s", smallBodyLimit+10000, prefix)
			}
			started := time.Now()
			// No signature: this request must time out before authentication.
			if _, err := fmt.Fprintf(conn, "POST /webhook/github HTTP/1.1\r\nHost: localhost\r\nX-GitHub-Event: push\r\n%s\r\n\r\n%s", framing, prefix); err != nil {
				t.Fatal(err)
			}
			stop := make(chan struct{})
			trickled := make(chan int, 1)
			go func() {
				ticker := time.NewTicker(50 * time.Millisecond)
				defer ticker.Stop()
				n := 0
				defer func() { trickled <- n }()
				for {
					select {
					case <-stop:
						return
					case <-ticker.C:
						if _, err := conn.Write([]byte(" ")); err != nil {
							return
						}
						n++
					}
				}
			}()
			defer func() {
				close(stop)
				if n := <-trickled; n < 2 {
					t.Errorf("sender trickled only %d bytes", n)
				}
			}()
			for len(h.largeBody) == 0 {
				if time.Since(started) > largeBodyReadTimeout {
					t.Fatal("sender never acquired the large-body slot")
				}
				time.Sleep(time.Millisecond)
			}
			client := srv.Client()
			client.Timeout = 2 * time.Second
			body := "{}" + strings.Repeat(" ", smallBodyLimit)
			deliver := func(want int) {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-GitHub-Event", "push")
				req.Header.Set("X-Hub-Signature-256", sign(testSecret, []byte(body)))
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != want {
					t.Fatalf("competing push status=%d, want %d", resp.StatusCode, want)
				}
			}
			deliver(http.StatusServiceUnavailable)
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("slow sender got no bounded response: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			got, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusBadRequest || string(got) != "read deadline expired\n" {
				t.Fatalf("slow sender: status=%d body=%q err=%v", resp.StatusCode, got, err)
			}
			if elapsed := time.Since(started); elapsed > largeBodyReadTimeout+2*time.Second {
				t.Fatalf("slow sender held slot for %v", elapsed)
			}
			deliver(http.StatusNoContent)
		})
	}
}

func TestWebhook_OversizedCounterAndSafeLog(t *testing.T) {
	var logs bytes.Buffer
	reg := metrics.NewRegistry()
	counter := reg.NewCounter("tier_webhook_oversized_total", "test", "event")
	h := New(newFakeStore(), testSecret, slog.New(slog.NewJSONHandler(&logs, nil)), WithOversizedCounter(counter))
	for _, event := range []string{"workflow_run", "unknown\nforged", "other-unknown"} {
		r := boundedRequest(event, strings.NewReader(strings.Repeat(" ", (1<<20)+1)))
		r.Header.Set("X-GitHub-Delivery", "delivery\nforged\x1b[31m")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 413 {
			t.Errorf("status=%d", w.Code)
		}
	}
	dec := json.NewDecoder(&logs)
	for i := 0; i < 3; i++ {
		var fields map[string]any
		if err := dec.Decode(&fields); err != nil {
			t.Fatal(err)
		}
		wantEvent := "other"
		if i == 0 {
			wantEvent = "workflow_run"
		}
		if fields["level"] != "ERROR" || fields["event"] != wantEvent || fields["size_class"] != ">1048576_bytes" {
			t.Errorf("unsafe or missing log attributes: %v", fields)
		}
		if delivery, ok := fields["delivery"].(string); !ok || strings.ContainsAny(delivery, "\n\r\x1b") {
			t.Errorf("unsafe delivery: %q", fields["delivery"])
		}
	}
	var out bytes.Buffer
	reg.Render(&out)
	for _, series := range []string{`tier_webhook_oversized_total{event="workflow_run"} 1`, `tier_webhook_oversized_total{event="other"} 2`} {
		if !strings.Contains(out.String(), "\n"+series+"\n") {
			t.Errorf("missing %s", series)
		}
	}
}

func TestWebhook_LargeReadReleasesSlotOnEveryExit(t *testing.T) {
	for _, failure := range []string{"overflow", "bad_signature", "read_error", "invalid_json", "success"} {
		t.Run(failure, func(t *testing.T) {
			h := New(newFakeStore(), testSecret, quietLogger())
			body := "{}" + strings.Repeat(" ", (1<<20)+10)
			want := 204
			if failure == "overflow" {
				body = strings.Repeat(" ", (25<<20)+1)
				want = 413
			}
			if failure == "bad_signature" {
				want = 403
			}
			if failure == "invalid_json" {
				body = "!" + body
				want = 500
			}
			r := boundedRequest("push", strings.NewReader(body))
			r.Header.Set("X-Hub-Signature-256", sign(testSecret, []byte(body)))
			if failure == "bad_signature" {
				r.Header.Del("X-Hub-Signature-256")
			}
			if failure == "read_error" {
				r.Body = io.NopCloser(io.MultiReader(strings.NewReader(body), failingBody{}))
				want = 400
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("status=%d want=%d", w.Code, want)
			}
			body = "{}" + strings.Repeat(" ", (1<<20)+10)
			r = boundedRequest("push", strings.NewReader(body))
			r.Header.Set("X-Hub-Signature-256", sign(testSecret, []byte(body)))
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 204 {
				t.Errorf("next large push status=%d; slot leaked", w.Code)
			}
		})
	}
}

type blockedAuditStore struct {
	*fakeStore
	entered chan struct{}
	release chan struct{}
}

func (s *blockedAuditStore) InsertWebhookPayload(ctx context.Context, event, delivery string, body []byte) error {
	close(s.entered)
	<-s.release
	return s.fakeStore.InsertWebhookPayload(ctx, event, delivery, body)
}

func TestWebhook_LargeBodySlotCoversPersistence(t *testing.T) {
	s := &blockedAuditStore{fakeStore: newFakeStore(), entered: make(chan struct{}), release: make(chan struct{})}
	h := New(s, testSecret, quietLogger())
	body := []byte("{}" + strings.Repeat(" ", (1<<20)+1))
	r := boundedRequest("push", bytes.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(first, r) }()
	defer func() { close(s.release); <-done }()
	select {
	case <-s.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("large body never reached persistence")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, boundedRequest("push", bytes.NewReader(body)))
	if w.Code != 503 {
		t.Errorf("large slot released before persistence completed: status=%d", w.Code)
	}
}

type deadlineAuditStore struct {
	*fakeStore
	delay    time.Duration
	observed chan error
}

func (s *deadlineAuditStore) InsertWebhookPayload(ctx context.Context, event, delivery string, body []byte) error {
	// Stay in processing past the read deadline, then observe the context while
	// the handler is still running (net/http cancels it after handler return).
	time.Sleep(s.delay)
	s.observed <- ctx.Err()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.fakeStore.InsertWebhookPayload(ctx, event, delivery, body)
}

func TestWebhook_LargeBodyDeadlineClearedBeforePersistence(t *testing.T) {
	previous := largeBodyReadTimeout
	largeBodyReadTimeout = 250 * time.Millisecond
	defer func() { largeBodyReadTimeout = previous }()
	s := &deadlineAuditStore{
		fakeStore: newFakeStore(), delay: 2 * largeBodyReadTimeout, observed: make(chan error, 1),
	}
	srv := httptest.NewServer(New(s, testSecret, quietLogger()))
	defer srv.Close()
	// The one-byte probe consumes the last Content-Length byte and reaches EOF
	// BEFORE SetReadDeadline, starting net/http's background connection read.
	body := []byte("{}" + strings.Repeat(" ", smallBodyLimit-1))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.ContentLength != smallBodyLimit+1 {
		t.Fatalf("Content-Length = %d, want %d", req.ContentLength, smallBodyLimit+1)
	}
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	client := srv.Client()
	client.Timeout = 3 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	select {
	case err := <-s.observed:
		if err != nil {
			t.Errorf("audit store context after %v (read timeout %v): %v", s.delay, largeBodyReadTimeout, err)
		} else {
			t.Logf("audit store context after %v (read timeout %v): active; status=%d", s.delay, largeBodyReadTimeout, resp.StatusCode)
		}
	default:
		t.Fatal("signed push never reached audit persistence")
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("injected read error") }

func TestWebhook_LargePushWithinCapProcessesRevert(t *testing.T) {
	r := newReconcileHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	_, err := r.db.RecordPROutcome(context.Background(), store.Outcome{Developer: "alice", IssueID: "issue-42", Repo: reconcileRepo, PRNumber: 7, Weight: 1, Quality: 1, MergeCommitSHA: wfMergeSHA, Timestamp: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// Padding is an ignored JSON field; the revert follows it in the same body.
	r.deliver(t, "push", map[string]any{"padding": strings.Repeat("x", 3<<20), "ref": "refs/heads/main", "repository": map[string]any{"default_branch": "main", "full_name": reconcileRepo}, "commits": []any{pushCommit("revert-sha", "Revert \"feat\"\n\nThis reverts commit "+wfMergeSHA+".", "bob", now)}})
	o, found, err := r.db.OutcomeByMergeCommit(context.Background(), wfMergeSHA)
	if err != nil || !found || o.Quality != 0.1 {
		t.Fatalf("quality=%v found=%v err=%v, want 0.1", o.Quality, found, err)
	}
}

func TestWebhook_LargePushStoredReaderCompatibility(t *testing.T) {
	r := newReconcileHarness(t)
	body := []byte("{}" + strings.Repeat(" ", (3<<20)-2))
	req := boundedRequest("push", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	req.Header.Set("X-GitHub-Delivery", "large-reader")
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Errorf("status=%d, want 204", w.Code)
	}
	got, found, err := r.db.WebhookPayloadByDelivery(context.Background(), "push", "large-reader")
	if err != nil || !found || !bytes.Equal(got, body) {
		t.Fatalf("stored body round trip: found=%v len=%d err=%v", found, len(got), err)
	}
}

func TestWebhook_StalledOneByteBodyAllocation(t *testing.T) {
	const requests = 16
	h := New(newFakeStore(), testSecret, quietLogger())
	entered := make(chan struct{}, requests)
	release := make(chan struct{})
	var done sync.WaitGroup
	defer func() { close(release); done.Wait() }()
	runtime.GC()
	var before, stalled runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < requests; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			body := io.MultiReader(strings.NewReader(" "), stalledBody{entered, release})
			h.ServeHTTP(httptest.NewRecorder(), boundedRequest("push", body))
		}()
	}
	for i := 0; i < requests; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("one-byte request did not stall")
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&stalled)
	held := int64(stalled.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("%d stalled one-byte requests retained %d bytes", requests, held)
	if held >= requests*smallBodyLimit/2 {
		t.Fatalf("stalled one-byte requests retained %d bytes; eager 1 MiB buffers", held)
	}
}

type stalledBody struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (r stalledBody) Read([]byte) (int, error) {
	r.entered <- struct{}{}
	<-r.release
	return 0, io.EOF
}

func TestWebhook_TruncatedBodyIs400(t *testing.T) {
	for _, size := range []int{2, smallBodyLimit, smallBodyLimit + 1, smallBodyLimit + 2} {
		for _, withData := range []bool{false, true} {
			for _, signed := range []bool{false, true} {
				body := "{}" + strings.Repeat(" ", size-2)
				reader := io.MultiReader(strings.NewReader(body), truncatedBody{strings.NewReader("")})
				if withData {
					reader = truncatedBody{strings.NewReader(body)}
				}
				s := newFakeStore()
				r := boundedRequest("push", reader)
				if signed {
					r.Header.Set("X-Hub-Signature-256", sign(testSecret, []byte(body)))
				}
				w := httptest.NewRecorder()
				New(s, testSecret, quietLogger()).ServeHTTP(w, r)
				if w.Code != 400 {
					t.Errorf("size=%d error_with_data=%v signed=%v: status=%d, want 400", size, withData, signed, w.Code)
				}
				if len(s.payloads)+len(s.outcomes)+len(s.events) != 0 {
					t.Errorf("size=%d error_with_data=%v signed=%v: truncated delivery wrote state", size, withData, signed)
				}
			}
		}
	}
}

type truncatedBody struct{ *strings.Reader }

func (r truncatedBody) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if r.Len() == 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func TestWebhook_LargeBodyBusyWarnsSafely(t *testing.T) {
	for _, event := range []string{"push", "unknown\nforged"} {
		var logs bytes.Buffer
		h := New(newFakeStore(), testSecret, slog.New(slog.NewJSONHandler(&logs, nil)))
		h.largeBody <- struct{}{}
		r := boundedRequest(event, io.LimitReader(spaceBody{}, smallBodyLimit+1))
		r.Header.Set("X-GitHub-Delivery", "delivery\nforged\x1b[31m")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if event != "push" {
			// Non-push headers cannot select the large-body path.
			if w.Code != 413 {
				t.Errorf("unknown event status=%d, want 413", w.Code)
			}
			continue
		}
		if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
			t.Fatalf("status=%d retry=%q", w.Code, w.Header().Get("Retry-After"))
		}
		var fields map[string]any
		if err := json.Unmarshal(logs.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		if fields["level"] != "WARN" || fields["event"] != "push" {
			t.Errorf("missing busy warning attributes: %v", fields)
		}
		if delivery, ok := fields["delivery"].(string); !ok || delivery == "" || strings.ContainsAny(delivery, "\n\r\x1b") {
			t.Errorf("unsafe or missing delivery: %q", fields["delivery"])
		}
		if msg, ok := fields["msg"].(string); !ok || !strings.Contains(strings.ToLower(msg), "redeliver") {
			t.Errorf("missing recovery instruction: %v", fields["msg"])
		}
	}
}

type bodyReadFailure struct{ err error }

func (r bodyReadFailure) Read([]byte) (int, error) { return 0, r.err }

func TestWebhook_LargeBodyDeadlineWarnsSafely(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		warn bool
	}{
		{"deadline", fmt.Errorf("read body: %w", os.ErrDeadlineExceeded), true},
		{"net_timeout", &net.DNSError{Err: "read timed out", IsTimeout: true}, true},
		{"other_read_error", io.ErrUnexpectedEOF, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := New(newFakeStore(), testSecret, slog.New(slog.NewJSONHandler(&logs, nil)))
			body := io.MultiReader(io.LimitReader(spaceBody{}, smallBodyLimit+1), bodyReadFailure{tc.err})
			r := boundedRequest("push", body)
			r.Header.Set("X-GitHub-Delivery", "delivery\nforged\x1b[31m")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			wantMessage := "read error\n"
			if tc.warn {
				wantMessage = "read deadline expired\n"
			}
			if w.Code != http.StatusBadRequest || w.Body.String() != wantMessage {
				t.Errorf("status=%d body=%q, want 400 %q", w.Code, w.Body.String(), wantMessage)
			}
			if !tc.warn {
				if strings.Contains(logs.String(), "deadline expired") {
					t.Errorf("non-timeout read error logged deadline warning: %s", &logs)
				}
				return
			}
			var fields map[string]any
			if err := json.Unmarshal(logs.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if fields["level"] != "WARN" || fields["event"] != "push" {
				t.Errorf("missing deadline warning attributes: %v", fields)
			}
			if delivery, ok := fields["delivery"].(string); !ok || delivery != logSafeStr(r.Header.Get("X-GitHub-Delivery")) || strings.ContainsAny(delivery, "\n\r\x1b") {
				t.Errorf("unsafe or missing delivery: %q", fields["delivery"])
			}
			if msg, ok := fields["msg"].(string); !ok || !strings.Contains(msg, "manually redeliver this delivery") {
				t.Errorf("missing recovery instruction: %v", fields["msg"])
			}
		})
	}
}

func TestWebhook_DefaultReadErrorLogsSafely(t *testing.T) {
	for _, event := range []string{"push", "unknown\nforged"} {
		t.Run(event, func(t *testing.T) {
			var logs bytes.Buffer
			h := New(newFakeStore(), testSecret, slog.New(slog.NewJSONHandler(&logs, nil)))
			readErr := errors.New("connection failed\nforged\r\x1b[31m")
			r := boundedRequest(event, bodyReadFailure{readErr})
			r.Header.Set("X-GitHub-Delivery", "delivery\nforged\x1b[31m")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || w.Body.String() != "read error\n" {
				t.Errorf("status=%d body=%q, want 400 read error", w.Code, w.Body.String())
			}
			var fields map[string]any
			if err := json.Unmarshal(logs.Bytes(), &fields); err != nil {
				t.Fatalf("expected one read-error log: %v; logs=%q", err, &logs)
			}
			if fields["level"] != "WARN" {
				t.Errorf("read-error level=%v, want WARN", fields["level"])
			}
			if fields["event"] != eventLabel(event) || fields["delivery"] != logSafeStr(r.Header.Get("X-GitHub-Delivery")) || fields["err"] != logsafe.Err(readErr) {
				t.Errorf("missing or unsafe read-error attributes: %v", fields)
			}
			if msg, ok := fields["msg"].(string); !ok || !strings.Contains(msg, "webhook body read error") || !strings.Contains(msg, "headers are unauthenticated") {
				t.Errorf("missing read-error diagnostic: %v", fields["msg"])
			}
		})
	}
}

type writerWithReadDeadlineError struct {
	http.ResponseWriter
	err error
}

func (w writerWithReadDeadlineError) SetReadDeadline(time.Time) error { return w.err }

func TestWebhook_SetReadDeadlineFailureIdentified(t *testing.T) {
	var logs bytes.Buffer
	h := New(newFakeStore(), testSecret, slog.New(slog.NewJSONHandler(&logs, nil)))
	deadlineErr := errors.New("injected deadline setup failure")
	w := httptest.NewRecorder()
	writer := writerWithReadDeadlineError{w, deadlineErr}
	r := boundedRequest("push", io.LimitReader(spaceBody{}, smallBodyLimit+1))
	_, release, err := h.readBody(writer, r, "push")
	if release != nil {
		release()
	}
	if !errors.Is(err, deadlineErr) || !strings.Contains(err.Error(), "set webhook body read deadline") {
		t.Errorf("readBody error=%v, want wrapped deadline setup failure", err)
	}
	h.ServeHTTP(writer, boundedRequest("push", io.LimitReader(spaceBody{}, smallBodyLimit+1)))
	if w.Code != http.StatusBadRequest || w.Body.String() != "read error\n" {
		t.Errorf("status=%d body=%q, want 400 read error", w.Code, w.Body.String())
	}
	if !strings.Contains(logs.String(), "set webhook body read deadline") || !strings.Contains(logs.String(), deadlineErr.Error()) {
		t.Errorf("missing deadline setup diagnostic: %s", &logs)
	}
}

// Hide deadline support, as middleware without Unwrap can do on a real server.
type writerWithoutReadDeadline struct{ http.ResponseWriter }

func TestWebhook_LargeBodyDeadlineUnsupportedWarnsOnce(t *testing.T) {
	largeBodyDeadlineUnsupported = sync.Once{}
	defer func() { largeBodyDeadlineUnsupported = sync.Once{} }()
	var logs bytes.Buffer
	h := New(newFakeStore(), testSecret, slog.New(slog.NewJSONHandler(&logs, nil)))
	prefix := `{"ref":"refs/heads/main","repository":{"default_branch":"main"}}`
	body := []byte(prefix + strings.Repeat(" ", smallBodyLimit+1-len(prefix)))
	request := func(url string) *http.Request {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("X-GitHub-Event", "push")
		r.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
		return r
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request("http://example.com/webhook/github"))
	if w.Code != http.StatusNoContent || logs.Len() != 0 {
		t.Fatalf("recorder: status=%d logs=%q, want 204 without warning", w.Code, &logs)
	}
	// Wait for each handler to return before reading its log buffer or resetting
	// the process-wide once; response arrival alone need not imply that return.
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(writerWithoutReadDeadline{w}, r)
		done <- struct{}{}
	}))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		resp, err := srv.Client().Do(request(srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		<-done
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("unsupported server writer: status=%d, want 204", resp.StatusCode)
		}
	}
	dec := json.NewDecoder(&logs)
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	if msg, ok := fields["msg"].(string); fields["level"] != "WARN" || !ok || !strings.Contains(msg, "large webhook body read bound is inactive") {
		t.Errorf("missing unsupported deadline warning: %v", fields)
	}
	if err := dec.Decode(&fields); !errors.Is(err, io.EOF) {
		t.Errorf("expected exactly one warning, additional log=%v err=%v", fields, err)
	}
}
