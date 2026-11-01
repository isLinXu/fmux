// Package breaker keeps one dead backend from costing every request a timeout.
//
// A fallback chain is only worth having if the gateway actually stops trying
// the target that just failed. Without this, a request whose primary backend
// has been killed still pays a full connect timeout — five seconds by default,
// and worse when the host is unreachable and the kernel retries — before it
// falls through to a target that works. Every request pays it again, forever,
// because nothing in the gateway remembers that the backend was already gone.
//
// The state machine is deliberately small: closed, and open with a cooldown.
// There is no separate half-open state to leak. When the cooldown expires the
// next caller is allowed through as a probe; while that probe is outstanding
// everybody else is still refused, so recovery costs one request rather than a
// thundering herd. A probe that never reports back (the caller vanished, the
// process died) expires with the next cooldown, so the breaker cannot get stuck.
package breaker

import (
	"sync"
	"time"
)

// Defaults applied when Options leaves a field at zero.
const (
	DefaultThreshold = 3
	DefaultCooldown  = 30 * time.Second
)

// Options configures a Breaker.
type Options struct {
	// Threshold is how many consecutive backend failures open the circuit.
	Threshold int
	// Cooldown is how long the circuit stays open before one probe is let
	// through.
	Cooldown time.Duration
}

// Report is a point-in-time view of one backend's circuit.
type Report struct {
	Backend  string     `json:"backend"`
	Open     bool       `json:"open"`
	Fails    int        `json:"consecutive_failures"`
	Trips    int64      `json:"trips"`
	OpenedAt *time.Time `json:"opened_at,omitempty"`
	LastErr  string     `json:"last_error,omitempty"`
}

// Breaker tracks consecutive failures per backend.
type Breaker struct {
	mu      sync.Mutex
	o       Options
	entries map[string]*entry
	now     func() time.Time
}

type entry struct {
	fails    int
	trips    int64
	openedAt time.Time
	probeAt  time.Time
	probing  bool
	lastErr  string
}

// New returns a breaker. Zero Options fields fall back to the defaults.
func New(o Options) *Breaker {
	if o.Threshold <= 0 {
		o.Threshold = DefaultThreshold
	}
	if o.Cooldown <= 0 {
		o.Cooldown = DefaultCooldown
	}
	return &Breaker{o: o, entries: map[string]*entry{}, now: time.Now}
}

// Allow reports whether a request may be sent to this backend right now.
//
// Returning true for a probe reserves that probe: concurrent callers get false
// until it resolves or the cooldown expires again.
func (b *Breaker) Allow(backend string) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entryLocked(backend)
	if e.openedAt.IsZero() {
		return true // closed
	}
	if b.now().Sub(e.openedAt) < b.o.Cooldown {
		return false // open
	}
	if e.probing && b.now().Sub(e.probeAt) < b.o.Cooldown {
		return false // a probe is already out
	}
	e.probing = true
	e.probeAt = b.now()
	return true
}

// Succeed clears the failure history for a backend.
func (b *Breaker) Succeed(backend string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entryLocked(backend)
	e.fails = 0
	e.openedAt = time.Time{}
	e.probing = false
}

// Fail records one backend failure and opens the circuit at the threshold.
func (b *Breaker) Fail(backend, msg string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entryLocked(backend)
	e.fails++
	e.probing = false
	e.lastErr = msg
	if e.fails >= b.o.Threshold {
		e.openedAt = b.now()
		e.trips++
	}
}

// Open reports whether the circuit is currently refusing requests.
func (b *Breaker) Open(backend string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entryLocked(backend)
	return !e.openedAt.IsZero() && b.now().Sub(e.openedAt) < b.o.Cooldown
}

// Snapshot lists every backend the breaker has an opinion about.
func (b *Breaker) Snapshot() []Report {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	out := make([]Report, 0, len(b.entries))
	for name, e := range b.entries {
		r := Report{
			Backend: name,
			Open:    !e.openedAt.IsZero() && now.Sub(e.openedAt) < b.o.Cooldown,
			Fails:   e.fails,
			Trips:   e.trips,
			LastErr: e.lastErr,
		}
		if !e.openedAt.IsZero() {
			at := e.openedAt
			r.OpenedAt = &at
		}
		out = append(out, r)
	}
	return out
}

// Reset clears all state, used when a config reload replaces the backends.
func (b *Breaker) Reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.entries = map[string]*entry{}
	b.mu.Unlock()
}

func (b *Breaker) entryLocked(backend string) *entry {
	e := b.entries[backend]
	if e == nil {
		e = &entry{}
		b.entries[backend] = e
	}
	return e
}
