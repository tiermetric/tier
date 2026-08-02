package health

import (
	"reflect"
	"sync"
	"testing"
)

// stubReporter is a minimal Snapshotter for exercising the Registry without a
// real subsystem. It reports a fixed health and detail.
type stubReporter struct {
	healthy bool
	detail  any
}

func (s stubReporter) HealthSnapshot() SubsystemSnapshot {
	return SubsystemSnapshot{Healthy: s.healthy, Detail: s.detail}
}

// TestRegistry_MultiSubsystemAggregation: several subsystems register and
// Snapshot returns each keyed by name; AllHealthy is true only when they all
// report healthy.
func TestRegistry_MultiSubsystemAggregation(t *testing.T) {
	r := NewRegistry()
	r.Register("watcher", stubReporter{healthy: true, detail: "w"})
	r.Register("anthropic_admin", stubReporter{healthy: true, detail: "a"})
	r.Register("openai_usage", stubReporter{healthy: true, detail: "o"})

	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}
	for _, name := range []string{"watcher", "anthropic_admin", "openai_usage"} {
		if _, ok := snap[name]; !ok {
			t.Errorf("subsystem %q missing from snapshot", name)
		}
	}
	if !AllHealthy(snap) {
		t.Errorf("AllHealthy = false, want true (all subsystems healthy)")
	}
	if got, want := r.Names(), []string{"anthropic_admin", "openai_usage", "watcher"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names = %v, want %v (sorted)", got, want)
	}
}

// TestRegistry_PartialHealth: one unhealthy subsystem makes the aggregate
// unhealthy while the healthy subsystems still render — this is the state that
// drives /healthz from 200 to 503.
func TestRegistry_PartialHealth(t *testing.T) {
	r := NewRegistry()
	r.Register("watcher", stubReporter{healthy: true})
	r.Register("anthropic_admin", stubReporter{healthy: false})

	snap := r.Snapshot()
	if AllHealthy(snap) {
		t.Errorf("AllHealthy = true, want false (one subsystem unhealthy)")
	}
	if !snap["watcher"].Healthy {
		t.Errorf("healthy subsystem should still report healthy in the snapshot")
	}
	if snap["anthropic_admin"].Healthy {
		t.Errorf("unhealthy subsystem should report unhealthy")
	}
}

// TestRegistry_EmptyIsHealthy: a registry with no subsystems is vacuously
// healthy (nothing degraded to report).
func TestRegistry_EmptyIsHealthy(t *testing.T) {
	if !AllHealthy(NewRegistry().Snapshot()) {
		t.Errorf("empty registry: AllHealthy = false, want true")
	}
}

// TestRegistry_RegisterPanics covers the three startup-wiring bugs Register
// fails loud on.
func TestRegistry_RegisterPanics(t *testing.T) {
	t.Run("empty name", func(t *testing.T) {
		defer wantPanic(t, "empty name")
		NewRegistry().Register("", stubReporter{})
	})
	t.Run("nil snapshotter", func(t *testing.T) {
		defer wantPanic(t, "nil snapshotter")
		NewRegistry().Register("x", nil)
	})
	t.Run("duplicate name", func(t *testing.T) {
		defer wantPanic(t, "duplicate name")
		r := NewRegistry()
		r.Register("x", stubReporter{})
		r.Register("x", stubReporter{})
	})
}

func wantPanic(t *testing.T, what string) {
	t.Helper()
	if recover() == nil {
		t.Errorf("Register(%s): expected panic, got none", what)
	}
}

// TestRegistry_Concurrency runs Snapshot concurrently with a late Register
// under the race detector. It asserts no data race and that Snapshot always
// returns a coherent map.
func TestRegistry_Concurrency(t *testing.T) {
	r := NewRegistry()
	r.Register("watcher", stubReporter{healthy: true})

	var wg sync.WaitGroup
	const readers = 8
	stop := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					snap := r.Snapshot()
					if _, ok := snap["watcher"]; !ok {
						// watcher is registered before the readers start and
						// never removed, so it must always be present.
						t.Errorf("watcher missing during concurrent snapshot")
						return
					}
				}
			}
		}()
	}

	// Register additional subsystems while readers are snapshotting.
	for i := 0; i < 50; i++ {
		r.Register("sub"+itoa(i), stubReporter{healthy: true})
	}
	close(stop)
	wg.Wait()

	if got := len(r.Snapshot()); got != 51 {
		t.Errorf("final subsystem count = %d, want 51", got)
	}
}

// itoa is a tiny dependency-free int-to-string for unique subsystem names.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
