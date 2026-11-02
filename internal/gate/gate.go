// Package gate bounds how many requests one backend serves at once.
//
// This exists because local inference is not a web service. A 27B model on a
// Mac answers one request at a time: the second concurrent request does not
// halve the latency, it doubles it, doubles the KV cache, and on a machine
// where GPU and CPU share one memory pool it can push the whole system into
// swap. An HTTP router that forwards everything immediately is therefore not
// neutral — it is an amplifier for exactly the failure mode a local model has.
//
// The gate is a per-backend semaphore with a bounded wait queue. Overflow is a
// 429 with Retry-After, not a 500 and not an unbounded goroutine pile.
package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ErrBusy means the backend stayed saturated for longer than the caller was
// willing to wait. It maps to 429: the request is fine, the timing is not.
var ErrBusy = errors.New("backend stayed busy")

// ErrQueueFull means the wait queue is full, so the request was rejected
// without waiting. Also 429, but the client should back off harder.
var ErrQueueFull = errors.New("backend queue is full")

// Limits describes one backend's admission policy.
type Limits struct {
	// Max is the number of requests that may be in flight at once.
	// 0 means unlimited; negative is treated as unlimited too, so that a
	// config file can say -1 and mean "I know what I am doing".
	Max int
	// Queue is how many requests may wait for a slot. 0 means "do not wait":
	// a saturated backend answers 429 immediately.
	Queue int
	// Timeout bounds how long a waiter may wait.
	Timeout time.Duration
}

// Unlimited reports whether this policy admits everything.
func (l Limits) Unlimited() bool { return l.Max <= 0 }

// String renders the policy for logs and /admin.
func (l Limits) String() string {
	if l.Unlimited() {
		return "unlimited"
	}
	return fmt.Sprintf("%d concurrent, queue %d, wait %s", l.Max, l.Queue, l.Timeout)
}

// Stat is a point-in-time view of one gate.
type Stat struct {
	Backend  string `json:"backend"`
	Max      int    `json:"max"`
	Queue    int    `json:"queue"`
	Timeout  string `json:"queue_timeout"`
	Inflight int    `json:"inflight"`
	Waiting  int    `json:"waiting"`
	Admitted int64  `json:"admitted"`
	Waited   int64  `json:"waited"`
	Rejected int64  `json:"rejected"`
}

// Hooks receives gate events so the caller can feed metrics without this
// package importing a metrics implementation.
type Hooks struct {
	Wait     func(backend string, d time.Duration)
	Rejected func(backend string, reason string)
	Inflight func(backend string, n int)
}

// Gate holds one semaphore per backend.
type Gate struct {
	mu    sync.RWMutex
	gates map[string]*gate
	def   Limits
	log   *slog.Logger
	hooks Hooks
}

type gate struct {
	limits Limits

	sem      chan struct{} // nil when unlimited
	inflight atomic.Int64
	waiting  atomic.Int64

	admitted atomic.Int64
	waited   atomic.Int64
	rejected atomic.Int64
}

// New returns a gate with no backends configured. The default policy applies
// to any backend that is never configured explicitly.
func New(def Limits, hooks Hooks, log *slog.Logger) *Gate {
	if log == nil {
		log = slog.Default()
	}
	return &Gate{gates: map[string]*gate{}, def: def, hooks: hooks, log: log}
}

