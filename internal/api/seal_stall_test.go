package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// stallKeys are the #913-D8 ruling C fields a sealed read names a stall with.
var stallKeys = []string{"stalled_period", "stall_reason", "owed_since"}

// stallOf is a decoded body's stall fields, "" for each one absent.
func stallOf(t *testing.T, rec *httptest.ResponseRecorder) [3]string {
	t.Helper()
	body := decodeJSON(t, rec)
	var got [3]string
	for i, k := range stallKeys {
		if v, has := body[k]; has {
			got[i], _ = v.(string)
			if got[i] == "" {
				t.Errorf("%s is present and not a non-empty string: %#v", k, v)
			}
		}
	}
	return got
}

// TestSealStall_ManifestAnd404NameTheStall (#913-D8 ruling C conditions 1 and
// 4): with June's seal failing for good, the manifest and June's 404 name June,
// the failure's category code and June's sealable_at, never the error's text;
// and neither the manifest nor the /scores header promises a next seal time.
func TestSealStall_ManifestAnd404NameTheStall(t *testing.T) {
	h, _, s, _ := juneStuck(t, sealAug1)
	if err := s.sealDue(context.Background()); !errors.Is(err, errSealRefoldMismatch) {
		t.Fatalf("control: June's pass: %v, want errSealRefoldMismatch", err)
	}
	want := [3]string{"2026-06", "refold_mismatch", "2026-07-15T00:00:00Z"}
	man := sealedGet(t, h, "/api/v1/report_manifest")
	if got := stallOf(t, man); man.Code != http.StatusOK || got != want {
		t.Errorf("manifest: %d %v, want 200 naming %v", man.Code, got, want)
	}
	if _, has := decodeJSON(t, man)["next_seal_at"]; has {
		t.Errorf("stalled manifest carries next_seal_at: %s", man.Body.String())
	}
	if sc := sealedGet(t, h, "/api/v1/scores"); sc.Code != http.StatusOK || sc.Header().Values(headerNextSealAt) != nil {
		t.Errorf("stalled /scores: %d %s=%q, want 200 and no %s", sc.Code, headerNextSealAt, sc.Header().Values(headerNextSealAt), headerNextSealAt)
	}
	for path, rec := range map[string]*httptest.ResponseRecorder{
		"June's /scores":       sealedGet(t, h, "/api/v1/scores?period=2026-06"),
		"July's manifest":      sealedGet(t, h, "/api/v1/report_manifest?period=2026-07"),
		"May..June's /compare": compareGet(t, h, "?period_a=2026-05&period_b=2026-06"),
	} {
		if got := stallOf(t, rec); rec.Code != http.StatusNotFound || got != want ||
			strings.Contains(rec.Body.String(), errSealRefoldMismatch.Error()) {
			t.Errorf("%s: %d %v %s; want a 404 naming %v and no error text", path, rec.Code, got, rec.Body.String(), want)
		}
	}
}

// TestSealStall_AbsentUntilOverdue (#913-D8 ruling C condition 2): with no
// pass reporting, June is named only once it is past its sealable_at by
// sealOverdueTicks passes, then with the unreported code; until then the
// manifest names no stall and promises June's seal time.
func TestSealStall_AbsentUntilOverdue(t *testing.T) {
	h, _, s := newSealedReadHandler(t)
	sealPass(t, s) // May
	deadline := sealableAt(Period{Kind: periodMonth, Start: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)}, sealGrace).Add(2 * sealTick)
	s.now = func() time.Time { return deadline }
	man := sealedGet(t, h, "/api/v1/report_manifest")
	if got := stallOf(t, man); man.Code != http.StatusOK || got != [3]string{} || decodeJSON(t, man)["next_seal_at"] != "2026-08-15T00:00:00Z" {
		t.Errorf("at the deadline: %d %v %s; want no stall and the next month's next_seal_at", man.Code, got, man.Body.String())
	}
	s.now = func() time.Time { return deadline.Add(time.Second) }
	want := [3]string{"2026-06", sealUnreported.code, "2026-07-15T00:00:00Z"}
	if got := stallOf(t, sealedGet(t, h, "/api/v1/report_manifest")); got != want {
		t.Errorf("past the deadline: %v, want %v", got, want)
	}
}

