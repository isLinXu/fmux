package api

import (
	"net/http"

	"github.com/isLinXu/fmux/internal/budget"
)

// handleProcesses reports supervised child processes and memory residency.
//
// This is the endpoint that tells you why a first request took 90 seconds: the
// process was still loading weights, or another model was being evicted.
func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"processes": []any{},
		"resident":  []any{},
		"budget": map[string]any{
			"limit_mb": 0,
			"used_mb":  0,
		},
	}
	if s.sup != nil {
		resp["processes"] = s.sup.Status()
	}
	if s.bud != nil {
		resident := s.bud.Snapshot()
		if resident == nil {
			resident = []budget.Resident{}
		}
		resp["resident"] = resident
		resp["budget"] = map[string]any{
			"limit_mb": s.bud.LimitMB(),
			"used_mb":  s.bud.UsedMB(),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMetrics renders Prometheus text.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := s.metrics.Render(w); err != nil {
		s.log.Warn("metrics render failed", "err", err)
	}
}
