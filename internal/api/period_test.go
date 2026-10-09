package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
)

func month(y int, m time.Month) Period {
	return Period{Kind: periodMonth, Start: time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)}
}

// TestParsePeriod_Grammar pins the ?period= grammar: exactly YYYY-MM inside
// [minPeriodYear, maxPeriodYear], parsed to a UTC month start that renders back
// to the same spelling, under the host zone and under a swapped time.Local.
//
// Do NOT add t.Parallel(): time.Local is process-global.
func TestParsePeriod_Grammar(t *testing.T) {
	valid := map[string]Period{
		"2026-08": month(2026, time.August),
		"2026-01": month(2026, time.January),
		"2026-12": month(2026, time.December),
		"2020-01": month(minPeriodYear, time.January),
		"2050-12": month(maxPeriodYear, time.December),
	}
	checkValid := func(zone string) {
		for in, want := range valid {
			got, err := parsePeriod(in)
			if err != nil {
				t.Errorf("[%s] parsePeriod(%q) = error %v, want %v", zone, in, err, want)
				continue
			}
			if got.Kind != periodMonth || !got.Start.Equal(want.Start) || got.Start.Location() != time.UTC {
				t.Errorf("[%s] parsePeriod(%q) = %+v, want %+v in UTC", zone, in, got, want)
			}
			if got.String() != in {
				t.Errorf("[%s] parsePeriod(%q).String() = %q, does not round-trip", zone, in, got.String())
			}
		}
	}
	checkValid("host")
	restoreLocal := time.Local
	time.Local = time.FixedZone("TIER-TEST-CHATHAM", 12*60*60+45*60)
	defer func() { time.Local = restoreLocal }()
	if _, off := time.Now().Zone(); off == 0 {
		t.Fatal("time.Local substitution did not take; a local-zone mutant would survive")
	}
	checkValid("time.Local=+12:45")
	for _, in := range []string{
		"", "2026-13", "2026-00", "2026-1", "2026-01-01", "2026", "26-08", "2026/08", "2026-Q3",
		"2019-12", "2051-01", "0202-05", "+2026-08",
		" 2026-08", "2026-08 ", "2026-08\n", "\t2026-08", "2026- 08",
	} {
		if got, err := parsePeriod(in); err == nil {
			t.Errorf("parsePeriod(%q) = %+v, want an error", in, got)
		}
	}
}

