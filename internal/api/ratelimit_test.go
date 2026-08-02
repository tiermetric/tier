package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"
)

// mustPrefixes parses each CIDR string into a netip.Prefix for table tests,
// failing the test on any malformed entry (test authoring bug, not a runtime
// path).
func mustPrefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	if len(cidrs) == 0 {
		return nil
	}
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("mustPrefixes: bad CIDR %q: %v", c, err)
		}
		out = append(out, p)
	}
	return out
}

// TestClientIP exercises the rate-limit key resolution. The default (no trusted
// prefixes) path must ignore X-Forwarded-For entirely and behave byte-identical
// to the pre-#131 code; the trusted-peer path walks XFF right-to-left and keys
// on the rightmost address that is NOT itself a trusted hop.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string // "" means no X-Forwarded-For header
		trusted    []string
		want       string
	}{
		{
			name:       "default_ignores_xff",
			remoteAddr: "203.0.113.50:9999",
			xff:        "1.1.1.1, 2.2.2.2",
			trusted:    nil,
			want:       "203.0.113.50",
		},
		{
			name:       "trusted_peer_uses_rightmost_untrusted",
			remoteAddr: "10.0.0.5:443",
			xff:        "203.0.113.7, 10.0.0.9",
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.7",
		},
		{
			name:       "untrusted_peer_ignores_xff",
			remoteAddr: "198.51.100.2:1",
			xff:        "203.0.113.7, 10.0.0.9",
			trusted:    []string{"10.0.0.0/8"},
			want:       "198.51.100.2",
		},
		{
			name:       "all_hops_trusted_falls_back_to_peer",
			remoteAddr: "10.0.0.5:443",
			xff:        "10.0.0.9, 10.0.0.8",
			trusted:    []string{"10.0.0.0/8"},
			want:       "10.0.0.5",
		},
		{
			name:       "malformed_xff_entry_falls_back",
			remoteAddr: "10.0.0.5:443",
			xff:        "garbage, 10.0.0.9",
			trusted:    []string{"10.0.0.0/8"},
			want:       "10.0.0.5",
		},
		{
			name:       "absent_xff_trusted_peer",
			remoteAddr: "10.0.0.5:443",
			xff:        "",
			trusted:    []string{"10.0.0.0/8"},
			want:       "10.0.0.5",
		},
		{
			name:       "ipv4_mapped_ipv6_peer_matches_v4_cidr",
			remoteAddr: "[::ffff:10.0.0.5]:443",
			xff:        "203.0.113.7, 10.0.0.9",
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.7",
		},
		{
			name:       "ipv6_zone_stripped",
			remoteAddr: "[fe80::1%eth0]:443",
			xff:        "203.0.113.7",
			trusted:    []string{"fe80::/10"},
			want:       "203.0.113.7",
		},
		{
			name:       "remoteaddr_without_port",
			remoteAddr: "203.0.113.50", // no port — preserved verbatim
			xff:        "1.1.1.1",
			trusted:    nil,
			want:       "203.0.113.50",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/scores", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			got := clientIP(req, mustPrefixes(t, tc.trusted...))
			if got != tc.want {
				t.Errorf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIP_IPv6BucketsSlash64 pins the #144 bucketing: IPv6 clients key on
// their /64 network (an attacker's whole residential prefix collapses to one
// lockout bucket), while IPv4 and IPv4-mapped IPv6 stay per-address (bucketing a
// shared NAT would let one abuser lock out every co-NAT client). The two
// distinct-suffix v6 addresses sharing a /64 map to ONE key; a different /64
// maps to a DIFFERENT key. FAILS on main (each v6 address had its own bucket).
func TestClientIP_IPv6BucketsSlash64(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{"v6_low_bits_a_collapse_to_64", "[2001:db8:1:2:aaaa::1]:443", "2001:db8:1:2::/64"},
		{"v6_low_bits_b_same_64", "[2001:db8:1:2:bbbb::2]:443", "2001:db8:1:2::/64"},
		{"v6_different_64_distinct_key", "[2001:db8:1:3::1]:443", "2001:db8:1:3::/64"},
		{"v4_stays_per_address", "192.0.2.1:9", "192.0.2.1"},
		{"v4_mapped_v6_keys_as_v4", "[::ffff:192.0.2.1]:9", "192.0.2.1"},
		{"portless_remoteaddr_verbatim", "192.0.2.1", "192.0.2.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/scores", nil)
			req.RemoteAddr = tc.remoteAddr
			// No trusted proxies: exercises the default/fast resolution path plus
			// bucketing. Bucketing composing with the trusted-proxy path is covered
			// by TestClientIP (its IPv4 XFF results are unchanged by bucketing).
			got := clientIP(req, nil)
			if got != tc.want {
				t.Errorf("clientIP(%q) = %q, want %q", tc.remoteAddr, got, tc.want)
			}
		})
	}
}

