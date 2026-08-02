package health

import (
	"fmt"
	"sort"
	"sync"
)

// Snapshotter is the contract a subsystem implements to appear in the
// /healthz body (#48). A subsystem registers itself into a Registry and,
// on every /healthz hit, the handler calls HealthSnapshot to obtain the
// subsystem's current health.
//
// Implementations MUST be:
//
//   - Concurrency-safe — /healthz can be polled from multiple probes while the
//     subsystem mutates its own state. WatcherState satisfies this (its
//     Snapshot takes an RWMutex).
//   - Non-blocking and I/O-free — HealthSnapshot runs INLINE on every /healthz
//     request, which is unauthenticated and pull-based, while the Registry
//     holds a read lock. A collector that fronts an external API (Anthropic
//     Admin, OpenAI Usage, ...) must return CACHED state here and never call
//     that upstream from HealthSnapshot; doing so would turn the open readiness
//     probe into a latency/DoS amplifier and could stall the shared lock.
//
// Future collectors (Anthropic Admin, OpenAI Usage, Copilot, Cursor) each
// implement this the same way instead of adding a new top-level /healthz key.
type Snapshotter interface {
	HealthSnapshot() SubsystemSnapshot
}

// SubsystemSnapshot is the generic, subsystem-agnostic health view rendered
// under a subsystem's name in the /healthz body. It carries just enough for
// a consumer (dashboard, kube probe, Prometheus exporter) to act without
// knowing the subsystem's concrete type:
//
//   - Healthy drives the aggregate 200/503 decision. A subsystem in a
//     legitimate idle mode (e.g. the watcher's not_configured state) reports
//     true — idle is not a fault.
//   - Detail is the subsystem-specific payload (for the watcher, a
//     WatcherSnapshot). It is deliberately typed `any`: the Registry is a
//     heterogeneous collection of independent subsystems, so a single
//     concrete detail type would couple them. Each Detail is a small,
//     JSON-serialisable value the owning subsystem controls; consumers that
//     only need the health verdict read Healthy and ignore Detail.
type SubsystemSnapshot struct {
	Healthy bool `json:"healthy"`
	Detail  any  `json:"detail,omitempty"`
}

// Registry is the set of named subsystems whose health composes the /healthz
// response (#48). It replaces the previous hard-coded single-subsystem body:
// subsystems register once at startup and the handler reads them all on each
// probe, so adding a subsystem no longer forces every consumer to learn a new
// top-level key.
//
// The zero value is not usable; construct with NewRegistry. Register is
// expected during single-goroutine startup wiring, Snapshot from concurrent
// /healthz handlers — both are guarded so a late Register never races an
// in-flight probe.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Snapshotter
}

// NewRegistry returns an empty Registry ready to accept Register calls.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Snapshotter)}
}

// Register adds a subsystem under name. It panics on an empty name, a nil
// snapshotter, or a duplicate name — all three are startup-wiring bugs, not
// runtime conditions, so failing loud at boot (like http.Handle) is safer
// than silently dropping or overwriting a subsystem's health.
func (r *Registry) Register(name string, s Snapshotter) {
	if name == "" {
		panic("health: Register called with empty name")
	}
	if s == nil {
		panic(fmt.Sprintf("health: Register(%q) called with nil Snapshotter", name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		panic(fmt.Sprintf("health: subsystem %q already registered", name))
	}
	r.byName[name] = s
}

// Snapshot returns a point-in-time health view of every registered subsystem,
// keyed by name. The returned map is freshly allocated and owned by the
// caller. Each subsystem's HealthSnapshot is invoked under a read lock held
// only for the map iteration; the returned map never aliases Registry state.
func (r *Registry) Snapshot() map[string]SubsystemSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]SubsystemSnapshot, len(r.byName))
	for name, s := range r.byName {
		out[name] = s.HealthSnapshot()
	}
	return out
}

// AllHealthy reports whether every subsystem snapshot in m is healthy — the
// aggregate that drives the /healthz 200/503 status code. It takes an already
// captured snapshot map (rather than re-reading the Registry) so the status
// code and the rendered body are derived from one consistent view, and each
// subsystem's HealthSnapshot is invoked exactly once per probe. An empty map
// is vacuously healthy (there is nothing degraded to report).
//
// Every subsystem is treated as EQUALLY readiness-critical: one unhealthy
// subsystem 503s the whole probe. That is correct while the only subsystem is
// the watcher (whose terminal failure already shuts the process down). If a
// future NON-critical collector is registered (e.g. a lagging usage poller
// that should report degraded without evicting the pod from its Service),
// SubsystemSnapshot will need a criticality flag and this aggregate must honour
// it — deliberately deferred (YAGNI) until such a collector actually exists.
func AllHealthy(m map[string]SubsystemSnapshot) bool {
	for _, s := range m {
		if !s.Healthy {
			return false
		}
	}
	return true
}

// Names returns the registered subsystem names in sorted order. Intended for
// tests and diagnostics; the /healthz body keys the subsystems by name so
// callers there do not need this.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