// TestPeriodBounds_UTCExclusiveEnd pins that a month's bounds are [first instant,
// first instant of the next month) in UTC, whatever zone Start was handed in.
func TestPeriodBounds_UTCExclusiveEnd(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load Asia/Kolkata: %v", err)
	}
	cases := []struct {
		p          Period
		start, end time.Time
	}{
		{month(2026, time.February), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{month(2024, time.February), time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
		{month(2026, time.December), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{
			Period{Kind: periodMonth, Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).In(kolkata)},
			time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		start, end := c.p.Bounds()
		if !start.Equal(c.start) || !end.Equal(c.end) {
			t.Errorf("%s.Bounds() = [%s, %s), want [%s, %s)", c.p, start, end, c.start, c.end)
		}
		if start.Location() != time.UTC || end.Location() != time.UTC {
			t.Errorf("%s.Bounds() zones = %v, %v, want UTC", c.p, start.Location(), end.Location())
		}
		// The end is the NEXT period's start: exclusive, so adjacent periods tile
		// with no instant in both and none in neither.
		next, _ := parsePeriod(c.end.Format(periodLayout))
		if ns, _ := next.Bounds(); !ns.Equal(end) {
			t.Errorf("%s ends at %s but the next month starts at %s", c.p, end, ns)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("Period{}.Bounds() returned; a kindless period must never yield a window")
		}
	}()
	Period{}.Bounds()
}

// TestLastSealable_GraceBoundaryExactInstant pins both sides of end + grace: at
// that instant the month is sealable, one nanosecond earlier it is not.
func TestLastSealable_GraceBoundaryExactInstant(t *testing.T) {
	for _, c := range []struct {
		p     Period
		grace time.Duration
	}{
		{month(2026, time.August), 14 * 24 * time.Hour},
		{month(2026, time.August), 24 * time.Hour},
		{month(2026, time.December), 14 * 24 * time.Hour},
		{month(2026, time.February), 14 * 24 * time.Hour},
	} {
		at := sealableAt(c.p, c.grace)
		_, end := c.p.Bounds()
		if !at.Equal(end.Add(c.grace)) {
			t.Fatalf("sealableAt(%s, %s) = %s, want end + grace = %s", c.p, c.grace, at, end.Add(c.grace))
		}
		if got := lastSealable(at, c.grace); got != c.p {
			t.Errorf("lastSealable(%s, %s) = %s, want %s: the month is sealable at exactly end + grace",
				at.Format(time.RFC3339Nano), c.grace, got, c.p)
		}
		if got, want := lastSealable(at.Add(-time.Nanosecond), c.grace), c.p.prev(); got != want {
			t.Errorf("lastSealable(%s, %s) = %s, want %s: one nanosecond before end + grace the month "+
				"is still in its grace period", at.Add(-time.Nanosecond).Format(time.RFC3339Nano), c.grace, got, want)
		}
	}
}

// TestLastSealable_IndependentOfHostZone pins that the sealable month is the same
// UTC month whatever zone the clock carries and whatever time.Local is (#180).
//
// Do NOT add t.Parallel(): time.Local is process-global.
func TestLastSealable_IndependentOfHostZone(t *testing.T) {
	grace := 14 * 24 * time.Hour
	boundary := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC) // August's sealableAt
	instants := []time.Time{
		boundary, boundary.Add(-time.Nanosecond), boundary.Add(-time.Hour), boundary.Add(time.Hour),
		time.Date(2027, 1, 14, 23, 30, 0, 0, time.UTC), time.Date(2027, 1, 15, 0, 30, 0, 0, time.UTC),
	}
	for _, name := range dstZones {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		for _, inst := range instants {
			want := lastSealable(inst, grace)
			got := lastSealable(inst.In(loc), grace)
			if got != want || got.Start.Location() != time.UTC {
				t.Errorf("lastSealable(%s in %s) = %+v, UTC gives %+v", inst.Format(time.RFC3339Nano), name, got, want)
			}
		}
	}

	restoreLocal := time.Local
	time.Local = time.FixedZone("TIER-TEST-CHATHAM", 12*60*60+45*60)
	defer func() { time.Local = restoreLocal }()
	if _, off := time.Now().Zone(); off == 0 {
		t.Fatal("time.Local substitution did not take; a local-zone mutant would survive")
	}
	for inst, want := range map[time.Time]Period{
		boundary:                         month(2026, time.August),
		boundary.Add(-time.Nanosecond):   month(2026, time.July),
		boundary.In(time.Local):          month(2026, time.August),
		boundary.Add(-time.Hour).Local(): month(2026, time.July),
	} {
		if got := lastSealable(inst, grace); got != want {
			t.Errorf("with time.Local=%s, lastSealable(%s) = %+v, want %+v",
				time.Local, inst.Format(time.RFC3339Nano), got, want)
		}
	}
}

// TestEarliestSealable_FirstFullMonth pins the first FULL month at or after the
// horizon, a month starting exactly at the horizon counting as full, the floor
// clamp, and ok=false (never a partial or default month) for a zero horizon or
// one with no full month by maxPeriodYear-12.
func TestEarliestSealable_FirstFullMonth(t *testing.T) {
	cases := []struct {
		name    string
		horizon time.Time
		want    Period
	}{
		{"mid-month gives the next month", time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC), month(2026, time.April)},
		{"exactly at the month start is full", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), month(2026, time.March)},
		{"one ns past the start is not", time.Date(2026, 3, 1, 0, 0, 0, 1, time.UTC), month(2026, time.April)},
		{"east-of-UTC midnight is before the UTC start", time.Date(2026, 3, 1, 0, 0, 0, 0, time.FixedZone("+0530", 330*60)), month(2026, time.March)},
		{"west-of-UTC Feb 28 evening is already March in UTC", time.Date(2026, 2, 28, 22, 0, 0, 0, time.FixedZone("-0500", -5*3600)), month(2026, time.April)},
		{"December rolls into the next year", time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC), month(2027, time.January)},
		{"below the floor clamps up", time.Date(2015, 6, 1, 0, 0, 0, 0, time.UTC), month(minPeriodYear, time.January)},
		{"last floor-year-minus-one month clamps up", time.Date(minPeriodYear-1, 12, 15, 0, 0, 0, 0, time.UTC), month(minPeriodYear, time.January)},
		{"the last month exactly at its start is full", time.Date(maxPeriodYear, 12, 1, 0, 0, 0, 0, time.UTC), month(maxPeriodYear, time.December)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := earliestSealable(c.horizon)
			if !ok || got != c.want {
				t.Fatalf("earliestSealable(%s) = %+v, %v, want %+v, true", c.horizon.Format(time.RFC3339Nano), got, ok, c.want)
			}
			if _, err := parsePeriod(got.String()); err != nil {
				t.Errorf("earliestSealable(%s) = %s, which parsePeriod refuses: %v", c.horizon, got, err)
			}
		})
	}
	for name, horizon := range map[string]time.Time{
		"no data (zero horizon)":        {},
		"mid last month: no full month": time.Date(maxPeriodYear, 12, 15, 0, 0, 0, 0, time.UTC),
		"one ns into the last month":    time.Date(maxPeriodYear, 12, 1, 0, 0, 0, 1, time.UTC),
		"far past the ceiling":          time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		if got, ok := earliestSealable(horizon); ok || got != (Period{}) {
			t.Errorf("%s: earliestSealable(%s) = %+v, %v, want zero, false", name, horizon.Format(time.RFC3339Nano), got, ok)
		}
	}
}

