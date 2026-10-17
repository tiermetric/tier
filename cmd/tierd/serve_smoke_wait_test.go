//go:build integration

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSmokeHTTPClientTimeout(t *testing.T) {
	if smokeClient.Timeout <= 0 || smokeClient.Timeout > 2*time.Minute {
		t.Fatalf("smokeClient.Timeout = %s, want > 0 and <= 2m", smokeClient.Timeout)
	}
	for _, body := range []bool{false, true} {
		name := "response headers"
		if body {
			name = "response body"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if body {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-release
			}))
			defer server.Close()
			defer close(release) // Also release the handler if the watchdog fires.
			client := *smokeClient
			client.Timeout = 200 * time.Millisecond
			defer client.CloseIdleConnections()
			start := time.Now()
			result := make(chan error, 1)
			go func() {
				_, _, err := httpGetWith(&client, server.URL)
				result <- err
			}()
			var err error
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Errorf("watchdog: GET stalled in %s did not return within 5s", name)
				return
			}
			elapsed := time.Since(start)
			var timeoutErr net.Error
			if !errors.As(err, &timeoutErr) || !timeoutErr.Timeout() {
				t.Fatalf("GET stalled in %s: %v, want client timeout", name, err)
			}
			// Allow scheduling headroom on a loaded gate box.
			if elapsed > 5*time.Second {
				t.Fatalf("GET stalled in %s took %s with client timeout %s", name, elapsed, client.Timeout)
			}
		})
	}
}

func TestSmokeWaitReady(t *testing.T) {
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probes.Add(1) == 1 {
			// A stalled probe must time out so a later probe can observe readiness.
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := waitForSmokeLive(server.URL, &lockedBuffer{}, make(chan error), 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestSmokeWaitOverallDeadline(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release) // Unblock the server even if the watchdog fires.
	timeout := 300 * time.Millisecond
	result := make(chan error, 1)
	go func() { result <- waitForSmokeLive(server.URL, &lockedBuffer{}, make(chan error), timeout) }()
	want := "server not live within " + timeout.String()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("wait = %v; want %q", err, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("watchdog: wait did not report %q while HTTP server was stalled", want)
	}
}

func TestSmokeWaitFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		exit bool
		body bool
	}{
		{name: "child exits early", exit: true},
		{name: "child stalls past deadline"},
		{name: "child stalls in response body", body: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var arrival sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.body {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				arrival.Do(func() { close(started) })
				<-release
			}))
			defer server.Close()
			defer close(release) // Also unblocks the old, unbounded probe on test failure.
			stderr := &lockedBuffer{}
			_, _ = stderr.Write([]byte("fake child output"))
			exited := make(chan error, 1) // Models the child Wait result; no tierd needed.
			timeout := 80 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- waitForSmokeLiveContext(ctx, server.URL, stderr, exited, timeout) }()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("probe never reached fake server")
			}
			want := "server not live within " + timeout.String()
			if tc.exit {
				exited <- errors.New("fake exit")
				want = "child exited before becoming live (fake exit)"
			} else {
				// Arm the deadline only once the probe has reached the handler.
				timer := time.AfterFunc(timeout, cancel)
				defer timer.Stop()
			}
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "fake child output") {
					t.Fatalf("wait = %v; want %q with child output", err, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("wait did not report %q while HTTP probe was stalled", want)
			}
		})
	}
}
