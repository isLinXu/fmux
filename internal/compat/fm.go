// Package compat isolates client-specific quirks. The fm CLI is the main
// subject: its wire format has several shapes that strict backends reject.
package compat

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/isLinXu/fmux/internal/ir"
)

// Quirk names recorded when a request is adjusted.
const (
	QuirkDroppedEmptySystem = "dropped-empty-system"
	QuirkNormalizedTools    = "normalized-empty-tools"
	QuirkNoContentField     = "missing-content-field"
	QuirkAlwaysStream       = "always-stream"
	QuirkEmptyToolChoice    = "empty-tool-choice"
)

// LooksLikeFm reports whether the request came from the fm CLI.
//
// Detection is deliberately multi-signal: the User-Agent is checked first, then
// the structural fingerprint that fm emits on every call (always stream=true,
// tools present but empty, stream_options.include_usage=true).
func LooksLikeFm(h http.Header, req *ir.ChatRequest) bool {
	ua := strings.ToLower(h.Get("User-Agent"))
	for _, marker := range []string{"fm/", "foundationmodels", "foundation-models", "apple.fm"} {
		if strings.Contains(ua, marker) {
			return true
		}
	}
	if req == nil {
		return false
	}
	return req.Stream &&
		req.Tools != nil && len(req.Tools) == 0 &&
		req.WantsUsageInStream()
}

// Normalize applies fm's quirk fixes in place and returns the quirks applied.
//
// The most important one: `fm --tool ocr` injects a system message with no
// content key at all. Backends with strict validation (vLLM, SGLang) answer
// 400 on that, so it must be dropped before forwarding.
func Normalize(req *ir.ChatRequest) ([]string, error) {
	var quirks []string

	kept := req.Messages[:0]
	for _, m := range req.Messages {
		if !m.Content.Present {
			quirks = append(quirks, QuirkNoContentField)
		}
		isSystemish := m.Role == ir.RoleSystem || m.Role == ir.RoleDeveloper
		if isSystemish && m.Content.IsEmpty() && len(m.ToolCalls) == 0 {
			quirks = append(quirks, QuirkDroppedEmptySystem)
			continue
		}
		kept = append(kept, m)
	}
	req.Messages = kept

	if req.Tools != nil && len(req.Tools) == 0 {
		req.Tools = nil
		quirks = append(quirks, QuirkNormalizedTools)
	}
	if len(req.ToolChoice) > 0 {
		tc := strings.TrimSpace(string(req.ToolChoice))
		if tc == `""` || tc == "null" {
			req.ToolChoice = nil
			quirks = append(quirks, QuirkEmptyToolChoice)
		}
	}

	req.Quirks = append(req.Quirks, quirks...)

	if len(req.Messages) == 0 {
		return quirks, fmt.Errorf("no usable messages after normalization")
	}
	return quirks, nil
}

// ApplySystemPrefix prepends a system message when the profile asks for one.
func ApplySystemPrefix(req *ir.ChatRequest, prefix string) {
	if strings.TrimSpace(prefix) == "" {
		return
	}
	if len(req.Messages) > 0 && req.Messages[0].Role == ir.RoleSystem {
		existing := req.Messages[0].Content.PlainText()
		req.Messages[0].Content = ir.Content{Text: prefix + "\n\n" + existing, Present: true}
		return
	}
	req.Messages = append([]ir.Message{{
		Role:    ir.RoleSystem,
		Content: ir.Content{Text: prefix, Present: true},
	}}, req.Messages...)
}

// JSONSchemaFallbackPrompt renders a schema into an instruction for backends
// without native structured output.
func JSONSchemaFallbackPrompt(spec *ir.JSONSchemaSpec) string {
	var sb strings.Builder
	sb.WriteString("Respond with a single JSON object and nothing else. ")
	if spec.Name != "" {
		sb.WriteString("The object is named " + spec.Name + ". ")
	}
	if len(spec.Schema) > 0 {
		sb.WriteString("It must validate against this JSON Schema:\n")
		sb.Write(spec.Schema)
	}
	return sb.String()
}
