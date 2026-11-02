// Package capprobe asks a live backend what it actually supports, instead of
// trusting the capability matrix inferred from its configured `type`.
//
// The matrix in build.CapabilitiesFor encodes what each server family usually
// supports. "Usually" is the problem: an older llama.cpp build may not accept
// a GBNF grammar parameter, and a llama.cpp build from a distribution may not
// have been compiled with vision. The gateway then advertises a capability the
// backend rejects, and the client learns about it as a 400 on a request it had
// every reason to believe would work.
package capprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/openai"
	"github.com/isLinXu/fmux/internal/ir"
)

// Check is one capability tested against a live backend.
type Check struct {
	Capability string `json:"capability"`
	Accepted   bool   `json:"accepted"`
	Detail     string `json:"detail,omitempty"`
}

// Report compares what a backend declares with what it accepted.
type Report struct {
	Backend  string               `json:"backend"`
	Model    string               `json:"model"`
	Declared adapter.Capabilities `json:"declared"`
	Checks   []Check              `json:"checks"`
	// Baseline is false when even a plain request failed, in which case the
	// individual checks say nothing about capabilities.
	Baseline bool `json:"baseline_ok"`
}

// Mismatches lists the capabilities that were rejected despite being declared.
func (r Report) Mismatches() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.Accepted {
			out = append(out, c)
		}
	}
	return out
}

// Agrees reports whether every probed capability matched the declaration.
func (r Report) Agrees() bool {
	return r.Baseline && len(r.Mismatches()) == 0
}

// Run probes one backend.
//
// It sends a baseline request first: if that fails, the backend is unreachable
// or the model is absent, and every later "rejected" verdict would be a lie.
//
// Each capability check costs one inference of at most one token. That is
// nearly free on a warm model and expensive on a cold one -- a load can take
// tens of seconds -- so callers should prefer probing backends that are
// already resident, or say so before starting.
func Run(ctx context.Context, ad adapter.Adapter, model string, perCheck time.Duration) Report {
	if perCheck <= 0 {
		perCheck = 20 * time.Second
	}
	rep := Report{Backend: ad.Name(), Model: model, Declared: ad.Caps()}

	// The baseline is bounded too. A backend that accepts the connection and
	// never answers would otherwise hang the probe before the first check, and
	// a probe that can hang is worse than no probe.
	bctx, bcancel := context.WithTimeout(ctx, perCheck)
	_, err := ad.Chat(bctx, baseRequest(model))
	bcancel()
	if err != nil {
		rep.Checks = []Check{{Capability: "baseline", Accepted: false, Detail: describe(err)}}
		return rep
	}
	rep.Baseline = true

	rep.Checks = []Check{
		run(ctx, ad, model, perCheck, "json_schema", withJSONSchema),
		run(ctx, ad, model, perCheck, "tools", withTools),
		run(ctx, ad, model, perCheck, "vision", withImage),
	}
	return rep
}

func run(ctx context.Context, ad adapter.Adapter, model string, d time.Duration,
	name string, mutate func(*ir.ChatRequest)) Check {

	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	req := baseRequest(model)
	mutate(req)
	_, err := ad.Chat(cctx, req)
	return classify(name, err)
}

func baseRequest(model string) *ir.ChatRequest {
	one := 1
	temp := 0.0
	return &ir.ChatRequest{
		Model: model,
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: ir.Content{Text: "ping", Present: true},
		}},
		MaxTokens:   &one,
		Temperature: &temp,
	}
}

func withJSONSchema(r *ir.ChatRequest) {
	r.Messages = []ir.Message{{
		Role:    ir.RoleUser,
		Content: ir.Content{Text: `Reply with {"ok":true}`, Present: true},
	}}
	r.ResponseFormat = &ir.ResponseFormat{
		Type: "json_schema",
		JSONSchema: &ir.JSONSchemaSpec{
			Name:   "probe",
			Schema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		},
	}
}

func withTools(r *ir.ChatRequest) {
	r.Tools = []ir.Tool{{
		Type: "function",
		Function: ir.ToolFuncDef{
			Name:        "probe_ok",
			Description: "Reports readiness",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
		},
	}}
}

// onePixelPNG is a 1x1 transparent PNG. Vision support is a property of the
// build, not the model, so any valid image answers the question; the point is
// whether the request is accepted, not what it sees.
const onePixelPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func withImage(r *ir.ChatRequest) {
	r.Messages = []ir.Message{{
		Role: ir.RoleUser,
		Content: ir.Content{
			Present: true,
			IsParts: true,
			Parts: []ir.ContentPart{
				{Type: "text", Text: "Describe this image."},
				{Type: "image_url", ImageURL: &ir.ImageURL{URL: onePixelPNG}},
			},
		},
	}}
}

func classify(capability string, err error) Check {
	if err == nil {
		return Check{Capability: capability, Accepted: true, Detail: "accepted"}
	}
	var he *openai.HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case 400, 422:
			// The backend understood the request and refused this shape. That
			// is the answer we were looking for.
			return Check{Capability: capability, Accepted: false,
				Detail: fmt.Sprintf("HTTP %d: %s", he.StatusCode, trim(he.Body, 160))}
		}
		return Check{Capability: capability, Accepted: false,
			Detail: fmt.Sprintf("HTTP %d, inconclusive: %s", he.StatusCode, trim(he.Body, 120))}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Check{Capability: capability, Accepted: false,
			Detail: "timed out; the backend may be loading a model"}
	}
	return Check{Capability: capability, Accepted: false, Detail: describe(err)}
}

func describe(err error) string {
	var he *openai.HTTPError
	if errors.As(err, &he) {
		return fmt.Sprintf("HTTP %d: %s", he.StatusCode, trim(he.Body, 160))
	}
	return trim(err.Error(), 200)
}

func trim(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
