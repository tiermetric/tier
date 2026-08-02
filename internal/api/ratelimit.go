package api

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// RateLimitConfig configures the per-IP failed-auth lockout (#36). It is
// defence-in-depth on top of the bearer gate (#22/#59): a high-entropy token
// already resists brute force and the compare is constant-time, but an
// unthrottled attacker can still run a wordlist at line rate. After MaxFailures
// failed auths from one IP inside Window, that IP is locked out (429) for
// Lockout. A successful auth clears the IP's counter.
//
// The zero value (MaxFailures == 0) disables the limiter entirely — the
// requireAuth path then behaves exactly as before this change.
type RateLimitConfig struct {
	MaxFailures int           // failed auths within Window before lockout; 0 disables
	Window      time.Duration // sliding window the failures must fall within
	Lockout     time.Duration // how long a tripped IP stays blocked
	// TrustedProxies are the CIDRs of reverse proxies / TLS terminators whose
	// X-Forwarded-For header may be believed (#131). Empty (the default) means
	// XFF is never trusted and the lockout keys on RemoteAddr exactly as before
	// — behind a shared terminator that makes every client share one bucket, so
	// configure this only when a trusted proxy sanitizes inbound XFF.
	TrustedProxies []netip.Prefix
}

// DefaultRateLimitConfig is the production default: 10 failures / 60s → 15m
// lockout. Tuned so a wordlist is throttled to a crawl while a human fat-
// fingering a token a few times is never affected.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		MaxFailures: 10,
		Window:      60 * time.Second,
		Lockout:     15 * time.Minute,
	}
}

// sweepThreshold bounds memory: once the tracking map exceeds this many IPs, a
// failure also drops entries that are no longer locked out and have no failures
// inside the window. In-memory state resets on restart (sufficient for v1).
const sweepThreshold = 4096

// maxTrackedIPs is the HARD ceiling on the tracking map (#144). sweepThreshold
// only prunes EXPIRED entries; a flood of still-active lockouts — trivial to
// generate by rotating IPv6 source addresses — is unprunable and would grow the
// map without bound (an OOM vector, and a lockout bypass since each fresh key
// starts a new counter). Once the map exceeds this ceiling, recordFailure
// force-evicts (see evictOldest) to hold the line. At ~100 B/entry this bounds
// the limiter at low single-digit MB; it is set far above any legitimate office
// NAT / IPv6-prefix population so honest traffic never triggers an eviction.
//
// Cost ceiling: while the map is saturated with live entries (only under a
// sustained attack), each failed auth does one sweepLocked plus one evictOldest
// scan — at most ~2*maxTrackedIPs (~32k) map ops under l.mu. That is bounded and
// mirrors the pre-existing above-sweepThreshold sweep cost, not a new
// amplification; if this limiter ever moves onto a hotter path, a min-heap would
// cut eviction to O(log n).
const maxTrackedIPs = 16384

// Compile-time invariant: maxTrackedIPs must exceed sweepThreshold so that by
// the time recordFailure force-evicts (len > maxTrackedIPs) the expired-entry
// sweep (len > sweepThreshold) has already run — evictOldest then only ever
// sacrifices LIVE entries, never something the cheap sweep would have dropped.
// If this ordering is ever broken, `maxTrackedIPs - sweepThreshold - 1`
// underflows an unsigned constant and the build fails here.
const _ = uint(maxTrackedIPs - sweepThreshold - 1)

type ipEntry struct {
	failures    []time.Time // failure timestamps, pruned to within Window
	lockedUntil time.Time   // zero when not locked
}

// authLimiter is a concurrency-safe per-IP failed-auth tracker. now is injected
// so tests can advance time deterministically; production passes time.Now.
type authLimiter struct {
	cfg RateLimitConfig
	now func() time.Time
	mu  sync.Mutex
	ips map[string]*ipEntry
}

func newAuthLimiter(cfg RateLimitConfig, now func() time.Time) *authLimiter {
	if now == nil {
		now = time.Now
	}
	return &authLimiter{cfg: cfg, now: now, ips: make(map[string]*ipEntry)}
}

