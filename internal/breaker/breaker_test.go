package breaker

import (
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
