package muse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// S08-4: refused spend must reach Lost before the scan cursor can advance.
func TestAuditS08MuseRefusalsRecordLost(t *testing.T) {
	for _, tc := range []struct {
		name, record string
		onlyRefused  bool
	}{
		{"missing model", `{"id":"bad","recorded_at":1790000200000000,"payload":{"kind":"run","event":{"kind":"model_completed","usage":{"input_tokens":10,"output_tokens":1}}}}`, false},
		{"missing timestamp", `{"id":"bad","payload":{"kind":"run","event":{"kind":"model_completed","model":"m","usage":{"input_tokens":10,"output_tokens":1}}}}`, true},
		{"future timestamp", `{"id":"bad","recorded_at":9999999999000000,"payload":{"kind":"run","event":{"kind":"model_completed","model":"m","usage":{"input_tokens":10,"output_tokens":1}}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, home := initGitRepo(t), t.TempDir()
			body := readFixture(t)
			if tc.onlyRefused {
				body = fixtureLinesWithout(t, `"kind":"model_completed"`, `"kind":"automated_review_completed"`)
			}
			writeSession(t, home, "s", repo, body+tc.record+"\n")
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			c := newCollector(t, home, []string{repo}, func() time.Time { return now })
			var losses [][2]time.Time
			c.lost = func(_ context.Context, from, through time.Time) error {
				losses = append(losses, [2]time.Time{from, through})
				return errors.New("loss store unavailable")
			}
			settled := 0
			c.settled = func(context.Context, time.Time, time.Time) { settled++ }
			c.runPass(context.Background(), since, &memIngester{})
			if len(losses) != 1 || !losses[0][0].Equal(since) || !losses[0][1].Equal(now) {
				t.Fatalf("lost %v, want the conservative refused-record window %s..%s", losses, since, now)
			}
			if !c.fileFloor.IsZero() || settled != 0 {
				t.Fatalf("failed loss write advanced cursor %s or settled %d times", c.fileFloor, settled)
			}
			c.lost = func(_ context.Context, from, through time.Time) error {
				losses = append(losses, [2]time.Time{from, through})
				return nil
			}
			c.runPass(context.Background(), since, &memIngester{})
			if len(losses) != 2 || !c.fileFloor.Equal(now) || settled != 1 {
				t.Fatalf("retry: losses %v, cursor %s, settled %d", losses, c.fileFloor, settled)
			}
		})
	}
}

func TestAuditS08MuseForeignAndDuplicateRecordsAreNotLost(t *testing.T) {
	repo, foreign, home := initGitRepo(t), initGitRepo(t), t.TempDir()
	body := readFixture(t)
	for _, line := range strings.SplitAfter(body, "\n") {
		if strings.Contains(line, `"id":"rec-fixture-0005"`) {
			body += line
			break
		}
	}
	writeSession(t, home, "ours", repo, body)
	writeSession(t, home, "foreign", foreign, readFixture(t)+`{"id":"bad","payload":{"kind":"run","event":{"kind":"model_completed"}}}`+"\n")
	c := newCollector(t, home, []string{repo}, nil)
	c.lost = func(context.Context, time.Time, time.Time) error {
		t.Error("foreign or duplicate records marked lost")
		return nil
	}
	ing := &memIngester{}
	c.runPass(context.Background(), time.Time{}, ing)
	if len(ing.byKey) != len(fixtureWant) {
		t.Fatalf("captured %d calls, want %d", len(ing.byKey), len(fixtureWant))
	}
}
