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
	"sync"
	"time"
)

// Registry accumulates request statistics per backend and model.
type Registry struct {
	mu    sync.Mutex
	start time.Time
	reqs  map[key]*counter
	evict int
}

type key struct {
	backend string
	model   string
	status  string
}

type counter struct {
	count      int64
	promptTok  int64
	complTok   int64
	durationMS int64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{start: time.Now(), reqs: map[key]*counter{}}
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
	c.durationMS += d.Milliseconds()
	r.mu.Unlock()
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

	fmt.Fprintln(w, "# HELP fmux_request_duration_milliseconds_sum Total wall time spent per backend and model.")
	fmt.Fprintln(w, "# TYPE fmux_request_duration_milliseconds_sum counter")
	for _, s := range snapshot {
		fmt.Fprintf(w, "fmux_request_duration_milliseconds_sum{backend=%q,model=%q} %d\n", s.k.backend, s.k.model, s.c.durationMS)
	}

	fmt.Fprintln(w, "# HELP fmux_evictions_total Models unloaded to stay inside the memory budget.")
	fmt.Fprintln(w, "# TYPE fmux_evictions_total counter")
	fmt.Fprintf(w, "fmux_evictions_total %d\n", evicted)

	fmt.Fprintln(w, "# HELP fmux_uptime_seconds Seconds since the gateway started.")
	fmt.Fprintln(w, "# TYPE fmux_uptime_seconds gauge")
	fmt.Fprintf(w, "fmux_uptime_seconds %.0f\n", uptime.Seconds())
	return nil
}