// enabled reports whether the limiter should be consulted. A nil limiter, a
// zero MaxFailures, or a non-positive Window/Lockout all mean "disabled" — the
// latter two would otherwise produce a limiter that can never trip (every prune
// cutoff is now, every lockout is already expired), which is worse than off
// because it looks armed. cmd/tierd additionally refuses to start in that
// misconfigured state; this guard keeps any other caller safe.
func (l *authLimiter) enabled() bool {
	return l != nil && l.cfg.MaxFailures > 0 && l.cfg.Window > 0 && l.cfg.Lockout > 0
}

// trustedProxies returns the configured trusted-proxy CIDRs, nil-safe so the
// clientIP call in authPasses keeps the same graceful-on-nil-limiter contract
// as retryAfter/recordFailure/recordSuccess (a nil limiter means "no trusted
// proxies", i.e. XFF is never consulted).
func (l *authLimiter) trustedProxies() []netip.Prefix {
	if l == nil {
		return nil
	}
	return l.cfg.TrustedProxies
}

// retryAfter returns the remaining lockout for ip and whether it is currently
// locked out. A non-locked (or expired-lockout) IP returns (0, false).
func (l *authLimiter) retryAfter(ip string) (time.Duration, bool) {
	if !l.enabled() {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.ips[ip]
	if e == nil || e.lockedUntil.IsZero() {
		return 0, false
	}
	now := l.now()
	if !now.Before(e.lockedUntil) {
		// Lockout elapsed — clear it so the IP gets a fresh window.
		delete(l.ips, ip)
		return 0, false
	}
	return e.lockedUntil.Sub(now), true
}

// recordFailure registers one failed auth for ip and trips the lockout when the
// IP reaches MaxFailures within Window.
func (l *authLimiter) recordFailure(ip string) {
	if !l.enabled() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.ips[ip]
	if e == nil {
		e = &ipEntry{}
		l.ips[ip] = e
	}
	// Already locked out: a request during lockout shouldn't extend it (the
	// retryAfter gate normally short-circuits before we get here; this keeps
	// the invariant even if a caller records a failure anyway).
	if !e.lockedUntil.IsZero() && now.Before(e.lockedUntil) {
		return
	}
	cutoff := now.Add(-l.cfg.Window)
	kept := e.failures[:0]
	for _, t := range e.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	e.failures = kept
	if len(kept) >= l.cfg.MaxFailures {
		e.lockedUntil = now.Add(l.cfg.Lockout)
		e.failures = nil // counter rolls into the lockout; reset on expiry
	}
	if len(l.ips) > sweepThreshold {
		l.sweepLocked(now)
	}
	// Hard cap (#144). The sweep above drops everything expired; if the map is
	// STILL over the ceiling, force-evict live entries until it is back within
	// bounds. Pass ip so the entry we just wrote — the current attacker's own
	// bucket — is never the victim and keeps counting toward its own lockout.
	if len(l.ips) > maxTrackedIPs {
		l.evictOldest(now, ip)
	}
}

// recordSuccess clears any failure/lockout state for ip — a correct credential
// proves the client is not the brute-forcer being throttled.
func (l *authLimiter) recordSuccess(ip string) {
	if !l.enabled() {
		return
	}
	l.mu.Lock()
	delete(l.ips, ip)
	l.mu.Unlock()
}

// sweepLocked drops entries that are neither locked out nor carry a failure
// inside the window. Caller must hold l.mu.
func (l *authLimiter) sweepLocked(now time.Time) {
	cutoff := now.Add(-l.cfg.Window)
	for ip, e := range l.ips {
		lockActive := !e.lockedUntil.IsZero() && now.Before(e.lockedUntil)
		if lockActive {
			continue
		}
		hasRecent := false
		for _, t := range e.failures {
			if t.After(cutoff) {
				hasRecent = true
				break
			}
		}
		if !hasRecent {
			delete(l.ips, ip)
		}
	}
}

// evictOldest force-evicts entries until the map is back within maxTrackedIPs,
// never evicting keep (the entry recordFailure just wrote — the current
// attacker's bucket must keep counting toward its own lockout). Caller holds
// l.mu.
//
// Eviction order is the security-critical part. Each pass removes the entry
// whose PROTECTIVE VALUE EXPIRES SOONEST, measured on one timeline:
//   - a locked entry's value ends at lockedUntil (a time in the future while the
//     lockout is active);
//   - an unlocked, still-counting entry's value is anchored at its most-recent
//     failure (a time in the past or present).
//
// Because any active lockout's lockedUntil is strictly after now, and any
// unlocked entry's last failure is at-or-before now, this single "soonest
// first" rule always drains cheap unlocked counters BEFORE sacrificing any
// lockout, and within the lockouts drains the OLDEST (nearest-to-expiry) first.
//
// Why this is safe — an attacker cannot flush a legitimate in-force lockout:
//   - A fresh lockout has the LATEST lockedUntil, so it is evicted LAST; an
//     attacker who floods the map cannot reach it while any older lockout or any
//     unlocked entry remains.
//   - Flooding with cheap unlocked entries (one failed auth per rotated /64,
//     below MaxFailures) only evicts other unlocked entries — the attacker's own
//     flood — never a lockout. To evict a lockout the attacker must LOCK
//     thousands of their own prefixes (MaxFailures auths each), which locks them
//     out everywhere and buys only the eviction of lockouts already nearer to
//     expiry than their own. Evicting newest-first or at random would instead
//     let a cheap flood knock out a just-created lockout, defeating the limiter.
//
// The trade-off is deliberate and bounded: under a sustained map-fill the very
// oldest lockouts are released early. That is preferred over unbounded lockout
// persistence because the /64 bucketing in clientIP already collapses an IPv6
// attacker's key space, and any re-offending source re-trips within one Window.
func (l *authLimiter) evictOldest(now time.Time, keep string) {
	for len(l.ips) > maxTrackedIPs {
		var (
			victim   string
			bestTime time.Time
			found    bool
		)
		for ip, e := range l.ips {
			if ip == keep {
				continue
			}
			var expiry time.Time
			if !e.lockedUntil.IsZero() && now.Before(e.lockedUntil) {
				expiry = e.lockedUntil // future: evicted only after all unlocked entries
			} else {
				expiry = lastFailure(e) // past/zero: evicted before any live lockout
			}
			if !found || expiry.Before(bestTime) {
				victim, bestTime, found = ip, expiry, true
			}
		}
		if !found {
			// Only keep remains (unreachable while len > maxTrackedIPs >= 1, but
			// never spin the loop rather than risk a hang under the lock).
			return
		}
		delete(l.ips, victim)
	}
}

// lastFailure returns the entry's most-recent failure timestamp (failures are
// appended in time order), or the zero time when it has none. The zero time
// sorts before any real timestamp, so a counter-less entry is evicted first.
func lastFailure(e *ipEntry) time.Time {
	if n := len(e.failures); n > 0 {
		return e.failures[n-1]
	}
	return time.Time{}
}

// clientIP returns the key the failed-auth lockout buckets on: the resolved
// client address (see resolveClientIP for the trusted-proxy / X-Forwarded-For
// resolution) collapsed to its /64 network when it is IPv6.
//
// IPv6 /64 bucketing (#144): a single residential IPv6 allocation is a whole
// /64 — 2^64 source addresses. Keying on the exact address would hand an
// attacker an unlimited supply of fresh lockout buckets: rotate the low 64 bits
// on every request and MaxFailures is never reached. Collapsing to the /64
// gives one bucket per customer prefix, exactly as an IPv4 address is already
// one bucket per customer. The deliberate cost is that every client sharing a
// /64 shares one failure budget — the same NAT trade-off already accepted for
// IPv4 (and for a shared TLS terminator, per the #131 note above). Per-address
// IPv6 keys would defeat the lockout entirely, so /64 bucketing is the safe
// default. IPv4 and IPv4-mapped IPv6 (::ffff:a.b.c.d) stay per-address:
// bucketing a shared IPv4 NAT into a subnet would instead let one abuser lock
// out every co-NAT client. Bucketing composes with the trusted-proxy resolution
// rather than bypassing it — it is applied to the address resolveClientIP
// returns, whether that is the direct peer or an XFF-derived real client.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	return bucketKey(resolveClientIP(r, trusted))
}