// fakeClock is a deterministic, concurrency-safe clock for limiter tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testLimiter(max int) (*authLimiter, *fakeClock) {
	// Fixed, non-zero base time so zero-value comparisons (lockedUntil.IsZero)
	// are unambiguous. Date.now() is unavailable in tests by policy, so use a
	// literal.
	clk := &fakeClock{t: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)}
	cfg := RateLimitConfig{MaxFailures: max, Window: 60 * time.Second, Lockout: 15 * time.Minute}
	return newAuthLimiter(cfg, clk.now), clk
}

func TestAuthLimiter_TripsAtMaxFailures(t *testing.T) {
	l, _ := testLimiter(3)
	const ip = "10.0.0.1"

	// Below threshold: not locked.
	l.recordFailure(ip)
	l.recordFailure(ip)
	if _, locked := l.retryAfter(ip); locked {
		t.Fatalf("locked after 2/3 failures, want not locked")
	}
	// Threshold reached: locked for ~Lockout.
	l.recordFailure(ip)
	d, locked := l.retryAfter(ip)
	if !locked {
		t.Fatalf("not locked after 3/3 failures, want locked")
	}
	if d <= 0 || d > 15*time.Minute {
		t.Errorf("retryAfter = %v, want (0, 15m]", d)
	}
}

func TestAuthLimiter_SuccessResets(t *testing.T) {
	l, _ := testLimiter(3)
	const ip = "10.0.0.2"
	l.recordFailure(ip)
	l.recordFailure(ip)
	l.recordSuccess(ip) // clears the 2 failures
	// Two fresh failures must NOT trip (would have been the 3rd+4th pre-reset).
	l.recordFailure(ip)
	l.recordFailure(ip)
	if _, locked := l.retryAfter(ip); locked {
		t.Errorf("locked after success reset + 2 failures, want not locked")
	}
}

func TestAuthLimiter_DifferentIPsIsolated(t *testing.T) {
	l, _ := testLimiter(3)
	for i := 0; i < 3; i++ {
		l.recordFailure("10.0.0.3")
	}
	if _, locked := l.retryAfter("10.0.0.3"); !locked {
		t.Fatalf("attacker IP not locked, want locked")
	}
	if _, locked := l.retryAfter("10.0.0.4"); locked {
		t.Errorf("innocent IP locked, want unaffected")
	}
}

func TestAuthLimiter_WindowPrunesStaleFailures(t *testing.T) {
	l, clk := testLimiter(3)
	const ip = "10.0.0.5"
	l.recordFailure(ip)
	l.recordFailure(ip)
	// Step past the 60s window, then two more failures. The first two have aged
	// out, so the count is 2, not 4 — must not trip.
	clk.advance(61 * time.Second)
	l.recordFailure(ip)
	l.recordFailure(ip)
	if _, locked := l.retryAfter(ip); locked {
		t.Errorf("locked on 2 in-window + 2 stale failures, want not locked")
	}
}