// TestSealStall_OverdueAfterRestart (#913-D8 ruling C condition 2): a server
// restarted after June's seal failed has no pass's failure in memory, and still
// names June, as unreported, and its 404 promises no next pass.
func TestSealStall_OverdueAfterRestart(t *testing.T) {
	h, _, s, _ := juneStuck(t, sealAug1)
	if err := s.sealDue(context.Background()); err == nil {
		t.Fatal("control: June's pass sealed")
	}
	restarted := newTestSealer(h)
	restarted.now = s.now
	h.sealer = restarted
	want := [3]string{"2026-06", sealUnreported.code, "2026-07-15T00:00:00Z"}
	if got := stallOf(t, sealedGet(t, h, "/api/v1/report_manifest")); got != want {
		t.Errorf("manifest after a restart: %v, want %v", got, want)
	}
	rec := sealedGet(t, h, "/api/v1/scores?period=2026-06")
	if body := rec.Body.String(); stallOf(t, rec) != want || !strings.Contains(body, "no seal pass has reported on it") ||
		strings.Contains(body, "next seal pass") {
		t.Errorf("June's 404 after a restart: %s; want it named as unreported and no next pass", body)
	}
}

// TestSealStall_StaleStallClearedBySkip (#913-D8 ruling C condition 3): after
// a CLI skip records June as a gap, the pass's failure still held in memory
// names June, and no read names it: July is first owed and not yet overdue.
func TestSealStall_StaleStallClearedBySkip(t *testing.T) {
	h, db, s, sk := juneStuck(t, sealAug1)
	ctx := context.Background()
	if err := s.sealDue(ctx); err == nil {
		t.Fatal("control: June's pass sealed")
	}
	if _, err := sk.skip(ctx, "2026-06", "cannot refold (OPS-12)", true, "refold_mismatch"); err != nil {
		t.Fatal(err)
	}
	h.store = db
	if st := s.stall.Load(); st == nil || st.month.String() != "2026-06" {
		t.Fatalf("control: in-memory stall %v, want June's", st)
	}
	man := sealedGet(t, h, "/api/v1/report_manifest")
	if got := stallOf(t, man); man.Code != http.StatusOK || got != [3]string{} || decodeJSON(t, man)["next_seal_at"] != "2026-08-15T00:00:00Z" {
		t.Errorf("manifest after the skip: %d %v %s; want no stall and July's next_seal_at", man.Code, got, man.Body.String())
	}
	if rec := sealedGet(t, h, "/api/v1/scores?period=2026-07"); stallOf(t, rec) != [3]string{} || strings.Contains(rec.Body.String(), "stalled at") {
		t.Errorf("July's 404 after the skip names a stall: %s", rec.Body.String())
	}
}

// TestSealStall_NeverOnHealthz (#913-D8 ruling C condition 1): /healthz and
// /livez are unauthenticated, so in team mode with a stall they carry no stall
// field, month or category, and the stall does not move /healthz's status.
func TestSealStall_NeverOnHealthz(t *testing.T) {
	h, _, s, _ := juneStuck(t, sealAug1)
	if err := s.sealDue(context.Background()); err == nil {
		t.Fatal("control: June's pass sealed")
	}
	if got := stallOf(t, sealedGet(t, h, "/api/v1/report_manifest")); got[0] != "2026-06" {
		t.Fatalf("control: manifest stall %v, want June", got)
	}
	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		h.Register(mux)
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	for _, path := range []string{"/api/v1/healthz", "/api/v1/livez", "/api/v1/health"} {
		code, body := get(path)
		for _, leak := range append(stallKeys, "stall", "2026-06", "refold_mismatch", "owed") {
			if strings.Contains(body, leak) {
				t.Errorf("%s carries %q: %s", path, leak, body)
			}
		}
		h.sealer = nil
		if unsealed, _ := get(path); unsealed != code {
			t.Errorf("%s: %d with the stall, %d with no sealer; want the same status", path, code, unsealed)
		}
		h.sealer = s
	}
}

