package breaker

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock lets a test move time without sleeping.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker(o Options) (*Breaker, *clock) {
	c := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	b := New(o)
	b.now = c.now
	return b, c
}

func TestClosedUntilThreshold(t *testing.T) {
	b, _ := newTestBreaker(Options{Threshold: 3, Cooldown: 30 * time.Second})

	b.Fail("a", "connection refused")
	b.Fail("a", "connection refused")
	if b.Open("a") {
		t.Fatal("circuit opened before the threshold")
	}
	if !b.Allow("a") {
		t.Fatal("closed circuit must allow")
	}

	b.Fail("a", "connection refused")
	if !b.Open("a") {
		t.Fatal("circuit should be open after three failures")
	}
	if b.Allow("a") {
		t.Fatal("open circuit must refuse")
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	b, _ := newTestBreaker(Options{Threshold: 3, Cooldown: 30 * time.Second})
	b.Fail("a", "boom")
	b.Fail("a", "boom")
	b.Succeed("a")
	b.Fail("a", "boom")
	b.Fail("a", "boom")
	if b.Open("a") {
		t.Fatal("a success in between should have reset the counter")
	}
}

// TestOneProbePerCooldown is the property that keeps recovery cheap: the first
// caller after the cooldown gets through, everybody else waits for its verdict
// instead of hammering a backend that is probably still dead.
func TestOneProbePerCooldown(t *testing.T) {
	b, c := newTestBreaker(Options{Threshold: 2, Cooldown: 30 * time.Second})
	b.Fail("a", "down")
	b.Fail("a", "down")
	if b.Allow("a") {
		t.Fatal("must be refused while open")
	}

	c.advance(31 * time.Second)
	if !b.Allow("a") {
		t.Fatal("first call after cooldown must be allowed as a probe")
	}
	if b.Allow("a") {
		t.Fatal("second call must wait for the probe to resolve")
	}

	// The probe succeeded: the circuit is closed and traffic flows again.
	b.Succeed("a")
	if !b.Allow("a") || !b.Allow("a") {
		t.Fatal("closed circuit must allow every request")
	}
}

// TestFailedProbeReopensImmediately: a probe that fails must not cost another
// full threshold of requests before the circuit closes again.
func TestFailedProbeReopensImmediately(t *testing.T) {
	b, c := newTestBreaker(Options{Threshold: 2, Cooldown: 30 * time.Second})
	b.Fail("a", "down")
	b.Fail("a", "down")
	c.advance(31 * time.Second)
	if !b.Allow("a") {
		t.Fatal("probe not allowed")
	}
	b.Fail("a", "still down")
	if b.Allow("a") {
		t.Fatal("a failed probe must reopen the circuit at once")
	}
	if got := b.Snapshot()[0].Trips; got != 2 {
		t.Fatalf("trips = %d, want 2", got)
	}
}

// TestLostProbeSelfHeals covers the probe that never reports back: the caller
// was killed, the process died mid-request, or the response was abandoned. The
// breaker must not stay shut forever.
func TestLostProbeSelfHeals(t *testing.T) {
	b, c := newTestBreaker(Options{Threshold: 2, Cooldown: 30 * time.Second})
	b.Fail("a", "down")
	b.Fail("a", "down")
	c.advance(31 * time.Second)
	if !b.Allow("a") {
		t.Fatal("probe not allowed")
	}
	// No Succeed, no Fail: the probe vanished.
	c.advance(31 * time.Second)
	if !b.Allow("a") {
		t.Fatal("a stale probe must not block the circuit forever")
	}
}

func TestSnapshot(t *testing.T) {
	b, _ := newTestBreaker(Options{Threshold: 1, Cooldown: time.Minute})
	b.Fail("dead", "connection refused")
	b.Succeed("alive")

	reports := map[string]Report{}
	for _, r := range b.Snapshot() {
		reports[r.Backend] = r
	}
	if !reports["dead"].Open || reports["dead"].LastErr != "connection refused" {
		t.Fatalf("dead = %+v", reports["dead"])
	}
	if reports["alive"].Open || reports["alive"].Fails != 0 {
		t.Fatalf("alive = %+v", reports["alive"])
	}
}

func TestNilBreakerAllowsEverything(t *testing.T) {
	var b *Breaker
	if !b.Allow("x") || b.Open("x") {
		t.Fatal("a nil breaker must be transparent")
	}
	b.Succeed("x")
	b.Fail("x", "")
	b.Reset()
	if b.Snapshot() != nil {
		t.Fatal("nil snapshot")
	}
}

func TestAvailableDoesNotReserveProbe(t *testing.T) {
	b, c := newTestBreaker(Options{Threshold: 1, Cooldown: time.Minute})
	if !b.Available("unknown") || len(b.Snapshot()) != 0 {
		t.Fatal("availability lookup created state")
	}
	b.Fail("a", "down")
	if b.Available("a") {
		t.Fatal("open circuit is not available")
	}
	c.advance(time.Minute)
	before := b.Snapshot()
	for i := 0; i < 5; i++ {
		if !b.Available("a") {
			t.Fatal("availability check consumed probe")
		}
	}
	if !reflect.DeepEqual(before, b.Snapshot()) {
		t.Fatal("availability changed circuit state")
	}
	if !b.Allow("a") || b.Available("a") {
		t.Fatal("outstanding probe must block availability")
	}
	c.advance(time.Minute)
	if !b.Available("a") {
		t.Fatal("lost probe did not expire")
	}
	b.Succeed("a")
	if !b.Available("a") {
		t.Fatal("closed circuit must be available")
	}
	var nilBreaker *Breaker
	if !nilBreaker.Available("a") {
		t.Fatal("nil breaker must be available")
	}
}

func TestConcurrentProbeReservation(t *testing.T) {
	b, c := newTestBreaker(Options{Threshold: 1, Cooldown: time.Minute})
	b.Fail("a", "down")
	c.advance(time.Minute)
	var passed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if b.Available("a") && b.Allow("a") {
				passed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := passed.Load(); got != 1 {
		t.Fatalf("reserved probes=%d want=1", got)
	}
}
