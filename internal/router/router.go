package router

import (
	"fmt"
	"sort"
	"strings"

	"github.com/linxu/fmux/internal/adapter"
	"github.com/linxu/fmux/internal/config"
)

// Target is one concrete backend/model pair a request can be sent to.
type Target struct {
	Backend string
	Model   string
	Adapter adapter.Adapter
}

// Plan is the ordered list of targets for a request, plus the profile to apply.
type Plan struct {
	Alias   string
	Targets []Target
	Profile *config.Profile
}

// Router resolves client-facing model names into concrete targets.
type Router struct {
	cfg *config.Config
	reg *adapter.Registry
}

// New builds a router over the given config and registry.
func New(cfg *config.Config, reg *adapter.Registry) *Router {
	return &Router{cfg: cfg, reg: reg}
}

// Resolve turns a model name into a plan.
//
// Resolution order:
//  1. exact alias name
//  2. "model@backend" explicit targeting
//  3. the configured default alias, with the requested model name preserved
//  4. a single backend that pins the model in its config
func (r *Router) Resolve(model string) (*Plan, error) {
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	if a, ok := r.cfg.Aliases[model]; ok {
		return r.planFromAlias(model, a, 0)
	}

	if name, backend, ok := strings.Cut(model, "@"); ok {
		ad, found := r.reg.Get(backend)
		if !found {
			return nil, fmt.Errorf("unknown backend %q (known: %s)", backend, strings.Join(r.reg.Names(), ", "))
		}
		return &Plan{
			Alias:   model,
			Targets: []Target{{Backend: backend, Model: name, Adapter: ad}},
		}, nil
	}

	if r.cfg.Default != "" {
		a, ok := r.cfg.Aliases[r.cfg.Default]
		if ok {
			p, err := r.planFromAlias(r.cfg.Default, a, 0)
			if err != nil {
				return nil, err
			}
			// Preserve the client's requested model on the primary target.
			if len(p.Targets) > 0 {
				p.Targets[0].Model = model
			}
			p.Alias = model
			return p, nil
		}
	}

	var pinned []Target
	for _, b := range r.cfg.Backends {
		for _, m := range b.ModelList {
			if m == model {
				if ad, ok := r.reg.Get(b.Name); ok {
					pinned = append(pinned, Target{Backend: b.Name, Model: model, Adapter: ad})
				}
			}
		}
	}
	if len(pinned) == 1 {
		return &Plan{Alias: model, Targets: pinned}, nil
	}
	if len(pinned) > 1 {
		return nil, fmt.Errorf("model %q exists on multiple backends (%s); use model@backend", model, targetNames(pinned))
	}

	return nil, fmt.Errorf("unknown model %q; known aliases: %s", model, strings.Join(r.aliasNames(), ", "))
}

func (r *Router) planFromAlias(name string, a config.Alias, depth int) (*Plan, error) {
	if depth > 4 {
		return nil, fmt.Errorf("alias %q: fallback chain too deep", name)
	}
	p := &Plan{Alias: name}
	if a.Profile != "" {
		if prof, ok := r.cfg.Profiles[a.Profile]; ok {
			pc := prof
			p.Profile = &pc
		}
	}
	if a.Backend != "" {
		ad, ok := r.reg.Get(a.Backend)
		if !ok {
			return nil, fmt.Errorf("alias %q: backend %q is not registered", name, a.Backend)
		}
		model := a.Model
		if model == "" {
			model = name
		}
		p.Targets = append(p.Targets, Target{Backend: a.Backend, Model: model, Adapter: ad})
	}
	for _, fb := range a.Fallback {
		if strings.Contains(fb, "@") {
			m, b, _ := strings.Cut(fb, "@")
			if ad, ok := r.reg.Get(b); ok {
				p.Targets = append(p.Targets, Target{Backend: b, Model: m, Adapter: ad})
			}
			continue
		}
		sub, ok := r.cfg.Aliases[fb]
		if !ok {
			continue
		}
		subPlan, err := r.planFromAlias(fb, sub, depth+1)
		if err != nil {
			continue
		}
		p.Targets = append(p.Targets, subPlan.Targets...)
	}
	if len(p.Targets) == 0 {
		return nil, fmt.Errorf("alias %q resolves to no target", name)
	}
	return p, nil
}

func (r *Router) aliasNames() []string {
	out := make([]string, 0, len(r.cfg.Aliases))
	for k := range r.cfg.Aliases {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func targetNames(ts []Target) string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Backend)
	}
	return strings.Join(out, ", ")
}
