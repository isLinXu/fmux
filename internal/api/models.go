package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/linxu/fmux/internal/breaker"
	"github.com/linxu/fmux/internal/gate"
	"github.com/linxu/fmux/internal/ir"
)

// handleModels serves /v1/models.
//
// By default it lists client-facing aliases, because those are the only names
// clients should use. `?all=1` additionally includes every model the backends
// report, which is what `fmux models --all` calls.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	out := make([]ir.ModelInfo, 0, len(s.cfg.Aliases))

	names := make([]string, 0, len(s.cfg.Aliases))
	for n := range s.cfg.Aliases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		a := s.cfg.Aliases[n]
		info := ir.ModelInfo{ID: n, Object: "model", OwnedBy: "fmux", Backend: a.Backend}
		if a.Model != "" && a.Model != n {
			info.Tags = []string{"alias:" + a.Model}
		}
		out = append(out, info)
	}

	if all || len(out) == 0 {
		for _, ad := range s.reg.All() {
			models, err := ad.Models(ctx)
			if err != nil {
				s.log.Warn("models list failed", "backend", ad.Name(), "err", err)
				continue
			}
			out = append(out, models...)
		}
	}

	writeJSON(w, http.StatusOK, ir.ModelList{Object: "list", Data: out})
}

// handleBackends reports the capability matrix, live health and the admission
// policy per backend.
func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	gates := map[string]gate.Stat{}
	if s.gate != nil {
		for _, g := range s.gate.Stats() {
			gates[g.Backend] = g
		}
	}
	circuits := map[string]breaker.Report{}
	for _, c := range s.brk.Snapshot() {
		circuits[c.Backend] = c
	}

	reports := make([]map[string]any, 0, len(s.reg.Names()))
	for _, ad := range s.reg.All() {
		healthy, errMsg := true, ""
		if err := ad.Health(ctx); err != nil {
			healthy = false
			errMsg = err.Error()
		}
		// A backend the breaker has never seen is closed, not missing.
		circuit, ok := circuits[ad.Name()]
		if !ok {
			circuit = breaker.Report{Backend: ad.Name()}
		}
		reports = append(reports, map[string]any{
			"backend": ad.Name(),
			"type":    ad.Type(),
			"caps":    ad.Caps(),
			"healthy": healthy,
			"error":   errMsg,
			"gate":    gates[ad.Name()],
			"circuit": circuit,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": reports})
}

// handleConfig dumps the effective configuration with secrets redacted.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	backends := make([]map[string]any, 0, len(s.cfg.Backends))
	for _, b := range s.cfg.Backends {
		backends = append(backends, map[string]any{
			"name":            b.Name,
			"type":            b.Type,
			"kind":            b.Kind,
			"url":             b.URL,
			"max_concurrency": b.MaxConcurrency,
			"queue_depth":     b.QueueDepth,
			"queue_timeout":   b.QueueTimeout.String(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"listen":    s.cfg.Listen,
		"default":   s.cfg.Default,
		"source":    s.cfg.SourcePath,
		"budget_mb": s.cfg.BudgetMB,
		"backends":  backends,
		"aliases":   s.cfg.Aliases,
		"profiles":  s.cfg.Profiles,
		"timeouts":  s.cfg.Timeouts,
		"guard":     sortedSet(s.allowedHosts()),
	})
}
