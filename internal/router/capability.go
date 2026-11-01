package router

import (
	"fmt"
	"strings"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/compat"
	"github.com/isLinXu/fmux/internal/config"
	"github.com/isLinXu/fmux/internal/ir"
)

// Degradation records a request shape that had to be softened for a backend.
type Degradation struct {
	Backend string
	Reason  string
}

// Prepare adapts a request to a target's capabilities.
//
// The policy is fail-fast for shapes that cannot be faked (vision, tool calls)
// and degrade-with-a-note for shapes that can (native JSON schema). Silent
// degradation is never allowed: callers must be able to see what happened.
func Prepare(req *ir.ChatRequest, t Target, prof *config.Profile) ([]Degradation, error) {
	caps := t.Adapter.Caps()
	var deg []Degradation

	if req.HasImages() && !caps.Vision {
		return nil, fmt.Errorf("backend %q does not support image input", t.Backend)
	}
	if len(req.Tools) > 0 && !caps.Tools {
		return nil, fmt.Errorf("backend %q does not support tool calling", t.Backend)
	}
	if caps.MaxContext > 0 {
		if mt := req.EffectiveMaxTokens(); mt != nil && *mt > caps.MaxContext {
			return nil, fmt.Errorf("max_tokens %d exceeds backend %q context window %d", *mt, t.Backend, caps.MaxContext)
		}
	}

	if req.WantsJSONSchema() && !caps.JSONSchema {
		prompt := compat.JSONSchemaFallbackPrompt(req.ResponseFormat.JSONSchema)
		compat.ApplySystemPrefix(req, prompt)
		req.ResponseFormat = nil
		deg = append(deg, Degradation{
			Backend: t.Backend,
			Reason:  "json_schema degraded to prompt constraint (backend lacks native structured output)",
		})
	}

	if prof != nil {
		applyProfile(req, prof)
	}

	req.ResolvedBackend = t.Backend
	req.ResolvedModel = t.Model
	// The backend must see its own model name, never the client-facing alias.
	req.Model = t.Model
	return deg, nil
}

func applyProfile(req *ir.ChatRequest, p *config.Profile) {
	if p.Temperature != nil && req.Temperature == nil {
		v := *p.Temperature
		req.Temperature = &v
	}
	if p.TopP != nil && req.TopP == nil {
		v := *p.TopP
		req.TopP = &v
	}
	if p.TopK != nil && req.TopK == nil {
		v := *p.TopK
		req.TopK = &v
	}
	if p.MaxTokens != nil && req.EffectiveMaxTokens() == nil {
		v := *p.MaxTokens
		req.MaxTokens = &v
	}
	if len(p.Stop) > 0 && len(req.Stop) == 0 {
		req.Stop = append([]string(nil), p.Stop...)
	}
	if strings.TrimSpace(p.SystemPrefix) != "" {
		compat.ApplySystemPrefix(req, p.SystemPrefix)
	}
}

// CapabilityReport renders the matrix for /admin/backends.
type CapabilityReport struct {
	Backend string               `json:"backend"`
	Type    string               `json:"type"`
	Kind    string               `json:"kind"`
	Caps    adapter.Capabilities `json:"caps"`
	Healthy bool                 `json:"healthy"`
	Error   string               `json:"error,omitempty"`
}
