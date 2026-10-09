package openaiusage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRunPass_SettledOnlyOnSuccess pins the seal gate's success signal
// (#913-D9): a successful pass reports its window, from the first day of the
// previous month through its start less the settlement lag; a pass the
// provider refuses reports nothing, and so does one that dropped an unpriced
// model's remainder (#913-D9 ruling R-8), which the pass after prices.yaml is
// updated re-reads.
func TestRunPass_SettledOnlyOnSuccess(t *testing.T) {
	unpriced := usageReport{Data: []usageBucket{dayBucket("2026-06-15", modelResult("gpt-6-not-in-table", 1000, 0, 500))}}
	for _, c := range []struct {
		name     string
		ok, want bool
		report   usageReport
	}{{"success", true, true, usageReport{}}, {"refused key", false, false, usageReport{}}, {"unpriced model", true, false, unpriced}} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case !c.ok:
					w.WriteHeader(http.StatusUnauthorized)
				case strings.HasPrefix(r.URL.Path, usagePath):
					_, _ = w.Write(mustJSON(t, c.report))
				default:
					_, _ = w.Write(mustJSON(t, costReport{}))
				}
			}))
			defer srv.Close()
			p := newTestPoller(newTestClient(t, srv, nil), newFakeStore(), nil)
			var got [][2]time.Time
			p.settled = func(_ context.Context, from, through time.Time) { got = append(got, [2]time.Time{from, through}) }
			p.runPass(context.Background(), &recordingIngester{})
			y, m, _ := fixedNow.UTC().Date()
			want := [2]time.Time{time.Date(y, m-1, 1, 0, 0, 0, 0, time.UTC), fixedNow.UTC().Add(-24 * time.Hour)}
			switch {
			case c.want && (len(got) != 1 || !got[0][0].Equal(want[0]) || !got[0][1].Equal(want[1])):
				t.Errorf("settled %v, want once with %v", got, want)
			case !c.want && len(got) != 0:
				t.Errorf("settled %v, want nothing", got)
			}
		})
	}
}
