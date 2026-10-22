module github.com/tiermetric/tier

// The `go` directive is deliberately a full patch version, and there is
// deliberately no `toolchain` directive (#230, originally GO-2026-5856 —
// crypto/tls ECH privacy leak, fixed in 1.26.5).
//
// Raised 1.26.5 -> 1.26.6 on 2026-08-22 (#694). The floor now also CLOSES eight
// fixable HIGH stdlib CVEs: CVE-2026-33818, -39821, -46600, -56853, -56858,
// -56859, -56860, -56862. All eight are fixed in 1.26.6; none needed a code
// change. Provenance, because a count like this has a shelf life and this one
// already moved (2 on 2026-08-14 -> 8 on 2026-08-22, same image digest):
//
//   trivy image --ignore-unfixed --severity HIGH,CRITICAL \
//     ghcr.io/tiermetric/tierd:latest        measured 2026-08-22 -> 8, all stdlib v1.26.5
//
// Raised 1.26.6 -> 1.26.9 on 2026-10-08 (#1136): closes nine called stdlib
// advisories govulncheck reported against go1.26.6 — GO-2026-6617 (HTTP/2 HPACK
// encoder race crash), -6613, -6612, -6611, -6610, -6605, -6603 (net/http),
// -6608 (net/textproto), -6607 (crypto/tls); all fixed in 1.26.9.
//
// Raised 1.26.9 -> 1.27.2 after v0.5.2 (#1140). Keep all build pins in
// lockstep; Go 1.27.0 and 1.27.1 carry the nine advisories above.
// Raised 1.27.2 -> 1.27.3 (#1344, GO-2026-6809)
// Fixes crypto/x509, reachable via io.Copy -> x509.Certificate.Verify.
//
// A toolchain raise does not retroactively fix an already-published image:
// Measured 2026-10-08: ghcr.io/tiermetric/tierd:latest (0.4.1, go1.26.6)
// carried those nine advisories; rebuilding and releasing v0.5.2 fixed it (#683).
//
// A `toolchain go1.27.3` line is ignored under GOTOOLCHAIN=local, so it would
// let someone silently build a tierd that links the vulnerable stdlib. The
// `go` directive hard-fails instead: `go.mod requires go >= 1.27.3`. Fail loud.
//
// The usual objection — "a patch-version floor burdens downstream importers" —
// does not apply: every package here is under internal/ or cmd/, so nothing in
// this module is externally importable. The only consumers are people building
// or `go install`-ing tierd, and those are exactly the people who must not end
// up with a vulnerable binary.
go 1.27.3

require (
	github.com/fsnotify/fsnotify v1.10.1
	go.yaml.in/yaml/v3 v3.0.5
	modernc.org/sqlite v1.60.1
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.49.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
