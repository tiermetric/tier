module github.com/tiermetric/tier

// The `go` directive is deliberately a full patch version, and there is
// deliberately no `toolchain` directive (#230, GO-2026-5856 — crypto/tls ECH
// privacy leak, fixed in 1.26.5).
//
// A `toolchain go1.26.5` line is ignored under GOTOOLCHAIN=local, so it would
// let someone silently build a tierd that links the vulnerable crypto/tls. The
// `go` directive hard-fails instead: `go.mod requires go >= 1.26.5`. Fail loud.
//
// The usual objection — "a patch-version floor burdens downstream importers" —
// does not apply: every package here is under internal/ or cmd/, so nothing in
// this module is externally importable. The only consumers are people building
// or `go install`-ing tierd, and those are exactly the people who must not end
// up with a vulnerable binary.
go 1.26.5

require (
	github.com/fsnotify/fsnotify v1.10.1
	go.yaml.in/yaml/v3 v3.0.4
	modernc.org/sqlite v1.48.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.42.0 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
