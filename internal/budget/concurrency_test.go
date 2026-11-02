package budget

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestEvictCallbackRunsWithoutTheLock guards the documented contract of
// EvictFunc: "It is called with the manager lock released, so it may block on
// a process shutdown."
//
// Eviction unloads a model by talking to a backend, which can take seconds.
// Holding the manager lock across that would stall every other request in the
// gateway, and calling back into the manager from the hook would deadlock.
func TestEvictCallbackRunsWithoutTheLock(t *testing.T) {
	var m *Manager
	called := make(chan struct{}, 1)

	m = New(100, func(_ context.Context, _ Key) error {
		// Any lock-taking method here deadlocks if the lock is still held.
		_ = m.UsedMB()
		_ = m.Snapshot()
		called <- struct{}{}
		return nil
	}, quietLog())

	m.Acquire(context.Background(), Key{Backend: "b", Model: "first"}, 80)

	done := make(chan struct{})
	go func() {
		m.Acquire(context.Background(), Key{Backend: "b", Model: "second"}, 80)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Acquire blocked: the eviction hook ran while the manager lock was held")
	}

	select {
	case <-called:
	default:
		t.Fatal("eviction hook was never called")
	}
}

// TestConcurrentPinUnpinBalances checks the pin counter under contention. A
// leak here is not a crash, it is worse: the model becomes permanently
// unevictable and the budget silently stops working.
func TestConcurrentPinUnpinBalances(t *testing.T) {
	m := New(1000, nil, quietLog())
	k := Key{Backend: "b", Model: "m"}
	m.Acquire(context.Background(), k, 100)

	const goroutines, rounds = 32, 50
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				m.Pin(k)
				m.Touch(k)
				m.Unpin(k)
			}
		}()
	}
	wg.Wait()

	for _, r := range m.Snapshot() {
		if r.Model == k.Model && r.Pins != 0 {
			t.Fatalf("pins=%d after every Pin had a matching Unpin", r.Pins)
		}
	}
}

// TestConcurrentAcquireNeverEvictsPinned is the invariant behind the pin
// feature: while one request is mid-generation on a model, another request
// asking for a different model must not unload it.
func TestConcurrentAcquireNeverEvictsPinned(t *testing.T) {
	m := New(200, nil, quietLog())
	pinned := Key{Backend: "b", Model: "generating"}
	m.Acquire(context.Background(), pinned, 120)
	m.Pin(pinned)

	var lost atomic.Bool
	var wg sync.WaitGroup

	// Churn: keep asking for models that only fit if `pinned` were evicted.
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				k := Key{Backend: "b", Model: "filler-" + string(rune('a'+i))}
				m.Acquire(context.Background(), k, 120)
				m.Touch(k)
				m.Release(k)
			}
		}(i)
	}

	// Watch: the pinned model must be resident for the whole run.
	stop := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			found := false
			for _, r := range m.Snapshot() {
				if r.Model == pinned.Model {
					found = true
					break
				}
			}
			if !found {
				lost.Store(true)
				return
			}
		}
	}()

	wg.Wait()
	close(stop)
	watcher.Wait()

	if lost.Load() {
		t.Fatal("a pinned model was evicted while another request was generating on it")
	}
	m.Unpin(pinned)
}

// TestConcurrentMixedWorkload is a race-detector target for the whole surface:
// acquire, touch, pin, unpin, release, backend release and snapshot all at
// once on overlapping keys.
func TestConcurrentMixedWorkload(t *testing.T) {
	m := New(300, func(_ context.Context, _ Key) error { return nil }, quietLog())
	backends := []string{"a", "b", "c"}

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			backend := backends[i%len(backends)]
			model := string(rune('x' + i%4))
			k := Key{Backend: backend, Model: model}

			for j := 0; j < 30; j++ {
				switch j % 7 {
				case 0:
					m.Acquire(context.Background(), k, 40)
				case 1:
					m.Touch(k)
				case 2:
					m.Pin(k)
				case 3:
					m.Unpin(k)
				case 4:
					_ = m.UsedMB()
					_ = m.Snapshot()
				case 5:
					m.Release(Key{Backend: backend, Model: "absent"})
				case 6:
					if j%14 == 6 {
						m.ReleaseBackend(backend)
					}
				}
			}
		}(i)
	}
	wg.Wait()

	// Accounting must still be consistent: the reported total equals the sum
	// of what Snapshot lists.
	total := 0
	for _, r := range m.Snapshot() {
		total += r.SizeMB
	}
	if got := m.UsedMB(); got != total {
		t.Fatalf("UsedMB()=%d but Snapshot sums to %d", got, total)
	}
}
