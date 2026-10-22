package webhook

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

const smallBodyLimit = 1 << 20

// largeBodyReadTimeout bounds ONE sender's hold of the scarce slot during body
// reads, not time between bytes. Its 10s budget matches GitHub's delivery timeout:
// a max-size 25 MiB body requires ~2.4 MiB/s after the 1 MiB prefix.
// Compared with the server's 30s ReadTimeout, 10s makes a relay of new connections
// ~3x costlier but does not close that pre-existing attack; a GitHub-IP allowlist
// or per-source cap is a separate mechanism. Tests may shorten this timeout.
var largeBodyReadTimeout = 10 * time.Second

var errLargeBodyBusy = errors.New("large webhook reader busy")

var largeBodyDeadlineUnsupported sync.Once

// readBody enforces limits before HMAC verification, independent of the claimed
// Content-Length. Only the unauthenticated exact "push" header selects 25 MiB.
// One probe byte distinguishes an at-limit body from overflow. The slot covers
// the large allocation and all later processing; the caller must release it even
// on read/signature/decode errors. There is only one full-size body buffer: the
// small prefix is copied into it, never a second full-body read or verification
// buffer. MaxBytesReader reads at most limit+1 bytes from the wire.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, event string) ([]byte, func(), error) {
	limit := int64(smallBodyLimit)
	if event == "push" {
		limit = store.MaxWebhookBodyBytes
	}
	// MaxBytesReader needs the server writer to disable draining on overflow.
	for {
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = wrapper.Unwrap()
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(io.LimitReader(r.Body, smallBodyLimit))
	if err != nil || len(body) < smallBodyLimit {
		return body, nil, err
	}
	probe, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(probe) == 0 {
		return body, nil, err
	}
	select {
	case h.largeBody <- struct{}{}:
	default:
		return nil, nil, errLargeBodyBusy
	}
	release := func() { <-h.largeBody }
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(largeBodyReadTimeout)); err != nil {
		if !errors.Is(err, http.ErrNotSupported) {
			return nil, release, fmt.Errorf("set webhook body read deadline: %w", err)
		}
		// A real net/http server supplies LocalAddrContextKey; in-memory
		// ResponseRecorders do not and are allowed to lack deadline support.
		if r.Context().Value(http.LocalAddrContextKey) != nil {
			largeBodyDeadlineUnsupported.Do(func() {
				h.logger.Warn("large webhook body read bound is inactive: response writer does not support read deadlines")
			})
		}
	}
	// Set this once: trickling bytes must not renew the slot's lease.
	large := make([]byte, limit+1)
	copy(large, body)
	large[smallBodyLimit] = probe[0]
	n := smallBodyLimit + 1
	for err == nil {
		var read int
		read, err = r.Body.Read(large[n:])
		n += read
	}
	err = bodyReadError(err)
	if err == nil {
		// With Content-Length == smallBodyLimit+1, the probe reaches EOF
		// before the deadline is set, so it lands on net/http's background
		// read. Clear it to prevent request-context cancellation during
		// processing. On error leave it armed so draining cannot stall.
		_ = controller.SetReadDeadline(time.Time{})
	}
	return large[:n], release, err
}

func bodyReadError(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
