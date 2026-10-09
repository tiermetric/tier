package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
)

// periodKind is the size of a sealed reporting period (#913). Month is the only
// kind today; a quarter is an additive case in each switch below and a new
// spelling in parsePeriod. The zero value is deliberately no kind at all, so an
// unparsed Period{} is never mistaken for a month.
type periodKind int

const (
	periodMonth periodKind = iota + 1
)

// String is the kind's stored sealed_report.period_size.
func (k periodKind) String() string {
	switch k {
	case periodMonth:
		return "month"
	default:
		return fmt.Sprintf("invalid-kind(%d)", int(k))
	}
}

// Period is one sealed reporting period. Start is the period's first instant in
// UTC; every constructor in this file guarantees it.
type Period struct {
	Kind  periodKind
	Start time.Time
}

// periodLayout is the one ?period= spelling for a month.
const periodLayout = "2006-01"

// parsePeriod reads ?period= strictly: exactly YYYY-MM, a year inside
// [minPeriodYear, maxPeriodYear]. The grammar is validatePeriod's, so a billing
// period and a report period can never accept different spellings.
func parsePeriod(s string) (Period, error) {
	if err := validatePeriod(s); err != nil {
		return Period{}, err
	}
	t, err := time.Parse(periodLayout, s)
	if err != nil {
		return Period{}, fmt.Errorf("period must be YYYY-MM (e.g. 2026-05), got %q", s)
	}
	return Period{Kind: periodMonth, Start: t.UTC()}, nil
}

// String renders the period in the spelling parsePeriod accepts.
func (p Period) String() string {
	switch p.Kind {
	case periodMonth:
		return p.Start.UTC().Format(periodLayout)
	default:
		return fmt.Sprintf("invalid-period(%d)", p.Kind)
	}
}

// Bounds returns the half-open UTC window [start, end) the period covers.
func (p Period) Bounds() (start, end time.Time) {
	start = p.Start.UTC()
	switch p.Kind {
	case periodMonth:
		return start, start.AddDate(0, 1, 0)
	default:
		panic(fmt.Sprintf("api: Bounds on a Period with no kind (%d); construct periods with parsePeriod", p.Kind))
	}
}

// prev returns the period immediately before p, of the same kind.
func (p Period) prev() Period {
	switch p.Kind {
	case periodMonth:
		return Period{Kind: periodMonth, Start: p.Start.UTC().AddDate(0, -1, 0)}
	default:
		panic(fmt.Sprintf("api: prev on a Period with no kind (%d)", p.Kind))
	}
}

// next returns the period immediately after p, of the same kind.
func (p Period) next() Period {
	switch p.Kind {
	case periodMonth:
		return Period{Kind: periodMonth, Start: p.Start.UTC().AddDate(0, 1, 0)}
	default:
		panic(fmt.Sprintf("api: next on a Period with no kind (%d)", p.Kind))
	}
}

// monthOf returns the month containing t, computed in UTC whatever zone t
// carries (#180): the host's zone never moves a month boundary.
func monthOf(t time.Time) Period {
	u := t.UTC()
	return Period{Kind: periodMonth, Start: time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)}
}

// sealableAt is the first instant p may be sealed: its exclusive end plus grace.
func sealableAt(p Period, grace time.Duration) time.Time {
	_, end := p.Bounds()
	return end.Add(grace)
}

// lastSealable returns the latest month whose end + grace <= now. The month
// containing now − grace has not ended by then, so the answer is always the one
// before it; now − grace landing exactly on a month start seals the month that
// ends there.
func lastSealable(now time.Time, grace time.Duration) Period {
	return monthOf(now.UTC().Add(-grace)).prev()
}

// earliestSealable returns the first FULL month at or after the cost-coverage
// horizon: a month starting exactly at the horizon counts as full. A horizon
// below minPeriodYear yields minPeriodYear-01. ok is false for a zero horizon
// (no cost data) and when no full month exists at or before maxPeriodYear-12, so
// a returned period is always full and always one parsePeriod accepts.
func earliestSealable(costCoverageStart time.Time) (p Period, ok bool) {
	if costCoverageStart.IsZero() {
		return Period{}, false
	}
	p = monthOf(costCoverageStart)
	if p.Start.Before(costCoverageStart) {
		p = Period{Kind: periodMonth, Start: p.Start.AddDate(0, 1, 0)}
	}
	if lo := time.Date(minPeriodYear, time.January, 1, 0, 0, 0, 0, time.UTC); p.Start.Before(lo) {
		return Period{Kind: periodMonth, Start: lo}, true
	}
	if p.Start.Year() > maxPeriodYear {
		return Period{}, false
	}
	return p, true
}