func TestAuthLimiter_LockoutExpires(t *testing.T) {
	l, clk := testLimiter(3)
	const ip = "10.0.0.6"
	for i := 0; i < 3; i++ {
		l.recordFailure(ip)
	}
	if _, locked := l.retryAfter(ip); !locked {
		t.Fatalf("want locked immediately after tripping")
	}
	clk.advance(15*time.Minute + time.Second)
	if d, locked := l.retryAfter(ip); locked {
		t.Errorf("still locked %v after lockout elapsed, want cleared", d)
	}
	// Fresh window: a single failure must not re-trip.
	l.recordFailure(ip)
	if _, locked := l.retryAfter(ip); locked {
		t.Errorf("re-locked on 1 failure after expiry, want fresh window")
	}
}

func TestAuthLimiter_DisabledIsNoOp(t *testing.T) {
	l := newAuthLimiter(RateLimitConfig{MaxFailures: 0}, nil)
	if l.enabled() {
		t.Fatalf("MaxFailures=0 should be disabled")
	}
	for i := 0; i < 100; i++ {
		l.recordFailure("10.0.0.7")
	}
	if _, locked := l.retryAfter("10.0.0.7"); locked {
		t.Errorf("disabled limiter locked an IP, want never")
	}
	// nil limiter is also safe (the requireAuth path relies on this).
	var nilL *authLimiter
	if nilL.enabled() {
		t.Errorf("nil limiter reports enabled")
	}
	if _, locked := nilL.retryAfter("x"); locked {
		t.Errorf("nil limiter reports locked")
	}
	nilL.recordFailure("x") // must not panic
	nilL.recordSuccess("x") // must not panic
}

func TestAuthLimiter_BadWindowOrLockoutDisables(t *testing.T) {
	// A non-zero MaxFailures with a non-positive window or lockout must be
	// treated as fully disabled, not as a limiter that can never trip (#36).
	for _, cfg := range []RateLimitConfig{
		{MaxFailures: 3, Window: 0, Lockout: time.Minute},
		{MaxFailures: 3, Window: time.Minute, Lockout: 0},
	} {
		l := newAuthLimiter(cfg, nil)
		if l.enabled() {
			t.Errorf("cfg %+v should be disabled (non-positive window/lockout)", cfg)
		}
		for i := 0; i < 50; i++ {
			l.recordFailure("10.0.0.1")
		}
		if _, locked := l.retryAfter("10.0.0.1"); locked {
			t.Errorf("cfg %+v locked an IP despite being disabled", cfg)
		}
	}
}

func TestAuthLimiter_LockoutDoesNotExtendOnFurtherFailures(t *testing.T) {
	l, clk := testLimiter(3)
	const ip = "10.7.7.7"
	for i := 0; i < 3; i++ {
		l.recordFailure(ip)
	}
	d1, locked := l.retryAfter(ip)
	if !locked {
		t.Fatalf("want locked after tripping")
	}
	// A failure mid-lockout must neither extend nor reset the countdown.
	clk.advance(5 * time.Minute)
	l.recordFailure(ip)
	d2, locked := l.retryAfter(ip)
	if !locked {
		t.Fatalf("want still locked 5m into a 15m lockout")
	}
	if d2 >= d1 {
		t.Errorf("lockout did not count down: before=%v after-5m=%v (a mid-lockout failure must not extend it)", d1, d2)
	}
}

