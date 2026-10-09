package metrics

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestExposition_Golden pins the full text exposition format: HELP/TYPE blocks,
// labelled + unlabelled series, deterministic (sorted) ordering, cumulative
// histogram buckets with _sum/_count/+Inf, and label-value escaping.
func TestExposition_Golden(t *testing.T) {
	r := NewRegistry()

	c := r.NewCounter("tier_requests_total", "total requests", "method")
	c.Inc("GET")
	c.Add(2, "POST")

	u := r.NewCounter("tier_starts_total", "process starts")
	u.Inc()

	g := r.NewGauge("tier_build_info", "build info", "version")
	g.Set(1, "v1.2.3")

	h := r.NewHistogram("tier_latency_seconds", "latency", []float64{1, 2, 5}, "route")
	for _, v := range []float64{0.5, 1.5, 3, 10} {
		h.Observe(v, "a")
	}

	e := r.NewCounter("tier_escaped_total", "esc", "l")
	e.Inc("a\"b\\c\n")

	var sb strings.Builder
	r.Render(&sb)

	want := `# HELP tier_requests_total total requests
# TYPE tier_requests_total counter
tier_requests_total{method="GET"} 1
tier_requests_total{method="POST"} 2
# HELP tier_starts_total process starts
# TYPE tier_starts_total counter
tier_starts_total 1
# HELP tier_build_info build info
# TYPE tier_build_info gauge
tier_build_info{version="v1.2.3"} 1
# HELP tier_latency_seconds latency
# TYPE tier_latency_seconds histogram
tier_latency_seconds_bucket{route="a",le="1"} 1
tier_latency_seconds_bucket{route="a",le="2"} 2
tier_latency_seconds_bucket{route="a",le="5"} 3
tier_latency_seconds_bucket{route="a",le="+Inf"} 4
tier_latency_seconds_sum{route="a"} 15
tier_latency_seconds_count{route="a"} 4
# HELP tier_escaped_total esc
# TYPE tier_escaped_total counter
tier_escaped_total{l="a\"b\\c\n"} 1
`
	if got := sb.String(); got != want {
		t.Errorf("exposition mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestHistogram_BucketBoundaryInclusive(t *testing.T) {
	// A value exactly equal to a bound falls into that bucket (le is inclusive).
	r := NewRegistry()
	h := r.NewHistogram("h", "h", []float64{1, 2})
	h.Observe(1) // exactly the first bound
	h.Observe(2) // exactly the second bound
	var sb strings.Builder
	r.Render(&sb)
	out := sb.String()
	// le=1 should hold the v=1 observation; le=2 should hold both.
	if !strings.Contains(out, `h_bucket{le="1"} 1`) {
		t.Errorf("v=1 should land in le=1 bucket:\n%s", out)
	}
	if !strings.Contains(out, `h_bucket{le="2"} 2`) || !strings.Contains(out, `h_bucket{le="+Inf"} 2`) {
		t.Errorf("le=2 and +Inf should both be 2:\n%s", out)
	}
}

func TestMustKey_ArityPanics(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("c", "c", "a", "b")
	defer func() {
		if recover() == nil {
			t.Error("expected panic on wrong label-value arity")
		}
	}()
	c.Inc("only-one") // declared 2 labels
}

func TestConcurrentRecording(t *testing.T) {
	// Exercises the per-vector mutex under -race.
	r := NewRegistry()
	c := r.NewCounter("c", "c", "k")
	h := r.NewHistogram("h", "h", []float64{1, 5}, "k")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc("x")
			h.Observe(2, "x")
		}()
	}
	wg.Wait()
	var sb strings.Builder
	r.Render(&sb)
	if !strings.Contains(sb.String(), `c{k="x"} 50`) {
		t.Errorf("counter should total 50:\n%s", sb.String())
	}
	if !strings.Contains(sb.String(), `h_count{k="x"} 50`) {
		t.Errorf("histogram count should be 50:\n%s", sb.String())
	}
}

