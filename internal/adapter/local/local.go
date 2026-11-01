// Package local adapts an OpenAI-compatible server that the gateway itself may
// have to start: llama.cpp, MLX-LM, LM Studio.
//
// The wire protocol is identical to any other OpenAI server, so this package
// embeds openai.Adapter and adds only the macOS-specific part: make sure the
// process is up and the model fits in memory before the request goes out.
package local

import (
	"context"

	"github.com/linxu/fmux/internal/adapter"
	"github.com/linxu/fmux/internal/adapter/openai"
	"github.com/linxu/fmux/internal/budget"
	"github.com/linxu/fmux/internal/supervisor"
)

// Options configures a supervised local adapter.
type Options struct {
	OpenAI     openai.Options
	Supervisor *supervisor.Supervisor // nil when the server is started by hand
	Budget     *budget.Manager        // nil disables residency accounting
	SizeMB     int                    // estimated resident size of one model
}

// Adapter is an OpenAI-compatible adapter with a process pre-flight.
type Adapter struct {
	*openai.Adapter
	sup    *supervisor.Supervisor
	bud    *budget.Manager
	sizeMB int
}

// New builds a supervised adapter.
func New(o Options) *Adapter {
	return &Adapter{
		Adapter: openai.New(o.OpenAI),
		sup:     o.Supervisor,
		bud:     o.Budget,
		sizeMB:  o.SizeMB,
	}
}

// Prepare starts the backend if needed and accounts for the model's memory.
//
// The returned release function drops the in-flight claim on the process, so
// the idle reaper leaves it alone until the request is actually over.
func (a *Adapter) Prepare(ctx context.Context, model string) (func(), error) {
	release := func() {}
	if a.sup != nil && a.sup.Has(a.Name()) {
		if err := a.sup.Ensure(ctx, a.Name()); err != nil {
			return release, err
		}
		release = a.sup.Enter(a.Name())
	}
	if a.bud != nil {
		if err := a.bud.Acquire(ctx, budget.Key{Backend: a.Name(), Model: model}, a.sizeMB); err != nil {
			release()
			return func() {}, err
		}
	}
	return release, nil
}

// Release drops the residency record, used when a model is unloaded.
func (a *Adapter) Release(model string) {
	if a.bud != nil {
		a.bud.Release(budget.Key{Backend: a.Name(), Model: model})
	}
}

var _ adapter.Preparer = (*Adapter)(nil)