func periodRequest(rawQuery string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/scores", nil)
	r.URL.RawQuery = rawQuery
	return r
}

// TestRefuseFreeBounds_EachParam pins that every free bound is refused in every
// anonymised mode, on presence alone, with a 400 text naming ?period= and never
// naming another mode (#949, #959); and that the guard is inert in developer mode.
func TestRefuseFreeBounds_EachParam(t *testing.T) {
	anonymised := []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision}
	for _, m := range anonymised {
		if !m.Anonymized() {
			t.Fatalf("%s is not anonymised; the fixture tests nothing", m)
		}
	}
	checkText := func(t *testing.T, query string, err error, want ...string) {
		t.Helper()
		if err == nil {
			t.Errorf("?%s: refuseFreeBounds = nil, want a refusal", query)
			return
		}
		msg := err.Error()
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Errorf("?%s: refusal lacks %q: %s", query, w, msg)
			}
		}
		for _, banned := range []string{"developer", "aggregation"} {
			if strings.Contains(strings.ToLower(msg), banned) {
				t.Errorf("?%s: refusal names %q, a mode switch: %s", query, banned, msg)
			}
		}
	}

	params := []string{"since", "until", "before", "since_a", "until_a", "since_b", "until_b"}
	for _, m := range anonymised {
		for _, p := range params {
			for _, q := range []string{p + "=2026-08-01", p + "=", "period=2026-08&" + p + "=2026-08-01"} {
				checkText(t, q, refuseFreeBounds(periodRequest(q), m), "?period=", "?"+p+"=")
			}
		}
		q := "period=2026-08&since=2026-08-01"
		checkText(t, q, refuseFreeBounds(periodRequest(q), m), "?period=", "?since=")

		// A semicolon voids the whole query for r.URL.Query(), which would hide
		// the until; the guard must refuse rather than see a clean request.
		q = "period=2026-08&x=1;until=2026-09-01"
		if _, hidden := periodRequest(q).URL.Query()["until"]; hidden {
			t.Fatalf("control: Query() kept until in %q; this arm no longer proves the strict parse", q)
		}
		checkText(t, q, refuseFreeBounds(periodRequest(q), m), "?period=")

		for _, ok := range []string{"", "period=2026-08", "period_a=2026-07&period_b=2026-08", "team=a&repo=o/r"} {
			if err := refuseFreeBounds(periodRequest(ok), m); err != nil {
				t.Errorf("%s mode ?%s: refuseFreeBounds = %v, want nil", m, ok, err)
			}
		}
	}
	for _, p := range params {
		q := p + "=2026-08-01"
		if err := refuseFreeBounds(periodRequest(q), scoring.AggregationDeveloper); err != nil {
			t.Errorf("developer mode ?%s: refuseFreeBounds = %v, want nil", q, err)
		}
	}
}

