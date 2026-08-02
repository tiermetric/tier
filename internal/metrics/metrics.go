// Package metrics is a tiny, dependency-free Prometheus exporter for tierd
// (#67). It implements exactly the metric types tier needs — labelled counters,
// gauges, and fixed-bucket histograms — and renders them in the Prometheus text
// exposition format (v0.0.4). It deliberately does NOT pull
// github.com/prometheus/client_golang: tier exposes a small, fixed metric set
// with static label sets, so client_golang's registry/cardinality/collector
// machinery would be ~6-10 transitive dependencies of unused weight against a
// project whose identity is a lean, digest-pinned static binary (3 direct deps).
//
// Concurrency: each metric vector guards its own series map with a mutex.
// Vectors are registered once at startup (single goroutine) and thereafter only
// recorded-into and rendered, so the Registry's vector list needs no lock.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// seriesSep joins label values into a map key. NUL can't appear in a label
// value we emit (HTTP method/route/status-class, version), so it's a safe,
// collision-free separator.
const seriesSep = "\x00"

// Registry holds the registered metric vectors and renders them.
type Registry struct {
	families []family
	names    map[string]bool // guards against duplicate metric names
}

// family is one named metric (its HELP/TYPE block plus all its label series).
type family interface {
	writeTo(w io.Writer)
}

func NewRegistry() *Registry { return &Registry{names: map[string]bool{}} }

// register appends a family, panicking on a duplicate name. A duplicate would
// emit two TYPE blocks for one metric and make Prometheus reject the whole
// scrape; since metrics are registered once at startup, a panic surfaces the
// programmer error immediately (in any test that touches the registry) rather
// than as a silent runtime scrape failure.
func (r *Registry) register(name string, f family) {
	if r.names[name] {
		panic("metrics: duplicate metric name " + name)
	}
	r.names[name] = true
	r.families = append(r.families, f)
}

// Render writes the whole registry in Prometheus text exposition format.
// Families render in registration order; series within a family are sorted for
// deterministic output (stable scrapes, golden-testable). (Named Render, not
// WriteTo, to avoid implying the io.WriterTo (int64, error) contract.)
func (r *Registry) Render(w io.Writer) {
	for _, f := range r.families {
		f.writeTo(w)
	}
}

// --- CounterVec ---

type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	vals       map[string]float64
}

// NewCounter registers a counter family. labels are the label NAMES; record
// with Inc/Add by passing the matching label VALUES in the same order.
func (r *Registry) NewCounter(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels, vals: map[string]float64{}}
	r.register(name, c)
	return c
}

func (c *CounterVec) Inc(labelValues ...string) { c.Add(1, labelValues...) }

func (c *CounterVec) Add(delta float64, labelValues ...string) {
	k := mustKey(c.labels, labelValues)
	c.mu.Lock()
	c.vals[k] += delta
	c.mu.Unlock()
}

func (c *CounterVec) writeTo(w io.Writer) {
	writeHeader(w, c.name, c.help, "counter")
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range sortedKeys(c.vals) {
		_, _ = fmt.Fprintf(w, "%s%s %s\n", c.name, labelBlock(c.labels, k), formatFloat(c.vals[k]))
	}
}

// --- GaugeVec ---

type GaugeVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	vals       map[string]float64
}

func (r *Registry) NewGauge(name, help string, labels ...string) *GaugeVec {
	g := &GaugeVec{name: name, help: help, labels: labels, vals: map[string]float64{}}
	r.register(name, g)
	return g
}

func (g *GaugeVec) Set(v float64, labelValues ...string) {
	k := mustKey(g.labels, labelValues)
	g.mu.Lock()
	g.vals[k] = v
	g.mu.Unlock()
}

func (g *GaugeVec) writeTo(w io.Writer) {
	writeHeader(w, g.name, g.help, "gauge")
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range sortedKeys(g.vals) {
		_, _ = fmt.Fprintf(w, "%s%s %s\n", g.name, labelBlock(g.labels, k), formatFloat(g.vals[k]))
	}
}

// --- HistogramVec ---

type HistogramVec struct {
	name, help string
	labels     []string
	bounds     []float64 // ascending bucket upper bounds (le); +Inf is implicit
	mu         sync.Mutex
	series     map[string]*histData
}