func TestAuthLimiter_SweepEvictsStaleEntries(t *testing.T) {
	l, clk := testLimiter(3)
	// One genuinely locked IP — must survive the sweep.
	const lockedIP = "10.9.9.9"
	for i := 0; i < 3; i++ {
		l.recordFailure(lockedIP)
	}
	// Push the map past sweepThreshold with one-off failing IPs at t0.
	for i := 0; i <= sweepThreshold; i++ {
		l.recordFailure(fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256))
	}
	// Age the one-offs out of the window (but stay inside the 15m lockout so the
	// locked IP is still locked), then a fresh failure triggers the sweep.
	clk.advance(2 * time.Minute)
	l.recordFailure("10.8.8.8")

	if _, locked := l.retryAfter(lockedIP); !locked {
		t.Errorf("sweep evicted a still-locked IP, want retained")
	}
	l.mu.Lock()
	n := len(l.ips)
	l.mu.Unlock()
	// Only the locked IP and the fresh trigger should remain; the thousands of
	// stale single-failure entries must be gone.
	if n > 10 {
		t.Errorf("after sweep len(ips) = %d, want stale entries evicted (~2)", n)
	}
}

// seedEntries installs n synthetic tracked IPs directly, bypassing the O(n)
// per-failure sweep recordFailure runs above sweepThreshold. Reaching the
// maxTrackedIPs cap through the public path costs ~12s under -race (the sweep
// ramp, exercised separately by TestAuthLimiter_SweepEvictsStaleEntries); these
// eviction tests care about the cap and eviction ORDER, not the ramp, so they
// pre-load a realistic at-capacity map and then drive recordFailure to trigger
// exactly the eviction path under test. locked installs an active lockout
// (lockedUntil in the future); otherwise an unlocked, still-counting entry with
// one in-window failure at `at`. Keys are prefixed so they never collide with a
// test's named entries. Caller must not hold l.mu.
func seedEntries(l *authLimiter, prefix string, n int, locked bool, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := 0; i < n; i++ {
		e := &ipEntry{}
		if locked {
			e.lockedUntil = at.Add(l.cfg.Lockout)
		} else {
			e.failures = []time.Time{at}
		}
		l.ips[fmt.Sprintf("%s-%d", prefix, i)] = e
	}
}

func (l *authLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ips)
}

// TestAuthLimiter_MapBoundedUnderAddressRotation proves the #144 hard cap: no
// sequence of failures from fresh keys can push the tracking map past
// maxTrackedIPs. On main the map has no ceiling and grows without bound (the
// OOM / lockout-bypass vector under IPv6 source-address rotation).
func TestAuthLimiter_MapBoundedUnderAddressRotation(t *testing.T) {
	l, clk := testLimiter(5) // 5 > 1 so a single failure leaves a key UNLOCKED
	// Pre-load to exactly the cap with unlocked, still-counting entries (the
	// cheapest thing for an attacker to mint: one failed auth per rotated key).
	seedEntries(l, "seed", maxTrackedIPs, false, clk.now())
	if l.size() != maxTrackedIPs {
		t.Fatalf("seed size = %d, want %d", l.size(), maxTrackedIPs)
	}
	// Every fresh failure must trigger an eviction that holds the line.
	for i := 0; i < 500; i++ {
		l.recordFailure(fmt.Sprintf("rot-%d", i))
		if n := l.size(); n > maxTrackedIPs {
			t.Fatalf("after insert %d: size = %d, exceeds cap %d", i, n, maxTrackedIPs)
		}
	}
}

// TestAuthLimiter_EvictionCannotFlushLiveLockout is the security test the eviction
// order exists for: an attacker who fills the map with cheap, unlocked entries
// (one failed auth per rotated address, below MaxFailures) must NOT be able to
// evict a legitimate in-force lockout. Because an unlocked entry's value expires
// (its last failure is in the past) before any active lockout's future
// lockedUntil, evictOldest drains the attacker's own flood first and never
// reaches the lockout. Evicting newest-first or at random would let this flood
// knock the victim's lockout out — the bug this ordering prevents.
func TestAuthLimiter_EvictionCannotFlushLiveLockout(t *testing.T) {
	l, clk := testLimiter(5)
	const victim = "legit-lockout"
	for i := 0; i < 5; i++ {
		l.recordFailure(victim) // trips a real lockout at t0
	}
	if _, locked := l.retryAfter(victim); !locked {
		t.Fatalf("victim not locked after 5 failures, want locked")
	}
	// Fill the rest of the map with unlocked counting entries, then have the
	// attacker keep rotating in fresh unlocked keys past the cap.
	seedEntries(l, "flood", maxTrackedIPs-1, false, clk.now())
	for i := 0; i < 300; i++ {
		l.recordFailure(fmt.Sprintf("attack-%d", i))
		if n := l.size(); n > maxTrackedIPs {
			t.Fatalf("after attack insert %d: size = %d, exceeds cap %d", i, n, maxTrackedIPs)
		}
		if _, locked := l.retryAfter(victim); !locked {
			t.Fatalf("attack insert %d flushed the victim's live lockout — eviction order is unsafe", i)
		}
	}
}

