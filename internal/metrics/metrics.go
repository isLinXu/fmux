// Package metrics keeps the counters the gateway exposes on /metrics.
//
// It is intentionally tiny: a map behind a mutex and a Prometheus text renderer.
// Pulling in a client library would buy histograms nobody asked for at the cost
// of a dependency and a global registry.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry accumulates request statistics per backend and model.
type Registry struct {
	mu       sync.Mutex
	start    time.Time
	reqs     map[key]*counter
	evict    int
	inflight map[string]int
	waits    map[string]*waitStat
	rejects  map[string]int64
	duration map[string]*hist
	ttft     map[string]*hist
}

// BucketsMS are the latency buckets, in milliseconds.
//
// The range is chosen for local inference, not for a web service: a first
// token under 100 ms means the model was already warm, a few seconds means it
// had to load, and anything past a minute is a machine that is swapping. The
// upper buckets exist so a slow 27B is visible rather than collapsed into
// +Inf.
var BucketsMS = []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 20000, 30000, 60000, 120000, 300000}

// hist is a fixed-bucket histogram. Counts are stored per bucket and made
// cumulative only when rendered, which is what Prometheus expects to read.
type hist struct {
	counts []int64
	sumMS  int64
	count  int64
}

func newHist() *hist { return &hist{counts: make([]int64, len(BucketsMS))} }

func (h *hist) add(d time.Duration) {
	ms := d.Milliseconds()
	h.count++
	h.sumMS += ms
	for i, b := range BucketsMS {
		if float64(ms) <= b {
			h.counts[i]++
			return
		}
	}
	// Above the last bucket: counted only in +Inf.
}

// waitStat is the admission-control history of one backend.
type waitStat struct {
	count   int64
	totalMS int64
}

type key struct {
	backend string
	model   string
	status  string
}

type counter struct {
	count     int64
	promptTok int64
	complTok  int64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		start:    time.Now(),
		reqs:     map[key]*counter{},
		inflight: map[string]int{},
		waits:    map[string]*waitStat{},
		rejects:  map[string]int64{},
		duration: map[string]*hist{},
		ttft:     map[string]*hist{},
	}
}

// Observe records one completed request.
func (r *Registry) Observe(backend, model string, status int, promptTok, complTok int, d time.Duration) {
	if r == nil {
		return
	}
	k := key{backend: backend, model: model, status: fmt.Sprintf("%d", status)}
	r.mu.Lock()
	c := r.reqs[k]
	if c == nil {
		c = &counter{}
		r.reqs[k] = c
	}
	c.count++
	c.promptTok += int64(promptTok)
	c.complTok += int64(complTok)
	r.histLocked(r.duration, backend).add(d)
	r.mu.Unlock()
}

// ObserveTTFT records the time to the first streamed token.
//
// Total duration says a request was slow; TTFT says why. A 30-second answer
// that produced its first token in 200 ms is a healthy model on a long
// generation, while the same 30 seconds with a 25-second first token is a cold
// load. Without this split the two are indistinguishable.
func (r *Registry) ObserveTTFT(backend string, d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.histLocked(r.ttft, backend).add(d)
	r.mu.Unlock()
}

func (r *Registry) histLocked(m map[string]*hist, backend string) *hist {
	h := m[backend]
	if h == nil {
		h = newHist()
		m[backend] = h
	}
	return h
}

// Evicted increments the eviction counter.
func (r *Registry) Evicted() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.evict++
	r.mu.Unlock()
}

// SetInflight records how many requests a backend is currently serving.
func (r *Registry) SetInflight(backend string, n int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inflight[backend] = n
	r.mu.Unlock()
}

// QueueWait records one request that had to wait for an admission slot.
func (r *Registry) QueueWait(backend string, d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	w := r.waits[backend]
	if w == nil {
		w = &waitStat{}
		r.waits[backend] = w
	}
	w.count++
	w.totalMS += d.Milliseconds()
	r.mu.Unlock()
}

// Rejected records a request the gate refused, with the reason it was refused.
func (r *Registry) Rejected(backend, reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.rejects[backend+"|"+reason]++
	r.mu.Unlock()
}

