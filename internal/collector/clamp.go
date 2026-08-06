package collector

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/tiermetric/tier/internal/logsafe"
)

// ClampNegativeTokens zeroes any negative counts in-place and reports whether
// anything was clamped. Negative usage is always a wire/parse defect — no
// provider bills negative tokens — so callers warn + count on a true return.
//
// The helper is deliberately pure: it does not log or bump any metric. Callers
// own the source-labelled warn (via slog) and the RecordClamp counter bump, so
// the "which boundary" context lives at the call site and the helper stays
// trivially testable. A nil pointer in fields is skipped defensively.
func ClampNegativeTokens(fields ...*int) bool {
	clamped := false
	for _, f := range fields {
		if f != nil && *f < 0 {
			*f = 0
			clamped = true
		}
	}
	return clamped
}

// ClampRecorder counts token events whose negative usage counts were clamped to
// zero at a parser boundary (a wire defect). It mirrors the store package's
// UnknownModelRecorder seam (prices.go): a nil recorder is a no-op, which keeps
// internal/collector off internal/metrics and means `tierd score` and unit
// tests need no wiring. Installed ONCE before serving via SetClampRecorder.
type ClampRecorder interface {
	Inc(labelValues ...string)
}

// clampRecorder holds the active recorder. atomic.Pointer mirrors the store
// package's unknownModelRecorder so a test can swap it without racing the
// concurrent parser goroutines that call RecordClamp under -race — a plain
// `var x ClampRecorder` assignment is an unsynchronized two-word interface
// write, latently unsafe when the proxy and watcher clamp concurrently.
var clampRecorder atomic.Pointer[ClampRecorder]

// SetClampRecorder installs (or clears, with nil) the recorder bumped once per
// clamped event. Called once from `tierd serve` startup with the
// tier_negative_tokens_clamped_total counter, next to SetUnknownModelRecorder;
// tests install a fake and restore with t.Cleanup(func(){ SetClampRecorder(nil) }).
func SetClampRecorder(r ClampRecorder) {
	if r == nil {
		clampRecorder.Store(nil)
		return
	}
	clampRecorder.Store(&r)
}

// RecordClamp bumps the negative-token clamp counter for the given source
// ("jsonl" or "proxy") if a recorder is installed. Callers invoke it once per
// clamped EVENT, not per field, so the metric reflects the count of defective
// events rather than the number of negative fields within them. A nil recorder
// (score path, tests) is a no-op.
//
// "Event" means one parser-boundary invocation: for the proxy paths that is one
// stored TokenEvent, but for JSONL it is one parsed assistant line — a negative-
// bearing streaming placeholder that is later superseded by the message.id dedup
// still counts here, because the clamp happens at the wire boundary before dedup.
// The counter is a wire-defect signal, so counting at the boundary is correct.
func RecordClamp(source string) {
	if p := clampRecorder.Load(); p != nil {
		(*p).Inc(source)
	}
}

// clampWarnSeen dedupes clamp WARN lines to one per (source, model) pair for the
// process lifetime, mirroring unknownModelSeen (prices.go).
var clampWarnSeen sync.Map

// WarnClamp emits a one-time WARN — deduped per (source, model) — that negative
// usage tokens were clamped at a parser boundary. Deliberately deduped, NOT
// per-event: a broken or hostile upstream emitting negatives on every request
// would otherwise flood the log on the hot proxy path. This mirrors
// warnUnknownModel's explicit anti-flood design; the unbounded per-event volume
// signal lives in the RecordClamp counter instead, keeping the fail-loud posture
// (loud once, countable always) without the log spam.
// The model name goes through logsafe.Str (#321 review, 2026-08-04). slog
// escapes CR/LF in both its Text and JSON handlers, so this was never a forgery
// — it is the LENGTH that was unbounded. Measured: slog caps nothing, and a 4 KB
// field produced a 4170-byte record. source is one of this package's own
// Source* constants and is printed bare, per logsafe's provably-bounded carve-out.
func WarnClamp(source, model string) {
	if _, loaded := clampWarnSeen.LoadOrStore(source+"\x00"+model, struct{}{}); loaded {
		return
	}
	slog.Warn("negative usage tokens clamped", "source", source, "model", logsafe.Str(model))
}