// TestAuthLimiter_EvictsOldestLockoutFirst proves that when the map is full of
// active lockouts, eviction sacrifices the OLDEST (nearest-to-expiry) lockout
// first and never the just-written entry. Fresh lockouts (latest lockedUntil)
// survive longest — that is what stops an attacker flushing a recent lockout.
func TestAuthLimiter_EvictsOldestLockoutFirst(t *testing.T) {
	l, clk := testLimiter(1) // 1 failure locks immediately
	l.recordFailure("A")     // A locked, lockedUntil = t0 + 15m (earliest)
	clk.advance(time.Minute)
	l.recordFailure("B") // B locked, lockedUntil = t0 + 16m
	// Fill the remainder with lockouts strictly newer than A (and >= B), so A is
	// the globally oldest lockout. Total now == cap; one more insert forces a
	// single eviction, which must take A.
	seedEntries(l, "fill", maxTrackedIPs-2, true, clk.now())
	if l.size() != maxTrackedIPs {
		t.Fatalf("pre-trigger size = %d, want %d", l.size(), maxTrackedIPs)
	}
	l.recordFailure("keep") // cap+1 → evict exactly one (the oldest lockout, A)

	if _, locked := l.retryAfter("A"); locked {
		t.Errorf("A (oldest lockout) survived eviction, want evicted")
	}
	if _, locked := l.retryAfter("B"); !locked {
		t.Errorf("B (newer lockout) was evicted, want retained")
	}
	if _, locked := l.retryAfter("keep"); !locked {
		t.Errorf("just-written key was evicted, want retained (the current attacker's bucket)")
	}
	if n := l.size(); n > maxTrackedIPs {
		t.Errorf("post-trigger size = %d, exceeds cap %d", n, maxTrackedIPs)
	}
}

// TestAuthLimiter_EvictionPrefersExpiredViaSweep proves the cheap sweep runs
// before any live lockout is force-evicted: a map full of now-expired counting
// entries is reclaimed by the sweep, so the eviction path never has to sacrifice
// the still-active lockout at all.
func TestAuthLimiter_EvictionPrefersExpiredViaSweep(t *testing.T) {
	l, clk := testLimiter(5)
	const victim = "still-locked"
	for i := 0; i < 5; i++ {
		l.recordFailure(victim) // locked at t0, lockedUntil = t0 + 15m
	}
	// Fill to the cap with counting entries whose single failure is at t0.
	seedEntries(l, "expiring", maxTrackedIPs-1, false, clk.now())
	if l.size() != maxTrackedIPs {
		t.Fatalf("pre-age size = %d, want %d", l.size(), maxTrackedIPs)
	}
	// Age past the window (but not the lockout): the counting entries are now
	// expired; the victim's lockout is still in force.
	clk.advance(61 * time.Second)
	l.recordFailure("trigger") // sweep reclaims all expired entries first

	if _, locked := l.retryAfter(victim); !locked {
		t.Errorf("victim lockout was sacrificed, want the sweep to reclaim expired entries first")
	}
	// Sweep dropped the thousands of expired entries; only the victim and the
	// trigger should remain (no live eviction was needed).
	if n := l.size(); n > 10 {
		t.Errorf("post-sweep size = %d, want expired entries reclaimed (~2)", n)
	}
}