// freeBoundParams are the window parameters an anonymised read never accepts:
// a free bound lets two overlapping windows be differenced down to one person.
// It must list every query key a window-date parser (parseSince, parseUntil,
// parseWindowUpperBound, parseCompareWindow) reads.
var freeBoundParams = []string{"since", "until", "before", "since_a", "until_a", "since_b", "until_b"}

// The remedies refuseFreeBoundsWith names: /scores and /report_manifest select
// one month, /scores/compare two.
const (
	periodRemedy        = "select a month with ?period=YYYY-MM (none: the latest sealed month)"
	comparePeriodRemedy = "select two months with ?period_a=YYYY-MM and ?period_b=YYYY-MM (neither: the two latest sealed months)"
)

// refuseFreeBounds is refuseFreeBoundsWith naming ?period=.
func refuseFreeBounds(r *http.Request, mode scoring.AggregationMode) error {
	return refuseFreeBoundsWith(r, mode, periodRemedy)
}

// refuseFreeBoundsWith refuses free window bounds in the anonymised modes (#913),
// backed by the mode-dependent parameter allowlists the read path adds. It
// returns nil in a non-anonymised mode. A parameter is refused on presence,
// whatever its value. Its text names remedy and never names another mode
// (#949, #959).
func refuseFreeBoundsWith(r *http.Request, mode scoring.AggregationMode, remedy string) error {
	if !mode.Anonymized() {
		return nil
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return fmt.Errorf("malformed query string: %v; %s", err, remedy)
	}
	var given []string
	for _, k := range freeBoundParams {
		if _, ok := q[k]; ok {
			given = append(given, "?"+k+"=")
		}
	}
	if len(given) == 0 {
		return nil
	}
	return fmt.Errorf("%s not accepted for anonymised reports (#913): they are served per sealed "+
		"calendar month, so two overlapping windows cannot be differenced to isolate one person's "+
		"figures. Remove %s and %s", strings.Join(given, ", "), strings.Join(given, ", "), remedy)
}

// queryPeriod reads one period parameter: absent is (zero, false, nil); present
// is parsed, and present-but-empty is an error, never the default read.
func queryPeriod(q url.Values, key string) (Period, bool, error) {
	if _, ok := q[key]; !ok {
		return Period{}, false, nil
	}
	p, err := parsePeriod(q.Get(key))
	if err != nil {
		return Period{}, true, fmt.Errorf("invalid %s: %w", key, err)
	}
	return p, true, nil
}

// parseRequestPeriod reads ?period= for /scores and /report_manifest: absent is
// (zero, false, nil), the default read; anything present must parse.
func parseRequestPeriod(r *http.Request) (p Period, given bool, err error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return Period{}, false, fmt.Errorf("malformed query string: %v", err)
	}
	if p, given, err = queryPeriod(q, "period"); err != nil {
		return Period{}, false, err
	}
	return p, given, nil
}

// parseComparePeriods reads /compare's ?period_a= and ?period_b=: both or
// neither (given=false, nil error), never one, and never the same period twice.
func parseComparePeriods(r *http.Request) (a, b Period, given bool, err error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return Period{}, Period{}, false, fmt.Errorf("malformed query string: %v", err)
	}
	a, hasA, errA := queryPeriod(q, "period_a")
	b, hasB, errB := queryPeriod(q, "period_b")
	if !hasA && !hasB {
		return Period{}, Period{}, false, nil
	}
	if hasA != hasB {
		return Period{}, Period{}, false, errors.New("?period_a= and ?period_b= must be given together, " +
			"or both omitted for the two latest sealed months")
	}
	for _, e := range []error{errA, errB} {
		if e != nil {
			return Period{}, Period{}, false, e
		}
	}
	if a.Kind == b.Kind && a.Start.Equal(b.Start) {
		return Period{}, Period{}, false, fmt.Errorf("?period_a= and ?period_b= name the same period %s; "+
			"a comparison needs two different periods", a)
	}
	return a, b, true, nil
}