// TestSealStall_LiveFailureNamedBeforeDeadline (#913-D6 ruling D, D8 ruling C
// condition 4): a pass that fails at June an hour after June's sealable_at,
// inside its sealOverdueTicks passes, is named at once with its category, and
// neither the manifest, the /scores header nor June's or July's 404 promises a
// next seal. At the same clock with no pass's failure reported, nothing is
// named and June's 404 promises the next pass.
func TestSealStall_LiveFailureNamedBeforeDeadline(t *testing.T) {
	june := Period{Kind: periodMonth, Start: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)}
	h, _, s, _ := juneStuck(t, sealableAt(june, sealGrace).Add(time.Hour))
	if err := s.sealDue(context.Background()); !errors.Is(err, errSealRefoldMismatch) {
		t.Fatalf("control: June's pass: %v, want errSealRefoldMismatch", err)
	}
	want := [3]string{"2026-06", "refold_mismatch", "2026-07-15T00:00:00Z"}
	man := sealedGet(t, h, "/api/v1/report_manifest")
	if _, has := decodeJSON(t, man)["next_seal_at"]; man.Code != http.StatusOK || stallOf(t, man) != want || has {
		t.Errorf("manifest: %d %s; want 200 naming %v and no next_seal_at", man.Code, man.Body.String(), want)
	}
	if sc := sealedGet(t, h, "/api/v1/scores"); sc.Code != http.StatusOK || sc.Header().Values(headerNextSealAt) != nil {
		t.Errorf("/scores: %d %s=%q, want 200 and no %s", sc.Code, headerNextSealAt, sc.Header().Values(headerNextSealAt), headerNextSealAt)
	}
	for _, month := range []string{"2026-06", "2026-07"} {
		rec := sealedGet(t, h, "/api/v1/scores?period="+month)
		if rec.Code != http.StatusNotFound || stallOf(t, rec) != want || strings.Contains(rec.Body.String(), "next seal pass") {
			t.Errorf("%s's 404: %d %s; want a 404 naming %v and no next seal pass", month, rec.Code, rec.Body.String(), want)
		}
	}

	restarted := newTestSealer(h)
	restarted.now = s.now
	h.sealer = restarted
	man = sealedGet(t, h, "/api/v1/report_manifest")
	if stallOf(t, man) != [3]string{} || decodeJSON(t, man)["next_seal_at"] != "2026-08-15T00:00:00Z" {
		t.Errorf("no failure reported: manifest %s; want no stall and July's next_seal_at", man.Body.String())
	}
	if rec := sealedGet(t, h, "/api/v1/scores?period=2026-06"); stallOf(t, rec) != [3]string{} ||
		!strings.Contains(rec.Body.String(), "next seal pass") {
		t.Errorf("no failure reported: June's 404 %s; want no stall and the next seal pass", rec.Body.String())
	}
}

// TestSealStall_NotUnreportedDuringPass (#913-D8 ruling C): an overdue month
// no pass has reported failing is named unreported, but not while a pass is
// sealing it: the serve is sealing, so "check serve is running" would be false.
func TestSealStall_NotUnreportedDuringPass(t *testing.T) {
	h, _, s := newSealedReadHandler(t)
	sealPass(t, s) // May
	june := Period{Kind: periodMonth, Start: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)}
	s.now = func() time.Time { return sealableAt(june, sealGrace).Add(2*sealTick + time.Second) }
	want := [3]string{"2026-06", sealUnreported.code, "2026-07-15T00:00:00Z"}
	if got := stallOf(t, sealedGet(t, h, "/api/v1/report_manifest")); got != want {
		t.Fatalf("control: before the pass: %v, want %v", got, want)
	}
	var during [][3]string
	s.beforeSeal = func() { during = append(during, stallOf(t, sealedGet(t, h, "/api/v1/report_manifest"))) }
	sealPass(t, s)
	if len(during) != 1 || during[0] != [3]string{} {
		t.Errorf("during June's seal: %v; want one read naming no stall", during)
	}
}

// gapFailingStore fails LatestSealedGap on its failAt'th call and records
// whether it did.
type gapFailingStore struct {
	*store.DB
	calls, failAt int
	failed        bool
}

func (g *gapFailingStore) LatestSealedGap(ctx context.Context, size string) (time.Time, bool, error) {
	if g.calls++; g.calls == g.failAt {
		g.failed = true
		return time.Time{}, false, errors.New("injected gap lookup failure")
	}
	return g.DB.LatestSealedGap(ctx, size)
}

// TestSealStall_LookupFailureIs500 (#913-D8 ruling C): with June stalled, a
// failure of any of a read's stall lookups, including one after earlier reads
// succeeded, is a 500, never a 404 or 200 that hides the stall or promises a
// next seal pass.
func TestSealStall_LookupFailureIs500(t *testing.T) {
	h, db, s, _ := juneStuck(t, sealAug1)
	if err := s.sealDue(context.Background()); err == nil {
		t.Fatal("control: June's pass sealed")
	}
	for path, okCode := range map[string]int{
		"/api/v1/scores?period=2026-06": http.StatusNotFound,
		"/api/v1/report_manifest":       http.StatusOK,
		"/api/v1/scores":                http.StatusOK,
	} {
		injected := 0
		for n := 1; ; n++ {
			fs := &gapFailingStore{DB: db, failAt: n}
			h.store = fs
			rec := sealedGet(t, h, path)
			if !fs.failed {
				if rec.Code != okCode {
					t.Errorf("control: %s with no failure: %d %s, want %d", path, rec.Code, rec.Body.String(), okCode)
				}
				break
			}
			injected++
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("%s, lookup %d of %d failing: %d %s; want 500", path, n, fs.calls, rec.Code, rec.Body.String())
			}
		}
		if injected < 2 {
			t.Errorf("%s: %d lookups failed; want the stall's lookups after the first read's", path, injected)
		}
	}
}