func TestAuthLimiter_ConcurrentAccessNoRace(t *testing.T) {
	// Exercises the mutex under `go test -race`: many goroutines hammering a
	// small set of IPs through all three mutating paths.
	l, _ := testLimiter(5)
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.0.0.%d", g%4)
			for i := 0; i < 200; i++ {
				l.recordFailure(ip)
				l.retryAfter(ip)
				if i%10 == 0 {
					l.recordSuccess(ip)
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestDefaultRateLimitConfig(t *testing.T) {
	c := DefaultRateLimitConfig()
	if c.MaxFailures != 10 || c.Window != 60*time.Second || c.Lockout != 15*time.Minute {
		t.Errorf("DefaultRateLimitConfig() = %+v, want {10, 60s, 15m}", c)
	}
}

// TestClientIP_MultipleXFFHeaderLines guards against a proxy that appends its
// forwarded IP as a SEPARATE X-Forwarded-For header line rather than folding
// into one comma value. net/http keeps repeated headers as distinct Values, and
// Header.Get would return only the first (attacker-controlled) line; the walk
// must flatten every line in order so the rightmost hop (the trusted proxy's
// appended real client) still wins. A regression here would let an untrusted
// client behind such a proxy forge its bucket.
func TestClientIP_MultipleXFFHeaderLines(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scores", nil)
	req.RemoteAddr = "10.0.0.5:443" // trusted peer
	// Two lines: the attacker-supplied one first, the proxy-appended real client
	// last. RFC 7230 §3.2.2 treats these as one ordered comma list.
	req.Header.Add("X-Forwarded-For", "1.1.1.1")
	req.Header.Add("X-Forwarded-For", "203.0.113.7")
	got := clientIP(req, mustPrefixes(t, "10.0.0.0/8"))
	if got != "203.0.113.7" {
		t.Errorf("clientIP() = %q, want %q (rightmost hop across all header lines)", got, "203.0.113.7")
	}
}

// TestRequireAuth_IgnoresXForwardedFor pins the security decision that the
// limiter keys on RemoteAddr, not X-Forwarded-For — otherwise an attacker would
// mint a fresh bucket per request by varying the header and never lock out.
func TestRequireAuth_IgnoresXForwardedFor(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithTokenAndLimit(t, token,
		RateLimitConfig{MaxFailures: 3, Window: time.Minute, Lockout: 15 * time.Minute})
	mux := http.NewServeMux()
	h.Register(mux)
	body, _ := json.Marshal(validCostPayload())

	post := func(xff string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/costs", bytes.NewReader(body))
		req.RemoteAddr = "203.0.113.50:9999" // constant peer
		req.Header.Set("X-Forwarded-For", xff)
		req.Header.Set("Authorization", "Bearer wrong-token-value")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	// 3 failures from the same peer but a different spoofed XFF each time.
	post("1.1.1.1")
	post("2.2.2.2")
	post("3.3.3.3")
	if c := post("4.4.4.4"); c != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 — X-Forwarded-For must not create fresh rate-limit buckets", c)
	}
}

// TestRequireAuth_IPv6RotationWithinSlash64Trips drives the #144 /64 bucketing
// through the real auth path: an attacker rotating source addresses within a
// single IPv6 /64 (a standard residential allocation) shares ONE lockout bucket,
// so 10 failures from 10 distinct addresses trip a 429 on the 11th address in
// that /64 — while an address in a DIFFERENT /64 is unaffected. On main each
// address had its own bucket and the lockout never tripped under rotation.
func TestRequireAuth_IPv6RotationWithinSlash64Trips(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithTokenAndLimit(t, token,
		RateLimitConfig{MaxFailures: 10, Window: time.Minute, Lockout: 15 * time.Minute})
	mux := http.NewServeMux()
	h.Register(mux)

	body, _ := json.Marshal(validCostPayload())
	post := func(remoteAddr string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/costs", bytes.NewReader(body))
		req.RemoteAddr = remoteAddr
		req.Header.Set("Authorization", "Bearer wrong-token-value")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	// 10 failed auths from 10 distinct addresses, all inside 2001:db8:aa:bb::/64.
	for i := 1; i <= 10; i++ {
		addr := fmt.Sprintf("[2001:db8:aa:bb::%d]:5555", i)
		if c := post(addr); c != http.StatusUnauthorized {
			t.Fatalf("rotation attempt %d (%s): status = %d, want 401", i, addr, c)
		}
	}
	// 11th distinct address in the SAME /64 is locked out — the rotation shared
	// one bucket and tripped it.
	if c := post("[2001:db8:aa:bb::ffff]:5555"); c != http.StatusTooManyRequests {
		t.Errorf("11th address in the /64: status = %d, want 429 (rotation must share one bucket)", c)
	}
	// An address in a DIFFERENT /64 must be unaffected — bucketing is per-/64,
	// not global.
	if c := post("[2001:db8:aa:cc::1]:5555"); c != http.StatusUnauthorized {
		t.Errorf("different /64: status = %d, want 401 (must not share the tripped bucket)", c)
	}
}

// TestRequireAuth_LockoutEndToEnd drives the limiter through the real HTTP path:
// repeated bad tokens from one IP trip a 429 with Retry-After, a different IP is
// unaffected, and a locked IP is rejected before the token compare (so even a
// correct token gets 429 while locked).
func TestRequireAuth_LockoutEndToEnd(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithTokenAndLimit(t, token,
		RateLimitConfig{MaxFailures: 3, Window: time.Minute, Lockout: 15 * time.Minute})
	mux := http.NewServeMux()
	h.Register(mux)

	body, _ := json.Marshal(validCostPayload())
	post := func(remoteAddr, authz string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/costs", bytes.NewReader(body))
		req.RemoteAddr = remoteAddr
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	const attacker = "203.0.113.9:5555"
	// 3 bad tokens → three 401s (under/at threshold the compare still runs).
	for i := 1; i <= 3; i++ {
		if rec := post(attacker, "Bearer wrong-token-value"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, rec.Code)
		}
	}
	// 4th request is locked out before the compare — even the CORRECT token 429s.
	rec := post(attacker, "Bearer "+token)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked-out attempt: status = %d, want 429", rec.Code)
	}
	if n, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || n <= 0 {
		t.Errorf("Retry-After = %q, want a positive integer seconds value", rec.Header().Get("Retry-After"))
	}

	// A different IP with the correct token is unaffected.
	if rec := post("198.51.100.7:4444", "Bearer "+token); rec.Code != http.StatusCreated {
		t.Errorf("innocent IP with valid token: status = %d, want 201", rec.Code)
	}
}

// TestRequireAuth_SuccessResetsLockoutCounter confirms that a valid auth before
// the threshold clears the failure count, so intermittent typos never lock out
// a legitimate client.
func TestRequireAuth_SuccessResetsLockoutCounter(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithTokenAndLimit(t, token,
		RateLimitConfig{MaxFailures: 3, Window: time.Minute, Lockout: 15 * time.Minute})
	mux := http.NewServeMux()
	h.Register(mux)

	body, _ := json.Marshal(validCostPayload())
	post := func(authz string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/costs", bytes.NewReader(body))
		req.RemoteAddr = "192.0.2.50:6666"
		req.Header.Set("Authorization", authz)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	post("Bearer wrong") // 1 failure
	post("Bearer wrong") // 2 failures
	if c := post("Bearer " + token); c != http.StatusCreated {
		t.Fatalf("valid token: status = %d, want 201", c)
	}
	// Counter reset: two more failures must not trip (would be 4 cumulative
	// without the reset).
	post("Bearer wrong")
	if c := post("Bearer wrong"); c != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (not 429 — success should have reset the counter)", c)
	}
}