// TestHistogram_OverflowBucket isolates the +Inf-only path: a value above all
// bounds increments _count and +Inf but no finite bucket (guards the overflow
// index counts[len(bounds)]).
func TestHistogram_OverflowBucket(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("h", "h", []float64{1, 2})
	h.Observe(99)
	var sb strings.Builder
	r.Render(&sb)
	out := sb.String()
	for _, want := range []string{
		`h_bucket{le="1"} 0`, `h_bucket{le="2"} 0`, `h_bucket{le="+Inf"} 1`,
		"h_sum 99", "h_count 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestHistogram_MultiSeries confirms per-label-series isolation and sorted output.
func TestHistogram_MultiSeries(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("h", "h", []float64{1, 5}, "route")
	h.Observe(0.5, "a")
	h.Observe(3, "b")
	var sb strings.Builder
	r.Render(&sb)
	out := sb.String()
	if !strings.Contains(out, `h_bucket{route="a",le="1"} 1`) || !strings.Contains(out, `h_count{route="a"} 1`) {
		t.Errorf("series a wrong:\n%s", out)
	}
	if !strings.Contains(out, `h_bucket{route="b",le="1"} 0`) || !strings.Contains(out, `h_bucket{route="b",le="5"} 1`) {
		t.Errorf("series b wrong:\n%s", out)
	}
	if strings.Index(out, `route="a"`) > strings.Index(out, `route="b"`) {
		t.Errorf("series not sorted (a before b):\n%s", out)
	}
}

// TestHelpEscaping exercises the HELP-string escaper (backslash + newline, not
// quotes) — the golden uses only plain HELP text, so this is the only coverage.
func TestHelpEscaping(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("c", "back\\slash and\nnewline")
	var sb strings.Builder
	r.Render(&sb)
	if !strings.Contains(sb.String(), `# HELP c back\\slash and\nnewline`) {
		t.Errorf("HELP not escaped:\n%s", sb.String())
	}
}

// TestFormatFloat_NoScientificNotation pins that large counters render as plain
// integers and small floats as plain decimals — never 1e+06 / 4.8e-05.
func TestFormatFloat_NoScientificNotation(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("big", "big").Add(1_500_000)
	r.NewGauge("frac", "frac").Set(0.000048208)
	var sb strings.Builder
	r.Render(&sb)
	out := sb.String()
	if !strings.Contains(out, "big 1500000") {
		t.Errorf("large counter should be a plain integer:\n%s", out)
	}
	if !strings.Contains(out, "frac 0.000048208") {
		t.Errorf("small float should be plain decimal:\n%s", out)
	}
	if strings.Contains(out, "e+") || strings.Contains(out, "e-") {
		t.Errorf("scientific notation leaked:\n%s", out)
	}
}

// TestDuplicateName_Panics and TestNonAscendingBounds_Panic pin the
// registration-time misuse contracts.
func TestDuplicateName_Panics(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("dup", "a")
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate metric name")
		}
	}()
	r.NewGauge("dup", "b")
}

func TestNonAscendingBounds_Panic(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Error("expected panic on non-ascending bounds")
		}
	}()
	r.NewHistogram("h", "h", []float64{5, 1})
}

// TestConcurrentRecordingAndRender stresses concurrent map GROWTH (distinct
// keys) while a renderer reads concurrently — proves writeTo's read-side lock.
func TestConcurrentRecordingAndRender(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("c", "c", "k")
	stop := make(chan struct{})
	var renderWG sync.WaitGroup
	renderWG.Add(1)
	go func() {
		defer renderWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				var sb strings.Builder
				r.Render(&sb)
			}
		}
	}()
	var writeWG sync.WaitGroup
	for i := 0; i < 50; i++ {
		writeWG.Add(1)
		go func(i int) {
			defer writeWG.Done()
			c.Inc("k" + strconv.Itoa(i))
		}(i)
	}
	writeWG.Wait()
	close(stop)
	renderWG.Wait()
	var sb strings.Builder
	r.Render(&sb)
	if got := strings.Count(sb.String(), `c{k="k`); got != 50 {
		t.Errorf("got %d distinct series, want 50", got)
	}
}
