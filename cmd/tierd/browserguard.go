package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// webhookPath is exempt from the Host check: its trust is the HMAC signature,
// not loopback reachability, so a rebinding page cannot forge it.
const webhookPath = "/webhook/github"

// browserGuard refuses, with 403, the two browser attacks on a tierd the
// operator's own browser can reach (#893):
//
//   - a cross-origin request (crossOrigin) to /api/ on any method, or to any
//     path on a method other than GET, HEAD or OPTIONS, so a page cannot drive
//     the provider proxies into metering spend (#903);
//   - when checkHost is set, any request whose Host is not a loopback name or
//     IP, which is what a DNS-rebinding page sends.
//
// Non-browser clients (curl, the shipper, Go clients) send neither Origin nor
// Sec-Fetch-Site, and address the server by the name they dialled, so they pass.
func browserGuard(next http.Handler, checkHost bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checkHost && r.URL.Path != webhookPath && !isLoopbackHost(strings.ToLower(hostWithoutPort(r.Host))) {
			refuse(w, "Host is not a loopback name: this tierd is bound to loopback without a token; browse via 127.0.0.1 or localhost, or set --api-token")
			return
		}
		write := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if (write || strings.HasPrefix(r.URL.Path, "/api/")) && crossOrigin(r) {
			refuse(w, "cross-origin request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// crossOrigin reports whether a browser sent r from a page that is not this
// server's own origin. Sec-Fetch-Site is authoritative when present, and only
// same-origin and none (typed URL, bookmark) pass: same-site would admit a page
// on another localhost port. Without it, Origin's host must equal Host; the
// scheme is not compared because a TLS-terminating proxy changes it. Same
// precedence as net/http.CrossOriginProtection, applied to every method.
func crossOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site != "same-origin" && site != "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host)
}

func hostWithoutPort(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

func refuse(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// hostCheckArmed reports whether browserGuard checks Host: only for a tokenless
// listener on a loopback address, where loopback reachability is the only
// credential. With a token a rebinding page cannot authenticate, and the README
// puts a token-mode tierd on 127.0.0.1 behind a proxy that forwards a public
// Host. The synthetic demo is exempt: its data is public and its writes are
// absent, and its tunnel forwards the public Host.
func hostCheckArmed(addr, apiToken string, syntheticDemo bool) bool {
	if apiToken != "" || syntheticDemo {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	return err == nil && isLoopbackHost(host)
}
