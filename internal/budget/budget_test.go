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
