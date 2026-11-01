// Package budget tracks how much unified memory the resident models occupy and
// evicts the least recently used one when a new model would not fit.
//
// This matters only on Apple Silicon, where CPU and GPU share one pool: loading
// a 27B model at 4-bit costs ~18 GB whether or not it is "on the GPU", and two
// of them plus the OS will start swapping. A Linux box with discrete VRAM can
// simply queue; a Mac has to unload.
package budget

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Key identifies a resident model.
type Key struct {
	Backend string
	Model   string
}

// Resident is one model currently held in memory.
type Resident struct {
	Backend  string    `json:"backend"`
	Model    string    `json:"model"`
	SizeMB   int       `json:"size_mb"`
	LastUsed time.Time `json:"last_used"`
	// Pins counts the requests currently using this model. A pinned model is
	// never evicted: unloading it would kill a generation that is halfway
	// through, which is a much worse outcome than briefly exceeding an
	// estimate.
	Pins int `json:"pins"`
}

// EvictFunc unloads a model. It is called with the manager lock released, so it
// may block on a process shutdown.
type EvictFunc func(ctx context.Context, k Key) error

// Manager is the residency accounting for one gateway process.
type Manager struct {
	mu      sync.Mutex
	limitMB int
	items   map[Key]*item
	pins    map[Key]int
	evict   EvictFunc
	log     *slog.Logger
}

type item struct {
	sizeMB   int
	lastUsed time.Time
}

// New returns a manager with the given limit. A limit of 0 disables eviction
// and records residency for observability only.
func New(limitMB int, evict EvictFunc, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		limitMB: limitMB,
		items:   map[Key]*item{},
		pins:    map[Key]int{},
		evict:   evict,
		log:     log,
	}
}

// Acquire records that a model is about to be used, evicting LRU residents if
// the budget would be exceeded.
//
// It is advisory by design: when even an empty cache cannot fit the model, the
// request still proceeds and a warning is logged. Refusing to serve because an
// estimate was wrong is worse than a brief memory spike.
func (m *Manager) Acquire(ctx context.Context, k Key, sizeMB int) error {
	m.mu.Lock()
	if it, ok := m.items[k]; ok {
		it.lastUsed = time.Now()
		m.mu.Unlock()
		return nil
	}
	if m.limitMB <= 0 || sizeMB <= 0 {
		m.items[k] = &item{sizeMB: sizeMB, lastUsed: time.Now()}
		m.mu.Unlock()
		return nil
	}

	var victims []Key
	blockedByPins := false
	for m.usedLocked()+sizeMB > m.limitMB {
		v := m.lruLocked(k)
		if v == nil {
			blockedByPins = m.anyPinnedLocked(k)
			break
		}
		delete(m.items, *v)
		victims = append(victims, *v)
	}
	m.items[k] = &item{sizeMB: sizeMB, lastUsed: time.Now()}
	used := m.usedLocked()
	m.mu.Unlock()

	for _, v := range victims {
		m.log.Info("evicting model for budget", "backend", v.Backend, "model", v.Model, "limit_mb", m.limitMB)
		if m.evict != nil {
			if err := m.evict(ctx, v); err != nil {
				m.log.Warn("eviction failed", "backend", v.Backend, "model", v.Model, "err", err)
			}
		}
	}
	if used > m.limitMB {
		if blockedByPins {
			m.log.Warn("memory budget exceeded: every evictable resident is in use",
				"used_mb", used, "limit_mb", m.limitMB, "model", k.Model)
		} else {
			m.log.Warn("memory budget exceeded after eviction", "used_mb", used, "limit_mb", m.limitMB, "model", k.Model)
		}
	}
	return nil
}

// Touch marks a resident model as recently used.
func (m *Manager) Touch(k Key) {
	m.mu.Lock()
	if it, ok := m.items[k]; ok {
		it.lastUsed = time.Now()
	}
	m.mu.Unlock()
}

// Pin marks a resident model as in use. Pinned models are skipped by eviction
// until the matching Unpin, so a second model cannot unload a model that is
// mid-generation on another backend.
//
// Pinning a model that was never acquired is harmless and still protects it if
// it becomes resident later.
func (m *Manager) Pin(k Key) {
	m.mu.Lock()
	m.pins[k]++
	m.mu.Unlock()
}

// Unpin releases one Pin.
func (m *Manager) Unpin(k Key) {
	m.mu.Lock()
	if n := m.pins[k]; n > 1 {
		m.pins[k] = n - 1
	} else {
		delete(m.pins, k)
	}
	m.mu.Unlock()
}

// Release forgets a resident model.
func (m *Manager) Release(k Key) {
	m.mu.Lock()
	delete(m.items, k)
	delete(m.pins, k)
	m.mu.Unlock()
}

// ReleaseBackend forgets every model resident on one backend, used when that
// backend's process is stopped.
func (m *Manager) ReleaseBackend(backend string) {
	m.mu.Lock()
	for k := range m.items {
		if k.Backend == backend {
			delete(m.items, k)
		}
	}
	for k := range m.pins {
		if k.Backend == backend {
			delete(m.pins, k)
		}
	}
	m.mu.Unlock()
}

// UsedMB reports current accounted memory.
func (m *Manager) UsedMB() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usedLocked()
}

// LimitMB reports the configured budget.
func (m *Manager) LimitMB() int { return m.limitMB }

// Snapshot lists residents, most recently used first.
func (m *Manager) Snapshot() []Resident {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Resident, 0, len(m.items))
	for k, it := range m.items {
		out = append(out, Resident{
			Backend:  k.Backend,
			Model:    k.Model,
			SizeMB:   it.sizeMB,
			LastUsed: it.lastUsed,
			Pins:     m.pins[k],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.After(out[j].LastUsed) })
	return out
}

func (m *Manager) usedLocked() int {
	total := 0
	for _, it := range m.items {
		total += it.sizeMB
	}
	return total
}

func (m *Manager) lruLocked(except Key) *Key {
	var victim *Key
	var oldest time.Time
	for k, it := range m.items {
		if k == except || m.pins[k] > 0 {
			continue
		}
		if victim == nil || it.lastUsed.Before(oldest) {
			kk := k
			victim = &kk
			oldest = it.lastUsed
		}
	}
	return victim
}

// anyPinnedLocked reports whether any resident other than except is in use.
func (m *Manager) anyPinnedLocked(except Key) bool {
	for k := range m.items {
		if k != except && m.pins[k] > 0 {
			return true
		}
	}
	return false
}
