package api

import (
	"context"
	"net/http"
	"sort"
	"time"

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

// handleBackends reports the capability matrix and live health.
func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	reports := make([]map[string]any, 0, len(s.reg.Names()))
	for _, ad := range s.reg.All() {
		healthy, errMsg := true, ""
		if err := ad.Health(ctx); err != nil {
			healthy = false
			errMsg = err.Error()
		}
		reports = append(reports, map[string]any{
			"backend": ad.Name(),
			"type":    ad.Type(),
			"caps":    ad.Caps(),
			"healthy": healthy,
			"error":   errMsg,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": reports})
}

// handleConfig dumps the effective configuration with secrets redacted.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	backends := make([]map[string]any, 0, len(s.cfg.Backends))
	for _, b := range s.cfg.Backends {
		backends = append(backends, map[string]any{
			"name": b.Name,
			"type": b.Type,
			"kind": b.Kind,
			"url":  b.URL,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"listen":   s.cfg.Listen,
		"default":  s.cfg.Default,
		"source":   s.cfg.SourcePath,
		"backends": backends,
		"aliases":  s.cfg.Aliases,
		"profiles": s.cfg.Profiles,
	})
}