// TestComparePeriods_BothOrNeither pins /compare's period pair: both or neither,
// each a valid month, never the same month twice, and a malformed query refused
// even when a lenient parse would still see both periods.
func TestComparePeriods_BothOrNeither(t *testing.T) {
	lenient := periodRequest("period_a=2026-07&period_b=2026-08&x=1;y=2").URL.Query()
	if lenient.Get("period_a") == "" || lenient.Get("period_b") == "" {
		t.Fatalf("control: Query() lost a period (%v); the malformed arm no longer proves the strict parse", lenient)
	}
	a, b, given, err := parseComparePeriods(periodRequest(""))
	if err != nil || given || a != (Period{}) || b != (Period{}) {
		t.Errorf("neither given: (%+v, %+v, %v, %v), want zero periods, given=false, nil", a, b, given, err)
	}
	a, b, given, err = parseComparePeriods(periodRequest("period_a=2026-07&period_b=2026-08"))
	if err != nil || !given || a != month(2026, time.July) || b != month(2026, time.August) {
		t.Errorf("both given: (%+v, %+v, %v, %v), want July, August, given=true, nil", a, b, given, err)
	}
	for _, q := range []string{
		"period_a=2026-07",
		"period_b=2026-08",
		"period_a=&period_b=2026-08",
		"period_a=2026-08&period_b=2026-08",
		"period_a=2026-13&period_b=2026-08",
		"period_a=2026-07&period_b=2026-8",
		"period_a=2026-07&period_b=2026-08&x=1;y=2",
	} {
		a, b, given, err := parseComparePeriods(periodRequest(q))
		if err == nil || given {
			t.Errorf("?%s: (%+v, %+v, given=%v, err=%v), want an error and given=false", q, a, b, given, err)
		}
	}
}

// TestParseRequestPeriod pins ?period= for /scores and /report_manifest: absent
// is the default read, present must parse (empty included), and a malformed
// query is refused even when a lenient parse would still see the period.
func TestParseRequestPeriod(t *testing.T) {
	if p, given, err := parseRequestPeriod(periodRequest("team=a")); err != nil || given || p != (Period{}) {
		t.Errorf("absent: (%+v, %v, %v), want zero, false, nil", p, given, err)
	}
	if p, given, err := parseRequestPeriod(periodRequest("period=2026-08")); err != nil || !given || p != month(2026, time.August) {
		t.Errorf("period=2026-08: (%+v, %v, %v), want August, true, nil", p, given, err)
	}
	malformed := "period=2026-08&x=1;y=2"
	if periodRequest(malformed).URL.Query().Get("period") != "2026-08" {
		t.Fatalf("control: Query() lost the period in %q; the malformed arm proves nothing", malformed)
	}
	for _, q := range []string{"period=", "period=2026-13", "period=2026-8", "period=2019-12", malformed} {
		if p, given, err := parseRequestPeriod(periodRequest(q)); err == nil || given {
			t.Errorf("?%s: (%+v, given=%v, err=%v), want an error and given=false", q, p, given, err)
		}
	}
}

