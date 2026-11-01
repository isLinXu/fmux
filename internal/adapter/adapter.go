package adapter

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/isLinXu/fmux/internal/ir"
)

// ErrNotSupported is returned when a backend cannot serve a request shape.
var ErrNotSupported = errors.New("not supported by backend")

// Capabilities declares what a backend can do. The gateway uses it to fail
// fast instead of silently degrading.
type Capabilities struct {
	Stream        bool `yaml:"stream" json:"stream"`
	Vision        bool `yaml:"vision" json:"vision"`
	JSONSchema    bool `yaml:"json_schema" json:"json_schema"`
	Tools         bool `yaml:"tools" json:"tools"`
	UsageInStream bool `yaml:"usage_in_stream" json:"usage_in_stream"`
	MaxContext    int  `yaml:"max_context" json:"max_context"`
}

// Stream is an in-flight streaming completion.
type Stream interface {
	// Recv returns the next chunk, or io.EOF when the stream is finished.
	Recv() (*ir.Chunk, error)
	Close() error
}

// Adapter is the single interface every backend implements.
type Adapter interface {
	Name() string
	Type() string
	Caps() Capabilities
	Models(ctx context.Context) ([]ir.ModelInfo, error)
	Chat(ctx context.Context, req *ir.ChatRequest) (*ir.ChatResponse, error)
	ChatStream(ctx context.Context, req *ir.ChatRequest) (Stream, error)
	Health(ctx context.Context) error
}

// Preparer is implemented by adapters that must do work before a request can
// succeed: starting a child process, waiting for a model to load, or evicting
// another model to make room.
//
// Prepare returns a release function that the caller must invoke when the
// request is finished. It is never nil, so callers can defer it
// unconditionally. Its job is to drop the in-flight claim on the backend, which
// is what keeps an idle reaper from stopping a process that is still
// generating: without it a request longer than `idle_timeout` would be killed
// halfway through.
type Preparer interface {
	Prepare(ctx context.Context, model string) (release func(), err error)
}

// Registry holds the live adapters by backend name.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Adapter{}}
}

// Add registers an adapter, replacing any previous one with the same name.
func (r *Registry) Add(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[a.Name()] = a
}

// Get looks an adapter up by backend name.
func (r *Registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byName[name]
	return a, ok
}

// Names lists registered backend names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// All returns every adapter, sorted by name.
func (r *Registry) All() []Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Adapter, 0, len(r.byName))
	for _, a := range r.byName {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