// bucketKey collapses an IPv6 address key to its /64 network; IPv4 (including
// IPv4-mapped IPv6, which is unmapped to its dotted form) and any key that is
// not a bare IP literal are returned unchanged. netip does the prefix math so
// there is no hand-rolled string bit-masking. See clientIP for the rationale.
func bucketKey(key string) string {
	addr, err := netip.ParseAddr(key)
	if err != nil {
		// Not a bare IP literal — a port-less non-IP RemoteAddr, or an already
		// bucketed key. Preserve exact-string behaviour (keys per whole value).
		return key
	}
	addr = addr.Unmap() // ::ffff:a.b.c.d must bucket as IPv4, not as an IPv6 /64.
	if addr.Is4() {
		return addr.String()
	}
	// IPv6: key on the /64 the client's ISP typically controls.
	return netip.PrefixFrom(addr, 64).Masked().String()
}

// resolveClientIP returns the client IP the lockout attributes a request to,
// before /64 bucketing (see clientIP).
//
// Default (trusted empty): the direct TCP peer (RemoteAddr) — an attacker
// controls X-Forwarded-For, so honoring it would mint unlimited rate-limit
// buckets and defeat the lockout. That reasoning still governs; XFF is only
// consulted when the direct peer is itself inside a configured trusted CIDR.
//
// With trusted CIDRs configured AND the direct peer inside one of them, walk
// X-Forwarded-For right-to-left and return the first (rightmost) address NOT
// inside a trusted CIDR — the hop our own edge appended, i.e. the real client.
// Rightmost-untrusted, not leftmost: the left end is client-supplied and
// forgeable even behind an honest proxy that appends rather than overwrites.
// An entry that fails to parse stops the walk (fail toward RemoteAddr, never
// toward trusting garbage). If every entry is trusted or the header is absent,
// fall back to the direct peer — never an empty string.
func resolveClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr without a port (unusual) — use it verbatim, preserving the
		// pre-#131 fallback.
		return r.RemoteAddr
	}
	// Fast path: no trusted proxies means XFF is never consulted — behaviour is
	// byte-identical to the pre-#131 code and no XFF parsing happens.
	if len(trusted) == 0 {
		return host
	}
	peer, perr := netip.ParseAddr(host)
	if perr != nil {
		// Not a parseable IP literal (can't happen for a real TCP peer, but be
		// safe) — fall back to the raw host, matching the old return value.
		return host
	}
	peer = canonicalAddr(peer)
	if !inTrusted(peer, trusted) {
		// Direct peer is not a trusted proxy — never believe its XFF.
		return peer.String()
	}
	// Trusted peer: walk XFF right-to-left for the rightmost untrusted hop.
	// Join ALL X-Forwarded-For header lines (a proxy may append a distinct
	// header line rather than fold into one comma list; net/http keeps repeats
	// as separate Values, and Header.Get would return only the first — the
	// attacker-supplied end). Comma-joining preserves left-to-right hop order so
	// the trusted proxy's appended real-client IP remains the rightmost token.
	xff := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			hop, herr := netip.ParseAddr(strings.TrimSpace(parts[i]))
			if herr != nil {
				// Garbage hop: stop the walk and fall back to the peer rather
				// than trusting an unparseable token.
				break
			}
			hop = canonicalAddr(hop)
			if !inTrusted(hop, trusted) {
				return hop.String()
			}
		}
	}
	// XFF absent, entirely trusted, or hit garbage — key on the trusted peer.
	return peer.String()
}

// canonicalAddr normalizes an address for both CIDR containment and use as a
// bucket key: it strips any IPv6 zone (so [fe80::1%eth0] compares by address)
// and unmaps IPv4-mapped IPv6 (so ::ffff:10.0.0.1 matches a 10.0.0.0/8 prefix).
func canonicalAddr(a netip.Addr) netip.Addr {
	return a.WithZone("").Unmap()
}

// inTrusted reports whether a falls inside any trusted prefix. Prefix.Contains
// masks the low bits itself, so a non-canonical CIDR (e.g. 10.0.0.5/8) still
// matches, and an IPv4 address is never contained in an IPv6 prefix (family
// mismatch returns false) — which is why the peer/hop is Unmap()'d first.
func inTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