// TestFreeBoundParams_CoversEveryWindowKey pins that refuseFreeBounds refuses
// every query key a window-date parser reads, with the keys taken from the
// package source rather than from a copy. The parsers are parseWindowDate, every
// function that calls it, and parseExportUntil (its own copy of the grammar). For
// each call to one of them, its argument is resolved to query keys: a
// .Get("key") literal; a .Get(param) whose literal comes from the enclosing
// function's call sites; or a local variable assigned from such a .Get. An
// argument of any other shape fails the test, so a new call pattern cannot pass
// unexamined.
func TestFreeBoundParams_CoversEveryWindowKey(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	callee := func(c *ast.CallExpr) string {
		switch fn := c.Fun.(type) {
		case *ast.Ident:
			return fn.Name
		case *ast.SelectorExpr:
			return fn.Sel.Name
		}
		return ""
	}
	callsIn := func(n ast.Node, name string) (out []*ast.CallExpr) {
		ast.Inspect(n, func(x ast.Node) bool {
			if c, ok := x.(*ast.CallExpr); ok && callee(c) == name {
				out = append(out, c)
			}
			return true
		})
		return out
	}

	parsers := map[string]bool{"parseWindowDate": true, "parseExportUntil": true}
	for name, fd := range funcs {
		if len(callsIn(fd, "parseWindowDate")) > 0 {
			parsers[name] = true
		}
	}
	for _, name := range []string{"parseWindowDate", "parseExportUntil", "parseSince", "parseUntil"} {
		if funcs[name] == nil || !parsers[name] {
			t.Fatalf("control: parser %s not found in the package source; the scan proves nothing", name)
		}
	}

	found := map[string]bool{}
	var keyOf func(e ast.Expr, fd *ast.FuncDecl)
	keyOf = func(e ast.Expr, fd *ast.FuncDecl) {
		if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			k, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			found[k] = true
			return
		}
		id, ok := e.(*ast.Ident)
		if !ok {
			t.Errorf("%s: window key %T in %s is not a literal or an identifier", fset.Position(e.Pos()), e, fd.Name.Name)
			return
		}
		idx := 0
		for _, field := range fd.Type.Params.List {
			for _, n := range field.Names {
				if n.Name == id.Name {
					sites := 0
					for _, f := range files {
						for _, c := range callsIn(f, fd.Name.Name) {
							sites++
							keyOf(c.Args[idx], fd)
						}
					}
					if sites == 0 {
						t.Errorf("%s: parameter %s of %s has no call site to read keys from", fset.Position(e.Pos()), id.Name, fd.Name.Name)
					}
					return
				}
				idx++
			}
		}
		t.Errorf("%s: window key %s in %s is not a parameter", fset.Position(e.Pos()), id.Name, fd.Name.Name)
	}
	var argOf func(e ast.Expr, fd *ast.FuncDecl)
	argOf = func(e ast.Expr, fd *ast.FuncDecl) {
		switch a := e.(type) {
		case *ast.CallExpr:
			if callee(a) == "Get" && len(a.Args) == 1 {
				keyOf(a.Args[0], fd)
				return
			}
		case *ast.Ident:
			assigned := 0
			ast.Inspect(fd.Body, func(x ast.Node) bool {
				as, ok := x.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != len(as.Rhs) {
					return true
				}
				for i, l := range as.Lhs {
					if lid, ok := l.(*ast.Ident); ok && lid.Name == a.Name {
						assigned++
						argOf(as.Rhs[i], fd)
					}
				}
				return true
			})
			if assigned > 0 {
				return
			}
		}
		t.Errorf("%s: a window-date parser in %s reads %T, not a query .Get", fset.Position(e.Pos()), fd.Name.Name, e)
	}
	for name, fd := range funcs {
		if parsers[name] {
			continue
		}
		for p := range parsers {
			for _, c := range callsIn(fd, p) {
				argOf(c.Args[0], fd)
			}
		}
	}

	for _, k := range []string{"since", "before", "since_a"} {
		if !found[k] {
			t.Fatalf("control: the scan did not find %q (literal, local-variable and parameter paths); found %v", k, found)
		}
	}
	refused := make(map[string]bool, len(freeBoundParams))
	for _, k := range freeBoundParams {
		refused[k] = true
	}
	for k := range found {
		if !refused[k] {
			t.Errorf("a window-date parser reads ?%s= but freeBoundParams does not list it, so refuseFreeBounds "+
				"lets it through in the anonymised modes", k)
		}
	}
}
