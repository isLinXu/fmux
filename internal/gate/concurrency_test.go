package gate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestConcurrentAdmissionNeverExceedsMax is the invariant the whole package
// exists for: a backend that says "one at a time" must never see two.
func TestConcurrentAdmissionNeverExceedsMax(t *testing.T) {
	const (
		maxConcurrent = 2
		callers       = 64
	)

	g := New(Limits{Max: maxConcurrent, Queue: callers, Timeout: 10 * time.Second}, Hooks{}, quiet())
	g.Configure("b", Limits{Max: maxConcurrent, Queue: callers, Timeout: 10 * time.Second})

	var live atomic.Int64
	var peak atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := g.Acquire(context.Background(), "b")
			if err != nil {
				return
			}
			n := live.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			live.Add(-1)
			release()
		}()
	}

	close(start)
	wg.Wait()

	if got := peak.Load(); got > maxConcurrent {
		t.Fatalf("observed %d concurrent admissions, max is %d", got, maxConcurrent)
	}
	if st := g.Stats()[0]; st.Inflight != 0 || st.Waiting != 0 {
		t.Fatalf("after all callers returned: inflight=%d waiting=%d, want 0/0", st.Inflight, st.Waiting)
	}
}

// TestConcurrentQueueDoesNotOverflow asserts that the waiter bound holds under
// contention.
//
// It does not reliably catch a reintroduced check-then-act gap. Acquire used to
// read the waiter count and increment it in two separate steps, so callers
// arriving together could all see room and all enter. Restoring that code and
// running this test five times still passed: the window is a few instructions
// wide and the scheduler serialises it away at this scale.
//
// The atomic claim in Acquire is therefore the guarantee, not this test. What
// this test does catch is a bound that breaks observably -- an off-by-one, a
// missing decrement on one of the exit paths, or a queue that is simply not
// enforced. Keep it as a warning bell, and do not read a pass as proof that the
// admission decision is race-free.
func TestConcurrentQueueDoesNotOverflow(t *testing.T) {
	const (
		queueDepth = 3
		callers    = 48
	)

	g := New(Limits{Max: 1, Queue: queueDepth, Timeout: 10 * time.Second}, Hooks{}, quiet())
	g.Configure("b", Limits{Max: 1, Queue: queueDepth, Timeout: 10 * time.Second})

	// Hold the only slot so every caller below has to queue or be rejected.
	held, err := g.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	var rejected atomic.Int64
	var admitted atomic.Int64

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := g.Acquire(context.Background(), "b")
			if err != nil {
				if errors.Is(err, ErrQueueFull) {
					rejected.Add(1)
				}
				return
			}
			admitted.Add(1)
			release()
		}()
	}

	close(start)

	// Let every caller reach its admission decision before looking.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := g.Stats()[0]
		if st.Waiting+int(rejected.Load()) >= callers {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	st := g.Stats()[0]
	if st.Waiting > queueDepth {
		t.Fatalf("waiting=%d exceeds queue_depth=%d: the admission check is not atomic", st.Waiting, queueDepth)
	}
	if got := int(rejected.Load()); got < callers-queueDepth {
		t.Fatalf("rejected=%d, want at least %d (%d callers, queue depth %d)",
			got, callers-queueDepth, callers, queueDepth)
	}

	held()
	wg.Wait()
}

// TestConcurrentAcquireReleaseHammersTheSemaphore is a race-detector target:
// it exercises the semaphore, the counters and the once-guarded release from
// many goroutines at once.
func TestConcurrentAcquireReleaseHammersTheSemaphore(t *testing.T) {
	g := New(Limits{Max: 4, Queue: 8, Timeout: 5 * time.Second}, Hooks{}, quiet())
	g.Configure("a", Limits{Max: 4, Queue: 8, Timeout: 5 * time.Second})
	g.Configure("b", Limits{Max: 1, Queue: 2, Timeout: 5 * time.Second})

	backends := []string{"a", "b", "unconfigured"}
	var wg sync.WaitGroup

	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			backend := backends[i%len(backends)]
			for j := 0; j < 10; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				release, err := g.Acquire(ctx, backend)
				cancel()
				if err == nil {
					_ = g.Stats()
					_ = g.LimitFor(backend)
					release()
					release() // idempotent by contract
				}
			}
		}(i)
	}

	wg.Wait()

	for _, st := range g.Stats() {
		if st.Inflight != 0 {
			t.Fatalf("%s: inflight=%d after every release, want 0", st.Backend, st.Inflight)
		}
		if st.Waiting != 0 {
			t.Fatalf("%s: waiting=%d after everyone gave up, want 0", st.Backend, st.Waiting)
		}
	}
}

// TestConcurrentConfigureWhileServing documents the supported boundary: the
// map may be read by any number of request goroutines while another one
// configures a different backend. Reconfiguring a backend that is in use is
// explicitly not supported, so it is not exercised here.
func TestConcurrentConfigureWhileServing(t *testing.T) {
	g := New(Limits{Max: 2, Queue: 2, Timeout: 2 * time.Second}, Hooks{}, quiet())
	g.Configure("stable", Limits{Max: 2, Queue: 2, Timeout: 2 * time.Second})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				release, err := g.Acquire(context.Background(), "stable")
				if err == nil {
					release()
				}
			}
		}()
	}

	for i := 0; i < 20; i++ {
		g.Configure("other-"+string(rune('a'+i%26)), Limits{Max: 1, Queue: 1, Timeout: time.Second})
		_ = g.Stats()
	}
	close(stop)
	wg.Wait()
}