// Render writes the Prometheus text exposition format.
func (r *Registry) Render(w io.Writer) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	keys := make([]key, 0, len(r.reqs))
	for k := range r.reqs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].backend != keys[j].backend {
			return keys[i].backend < keys[j].backend
		}
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		return keys[i].status < keys[j].status
	})
	snapshot := make([]struct {
		k key
		c counter
	}, 0, len(keys))
	for _, k := range keys {
		snapshot = append(snapshot, struct {
			k key
			c counter
		}{k, *r.reqs[k]})
	}
	evicted := r.evict
	uptime := time.Since(r.start)

	inflight := make(map[string]int, len(r.inflight))
	for b, n := range r.inflight {
		inflight[b] = n
	}
	waits := make(map[string]waitStat, len(r.waits))
	for b, w := range r.waits {
		waits[b] = *w
	}
	rejects := make(map[string]int64, len(r.rejects))
	for k, v := range r.rejects {
		rejects[k] = v
	}
	durations := copyHists(r.duration)
	ttfts := copyHists(r.ttft)
	r.mu.Unlock()

	fmt.Fprintln(w, "# HELP fmux_requests_total Completed chat completion requests.")
	fmt.Fprintln(w, "# TYPE fmux_requests_total counter")
	for _, s := range snapshot {
		fmt.Fprintf(w, "fmux_requests_total{backend=%q,model=%q,status=%q} %d\n",
			s.k.backend, s.k.model, s.k.status, s.c.count)
	}

	fmt.Fprintln(w, "# HELP fmux_prompt_tokens_total Prompt tokens reported by backends.")
	fmt.Fprintln(w, "# TYPE fmux_prompt_tokens_total counter")
	for _, s := range snapshot {
		fmt.Fprintf(w, "fmux_prompt_tokens_total{backend=%q,model=%q} %d\n", s.k.backend, s.k.model, s.c.promptTok)
	}

	fmt.Fprintln(w, "# HELP fmux_completion_tokens_total Completion tokens reported by backends.")
	fmt.Fprintln(w, "# TYPE fmux_completion_tokens_total counter")
	for _, s := range snapshot {
		fmt.Fprintf(w, "fmux_completion_tokens_total{backend=%q,model=%q} %d\n", s.k.backend, s.k.model, s.c.complTok)
	}

	fmt.Fprintln(w, "# HELP fmux_request_duration_milliseconds Wall time per request, bucketed.")
	fmt.Fprintln(w, "# TYPE fmux_request_duration_milliseconds histogram")
	renderHist(w, "fmux_request_duration_milliseconds", durations)

	fmt.Fprintln(w, "# HELP fmux_time_to_first_token_milliseconds Time from request start to the first streamed token.")
	fmt.Fprintln(w, "# TYPE fmux_time_to_first_token_milliseconds histogram")
	renderHist(w, "fmux_time_to_first_token_milliseconds", ttfts)

	fmt.Fprintln(w, "# HELP fmux_evictions_total Models unloaded to stay inside the memory budget.")
	fmt.Fprintln(w, "# TYPE fmux_evictions_total counter")
	fmt.Fprintf(w, "fmux_evictions_total %d\n", evicted)

	fmt.Fprintln(w, "# HELP fmux_inflight_requests Requests currently being served per backend.")
	fmt.Fprintln(w, "# TYPE fmux_inflight_requests gauge")
	for _, b := range sortedKeys(inflight) {
		fmt.Fprintf(w, "fmux_inflight_requests{backend=%q} %d\n", b, inflight[b])
	}

	fmt.Fprintln(w, "# HELP fmux_queue_waits_total Requests that had to wait for an admission slot.")
	fmt.Fprintln(w, "# TYPE fmux_queue_waits_total counter")
	for _, b := range sortedKeys(waits) {
		fmt.Fprintf(w, "fmux_queue_waits_total{backend=%q} %d\n", b, waits[b].count)
	}

	fmt.Fprintln(w, "# HELP fmux_queue_wait_milliseconds_sum Time spent waiting for an admission slot.")
	fmt.Fprintln(w, "# TYPE fmux_queue_wait_milliseconds_sum counter")
	for _, b := range sortedKeys(waits) {
		fmt.Fprintf(w, "fmux_queue_wait_milliseconds_sum{backend=%q} %d\n", b, waits[b].totalMS)
	}

	fmt.Fprintln(w, "# HELP fmux_rejected_total Requests refused by admission control.")
	fmt.Fprintln(w, "# TYPE fmux_rejected_total counter")
	for _, k := range sortedKeys(rejects) {
		backend, reason, _ := strings.Cut(k, "|")
		fmt.Fprintf(w, "fmux_rejected_total{backend=%q,reason=%q} %d\n", backend, reason, rejects[k])
	}

	fmt.Fprintln(w, "# HELP fmux_uptime_seconds Seconds since the gateway started.")
	fmt.Fprintln(w, "# TYPE fmux_uptime_seconds gauge")
	fmt.Fprintf(w, "fmux_uptime_seconds %.0f\n", uptime.Seconds())
	return nil
}

// sortedKeys returns map keys in a stable order, so the exposition text does
// not churn between scrapes.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// copyHists deep-copies the histogram map so rendering never reads a slice the
// request path is still writing to.
func copyHists(m map[string]*hist) map[string]hist {
	out := make(map[string]hist, len(m))
	for k, h := range m {
		c := make([]int64, len(h.counts))
		copy(c, h.counts)
		out[k] = hist{counts: c, sumMS: h.sumMS, count: h.count}
	}
	return out
}

// renderHist writes one histogram family in the Prometheus text format.
// Bucket counts must be cumulative, which is why the running total is
// accumulated here rather than stored.
func renderHist(w io.Writer, name string, hists map[string]hist) {
	for _, backend := range sortedKeys(hists) {
		h := hists[backend]
		var cum int64
		for i, le := range BucketsMS {
			cum += h.counts[i]
			fmt.Fprintf(w, "%s_bucket{backend=%q,le=\"%g\"} %d\n", name, backend, le, cum)
		}
		fmt.Fprintf(w, "%s_bucket{backend=%q,le=\"+Inf\"} %d\n", name, backend, h.count)
		fmt.Fprintf(w, "%s_sum{backend=%q} %d\n", name, backend, h.sumMS)
		fmt.Fprintf(w, "%s_count{backend=%q} %d\n", name, backend, h.count)
	}
}
