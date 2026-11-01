package gate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestUnlimitedAdmitsEverything(t *testing.T) {
	g := New(Limits{}, Hooks{}, nil)
	g.Configure("b", Limits{Max: 0})

	var rels []func()
	for i := 0; i < 50; i++ {
		rel, err := g.Acquire(context.Background(), "b")
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		rels = append(rels, rel)
	}
	for _, rel := range rels {
		rel()
	}
	if got := g.Stats()[0].Inflight; got != 0 {
		t.Fatalf("inflight after release = %d", got)
	}
}

func TestSaturatedRejectsWhenQueueIsZero(t *testing.T) {
	g := New(Limits{}, Hooks{}, nil)
	g.Configure("b", Limits{Max: 1, Queue: 0})

	rel, err := g.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	if _, err := g.Acquire(context.Background(), "b"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second acquire = %v, want ErrQueueFull", err)
	}
}

func TestWaiterGetsTheFreedSlot(t *testing.T) {
	g := New(Limits{}, Hooks{}, nil)
	g.Configure("b", Limits{Max: 1, Queue: 4, Timeout: 2 * time.Second})

	first, err := g.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan error, 1)
	go func() {
		rel, err := g.Acquire(context.Background(), "b")
		if err == nil {
			rel()
		}
		got <- err
	}()

	// Give the waiter time to park, then hand over the slot.
	time.Sleep(50 * time.Millisecond)
	first()

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never woke up")
	}

	st := g.Stats()[0]
	if st.Waited != 1 {
		t.Fatalf("waited = %d, want 1", st.Waited)
	}
	if st.Inflight != 0 {
		t.Fatalf("inflight = %d, want 0", st.Inflight)
	}
}

func TestQueueTimeout(t *testing.T) {
	g := New(Limits{}, Hooks{}, nil)
	g.Configure("b", Limits{Max: 1, Queue: 2, Timeout: 60 * time.Millisecond})

	rel, err := g.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	start := time.Now()
	if _, err := g.Acquire(context.Background(), "b"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Fatalf("returned after %s, before the timeout", d)
	}
}

func TestQueueFullRejectsImmediately(t *testing.T) {
	g := New(Limits{}, Hooks{}, nil)
	g.Configure("b", Limits{Max: 1, Queue: 1, Timeout: 5 * time.Second})

	rel, err := g.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	// One waiter is allowed to park.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if rel, err := g.Acquire(context.Background(), "b"); err == nil {
			rel()
		}
	}()
	time.Sleep(50 * time.Millisecond)

	// The second waiter has no room.
	if _, err := g.Acquire(context.Background(), "b"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	rel()
	wg.Wait()
}

func TestCancelWhileWaiting(t *testing.T) {
	g := New(Limits{}, Hooks{}, nil)
	g.Configure("b", Limits{Max: 1, Queue: 2, Timeout: 5 * time.Second})

	rel, err := g.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	if _, err := g.Acquire(ctx, "b"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	g := New(Limits{}, Hooks{}, nil)
	g.Configure("b", Limits{Max: 1})

	rel, err := g.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	rel() // a double release must not free a slot that is not held

	if st := g.Stats()[0]; st.Inflight != 0 {
		t.Fatalf("inflight = %d, want 0", st.Inflight)
	}
	// The slot must still be available exactly once.
	rel2, err := g.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatalf("acquire after double release: %v", err)
	}
	rel2()
}

func TestUnconfiguredBackendUsesDefault(t *testing.T) {
	g := New(Limits{Max: 1, Queue: 0}, Hooks{}, nil)
	if _, err := g.Acquire(context.Background(), "never-configured"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Acquire(context.Background(), "never-configured"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull from the default policy", err)
	}
}

func TestHooksFire(t *testing.T) {
	var (
		mu       sync.Mutex
		rejected []string
		waits    int
		last     int
	)
	g := New(Limits{}, Hooks{
		Rejected: func(_ string, reason string) {
			mu.Lock()
			rejected = append(rejected, reason)
			mu.Unlock()
		},
		Wait: func(_ string, _ time.Duration) { mu.Lock(); waits++; mu.Unlock() },
		Inflight: func(_ string, n int) {
			mu.Lock()
			last = n
			mu.Unlock()
		},
	}, nil)
	g.Configure("b", Limits{Max: 1, Queue: 1, Timeout: 2 * time.Second})

	rel, _ := g.Acquire(context.Background(), "b")
	rel() // inflight back to 0
	if _, err := g.Acquire(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if last != 1 {
		t.Fatalf("inflight hook saw %d, want 1", last)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
}
