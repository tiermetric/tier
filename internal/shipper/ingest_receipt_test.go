package shipper

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShipper_WarnsRejectedCrossDeveloper(t *testing.T) {
	for _, accepted := range []int{0, 1} {
		t.Run(fmt.Sprintf("accepted=%d", accepted), func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(old)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/api/v1/events" || r.Header.Get("Authorization") != "Bearer ingest-1144" {
					t.Errorf("unexpected wire contract: %s %s %s", r.Method, r.URL.Path, r.Header.Get("Authorization"))
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{"accepted":%d,"rejected_cross_developer":%d}`, accepted, 2-accepted)
			}))
			defer server.Close()
			client, err := New(Config{ServerURL: server.URL, APIToken: "ingest-1144"})
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"one", "two"} {
				if err := client.Ingest(context.Background(), testEvent(key)); err != nil {
					t.Fatal(err)
				}
			}
			if err := client.Flush(context.Background()); err != nil {
				t.Fatalf("rejection must not fail ship: %v", err)
			}
			if err := client.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Errorf("POST calls=%d, want 1 (no retries)", calls)
			}
			if client.Shipped() != accepted || client.RejectedCrossDeveloper() != 2-accepted {
				t.Errorf("shipped=%d rejected=%d, want %d/%d", client.Shipped(), client.RejectedCrossDeveloper(), accepted, 2-accepted)
			}
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), fmt.Sprintf("rejected_cross_developer=%d", 2-accepted)) {
				t.Errorf("missing rejection-count warning: %s", logs.String())
			}
		})
	}
}