type histData struct {
	counts []uint64 // per-bucket, len == len(bounds)+1 (last == the +Inf-only overflow)
	sum    float64
	count  uint64
}

// NewHistogram registers a histogram family. bounds are the bucket upper bounds
// (le); the +Inf bucket is implicit. bounds must be non-empty and strictly
// ascending — Observe relies on sort.Search over them, so a violation would
// silently mis-bucket. Since buckets are declared once at startup, the panic
// surfaces a bad bucket ladder immediately rather than as wrong production data.
func (r *Registry) NewHistogram(name, help string, bounds []float64, labels ...string) *HistogramVec {
	if len(bounds) == 0 {
		panic("metrics: histogram " + name + " needs at least one bucket bound")
	}
	for i := 1; i < len(bounds); i++ {
		if bounds[i] <= bounds[i-1] {
			panic("metrics: histogram " + name + " bounds must be strictly ascending")
		}
	}
	h := &HistogramVec{name: name, help: help, labels: labels, bounds: bounds, series: map[string]*histData{}}
	r.register(name, h)
	return h
}

func (h *HistogramVec) Observe(v float64, labelValues ...string) {
	k := mustKey(h.labels, labelValues)
	// First bound >= v; len(bounds) means v exceeds all bounds (+Inf overflow).
	idx := sort.Search(len(h.bounds), func(i int) bool { return v <= h.bounds[i] })
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.series[k]
	if d == nil {
		d = &histData{counts: make([]uint64, len(h.bounds)+1)}
		h.series[k] = d
	}
	d.counts[idx]++
	d.sum += v
	d.count++
}

func (h *HistogramVec) writeTo(w io.Writer) {
	writeHeader(w, h.name, h.help, "histogram")
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range sortedKeys(h.series) {
		d := h.series[k]
		var cum uint64
		for i, b := range h.bounds {
			cum += d.counts[i]
			_, _ = fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelBlockWith(h.labels, k, "le", formatFloat(b)), cum)
		}
		cum += d.counts[len(h.bounds)] // +Inf overflow
		_, _ = fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelBlockWith(h.labels, k, "le", "+Inf"), cum)
		_, _ = fmt.Fprintf(w, "%s_sum%s %s\n", h.name, labelBlock(h.labels, k), formatFloat(d.sum))
		_, _ = fmt.Fprintf(w, "%s_count%s %d\n", h.name, labelBlock(h.labels, k), d.count)
	}
}

// --- shared rendering helpers ---

func writeHeader(w io.Writer, name, help, typ string) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
}

// mustKey validates the label-value arity and joins values into a series key.
// A mismatch is a programmer error (the metric was declared with N labels but
// recorded with M), so it panics — caught immediately in tests, never in prod
// since label sets are static.
func mustKey(labels, values []string) string {
	if len(labels) != len(values) {
		panic(fmt.Sprintf("metrics: %d label values for %d labels", len(values), len(labels)))
	}
	return strings.Join(values, seriesSep)
}

func splitKey(k string) []string {
	if k == "" {
		return nil
	}
	return strings.Split(k, seriesSep)
}

// labelBlock renders `{name="val",...}` for a series key, or "" when unlabelled.
func labelBlock(names []string, key string) string {
	return labelBlockWith(names, key, "", "")
}

// labelBlockWith renders the series labels plus one optional extra label
// (used for the histogram `le`). The extra label is emitted last.
func labelBlockWith(names []string, key, extraName, extraVal string) string {
	vals := splitKey(key)
	if len(names) == 0 && extraName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, n, escapeLabel(vals[i]))
	}
	if extraName != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, extraName, escapeLabel(extraVal))
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabel escapes a label VALUE per the exposition format: backslash, double
// quote, and newline.
func escapeLabel(s string) string {
	return labelEscaper.Replace(s)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// escapeHelp escapes a HELP string: backslash and newline (not quotes).
func escapeHelp(s string) string {
	return helpEscaper.Replace(s)
}

var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

// formatFloat renders a sample value. Whole numbers (counters, counts, the
// integer bucket bounds) render as plain integers; everything else uses 'f'
// with shortest round-trippable precision. Deliberately NOT 'g': that switches
// to scientific notation (e.g. 1e+06) above ~1e6, which is surprising for an
// integer counter and trips stricter exposition consumers.
func formatFloat(v float64) string {
	if !math.IsInf(v, 0) && !math.IsNaN(v) && v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
