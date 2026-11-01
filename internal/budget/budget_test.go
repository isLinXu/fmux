package budget

import (
	"context"
	"testing"
)

func TestAcquireEvictsLRU(t *testing.T) {
	var evicted []Key
	m := New(100, func(_ context.Context, k Key) error {
		evicted = append(evicted, k)
		return nil
	}, nil)

	ctx := context.Background()
	if err := m.Acquire(ctx, Key{"a", "m1"}, 60); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(ctx, Key{"a", "m2"}, 30); err != nil {
		t.Fatal(err)
	}
	if m.UsedMB() != 90 {
		t.Fatalf("used = %d, want 90", m.UsedMB())
	}

	// 90 + 40 > 100, so the least recently used entry (m1) must go.
	if err := m.Acquire(ctx, Key{"b", "m3"}, 40); err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0].Model != "m1" {
		t.Fatalf("evicted = %v, want [m1]", evicted)
	}
	if got := m.UsedMB(); got != 70 {
		t.Fatalf("used = %d, want 70", got)
	}
}

func TestAcquireTouchProtectsModel(t *testing.T) {
	var evicted []Key
	m := New(100, func(_ context.Context, k Key) error {
		evicted = append(evicted, k)
		return nil
	}, nil)
	ctx := context.Background()

	_ = m.Acquire(ctx, Key{"a", "old"}, 60)
	_ = m.Acquire(ctx, Key{"a", "new"}, 30)
	m.Touch(Key{"a", "old"}) // now "new" is the LRU victim

	if err := m.Acquire(ctx, Key{"a", "third"}, 40); err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0].Model != "new" {
		t.Fatalf("evicted = %v, want [new]", evicted)
	}
}

func TestAcquireWithoutLimitOnlyRecords(t *testing.T) {
	called := false
	m := New(0, func(context.Context, Key) error { called = true; return nil }, nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = m.Acquire(ctx, Key{"a", string(rune('a' + i))}, 1000)
	}
	if called {
		t.Fatal("eviction ran with no limit")
	}
	if got := m.UsedMB(); got != 5000 {
		t.Fatalf("used = %d, want 5000", got)
	}
}

func TestAcquireAdmitsOversizedModel(t *testing.T) {
	// An oversized model must still be served: a wrong estimate is worse than
	// a brief memory spike.
	var evicted []Key
	m := New(100, func(_ context.Context, k Key) error {
		evicted = append(evicted, k)
		return nil
	}, nil)
	ctx := context.Background()
	_ = m.Acquire(ctx, Key{"a", "small"}, 10)

	if err := m.Acquire(ctx, Key{"a", "huge"}, 500); err != nil {
		t.Fatalf("oversized acquire returned error: %v", err)
	}
	if len(evicted) != 1 || evicted[0].Model != "small" {
		t.Fatalf("evicted = %v, want [small]", evicted)
	}
	if got := m.UsedMB(); got != 500 {
		t.Fatalf("used = %d, want 500", got)
	}
}

// TestPinnedModelIsNotEvicted is the load-bearing one: without it, a request
// for a second model could unload the model another request is generating on.
func TestPinnedModelIsNotEvicted(t *testing.T) {
	var evicted []Key
	m := New(100, func(_ context.Context, k Key) error {
		evicted = append(evicted, k)
		return nil
	}, nil)
	ctx := context.Background()

	busy := Key{"a", "busy"}
	if err := m.Acquire(ctx, busy, 60); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(ctx, Key{"a", "idle"}, 30); err != nil {
		t.Fatal(err)
	}
	m.Pin(busy)

	// 90 + 40 > 100: the pinned model must survive and the idle one must go.
	if err := m.Acquire(ctx, Key{"b", "new"}, 40); err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0].Model != "idle" {
		t.Fatalf("evicted = %v, want [idle]", evicted)
	}
	m.Unpin(busy)

	// Once unpinned it is evictable again.
	if err := m.Acquire(ctx, Key{"b", "newer"}, 40); err != nil {
		t.Fatal(err)
	}
	var sawBusy bool
	for _, k := range evicted {
		if k == busy {
			sawBusy = true
		}
	}
	if !sawBusy {
		t.Fatalf("unpinned model was never evicted: %v", evicted)
	}
}

// TestAllPinnedStillServes: an over-budget load is a warning, never a refusal.
func TestAllPinnedStillServes(t *testing.T) {
	var evicted []Key
	m := New(100, func(_ context.Context, k Key) error {
		evicted = append(evicted, k)
		return nil
	}, nil)
	ctx := context.Background()

	a := Key{"a", "one"}
	_ = m.Acquire(ctx, a, 60)
	m.Pin(a)

	if err := m.Acquire(ctx, Key{"a", "two"}, 60); err != nil {
		t.Fatalf("acquire returned error: %v", err)
	}
	if len(evicted) != 0 {
		t.Fatalf("a pinned model was evicted: %v", evicted)
	}
	if got := m.UsedMB(); got != 120 {
		t.Fatalf("used = %d, want 120 (over budget but served)", got)
	}
}

// TestPinSurvivesReleaseBackend: stopping a process must not leave stale pins
// that would permanently protect a slot that no longer exists.
func TestPinSurvivesReleaseBackend(t *testing.T) {
	m := New(100, nil, nil)
	ctx := context.Background()

	k := Key{"a", "m"}
	_ = m.Acquire(ctx, k, 10)
	m.Pin(k)
	m.ReleaseBackend("a")

	for _, r := range m.Snapshot() {
		t.Fatalf("resident after ReleaseBackend: %+v", r)
	}
	// A fresh acquire on the same key must be evictable again.
	_ = m.Acquire(ctx, k, 10)
	if err := m.Acquire(ctx, Key{"b", "n"}, 95); err != nil {
		t.Fatal(err)
	}
	if got := m.UsedMB(); got != 95 {
		t.Fatalf("used = %d, want 95", got)
	}
}