// Configure sets one backend's policy. It must be called before serving.
func (g *Gate) Configure(backend string, l Limits) {
	if l.Timeout <= 0 {
		l.Timeout = 2 * time.Minute
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gates[backend] = newGate(l)
}

func newGate(l Limits) *gate {
	gt := &gate{limits: l}
	if !l.Unlimited() {
		gt.sem = make(chan struct{}, l.Max)
	}
	return gt
}

func (g *Gate) get(backend string) *gate {
	g.mu.RLock()
	gt, ok := g.gates[backend]
	g.mu.RUnlock()
	if ok {
		return gt
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if gt, ok := g.gates[backend]; ok {
		return gt
	}
	gt = newGate(g.def)
	g.gates[backend] = gt
	return gt
}

// Acquire reserves a slot on the backend. The returned function must be called
// exactly once, and it is safe to call from a defer.
//
// On rejection the error is ErrBusy, ErrQueueFull, or ctx.Err().
func (g *Gate) Acquire(ctx context.Context, backend string) (func(), error) {
	gt := g.get(backend)
	if gt.sem == nil {
		gt.admitted.Add(1)
		g.enter(backend, gt)
		return g.releaseFn(backend, gt), nil
	}

	// Fast path: a free slot, no queueing, no timer.
	select {
	case gt.sem <- struct{}{}:
		gt.admitted.Add(1)
		g.enter(backend, gt)
		return g.releaseFn(backend, gt), nil
	default:
	}

	if gt.limits.Queue <= 0 {
		gt.rejected.Add(1)
		g.notifyRejected(backend, "saturated")
		return nil, fmt.Errorf("%w: %s is serving %d request(s)", ErrQueueFull, backend, len(gt.sem))
	}
	// Claim a waiter slot atomically. Reading the count and then incrementing
	// in two steps lets goroutines that arrive together all observe room and
	// all enter, so queue_depth stops being a hard bound under load -- which
	// is exactly the situation it exists for.
	if gt.waiting.Add(1) > int64(gt.limits.Queue) {
		gt.waiting.Add(-1)
		gt.rejected.Add(1)
		g.notifyRejected(backend, "queue_full")
		return nil, fmt.Errorf("%w: %s already has %d request(s) waiting", ErrQueueFull, backend, gt.limits.Queue)
	}

	start := time.Now()
	timer := time.NewTimer(gt.limits.Timeout)
	defer func() {
		timer.Stop()
		gt.waiting.Add(-1)
	}()

	select {
	case gt.sem <- struct{}{}:
		gt.admitted.Add(1)
		gt.waited.Add(1)
		g.notifyWait(backend, time.Since(start))
		g.enter(backend, gt)
		return g.releaseFn(backend, gt), nil
	case <-ctx.Done():
		gt.rejected.Add(1)
		g.notifyRejected(backend, "cancelled")
		return nil, ctx.Err()
	case <-timer.C:
		gt.rejected.Add(1)
		g.notifyRejected(backend, "timeout")
		return nil, fmt.Errorf("%w: %s did not free a slot within %s", ErrBusy, backend, gt.limits.Timeout)
	}
}

func (g *Gate) enter(backend string, gt *gate) {
	g.notifyInflight(backend, int(gt.inflight.Add(1)))
}

func (g *Gate) releaseFn(backend string, gt *gate) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if gt.sem != nil {
				<-gt.sem
			}
			g.notifyInflight(backend, int(gt.inflight.Add(-1)))
		})
	}
}

// Stats reports every configured gate, sorted by backend name.
func (g *Gate) Stats() []Stat {
	g.mu.RLock()
	out := make([]Stat, 0, len(g.gates))
	for name, gt := range g.gates {
		s := Stat{
			Backend:  name,
			Max:      gt.limits.Max,
			Queue:    gt.limits.Queue,
			Timeout:  gt.limits.Timeout.String(),
			Inflight: int(gt.inflight.Load()),
			Waiting:  int(gt.waiting.Load()),
			Admitted: gt.admitted.Load(),
			Waited:   gt.waited.Load(),
			Rejected: gt.rejected.Load(),
		}
		out = append(out, s)
	}
	g.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Backend < out[j].Backend })
	return out
}

// LimitFor returns the configured policy for a backend.
func (g *Gate) LimitFor(backend string) Limits { return g.get(backend).limits }

func (g *Gate) notifyWait(b string, d time.Duration) {
	if g.hooks.Wait != nil {
		g.hooks.Wait(b, d)
	}
	g.log.Debug("request waited for a slot", "backend", b, "wait_ms", d.Milliseconds())
}

func (g *Gate) notifyRejected(b, reason string) {
	if g.hooks.Rejected != nil {
		g.hooks.Rejected(b, reason)
	}
}

func (g *Gate) notifyInflight(b string, n int) {
	if g.hooks.Inflight != nil {
		g.hooks.Inflight(b, n)
	}
}
