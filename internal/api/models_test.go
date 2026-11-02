package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/openai"
	"github.com/isLinXu/fmux/internal/config"
	"github.com/isLinXu/fmux/internal/ir"
)

// TestModelsReportCapabilities covers the gap that made alias choice guesswork:
// the same fm command works against one backend and returns 400 against
// another, and /v1/models used to say nothing about which is which.
func TestModelsReportCapabilities(t *testing.T) {
	reg := adapter.NewRegistry()
	reg.Add(openai.New(openai.Options{
		Name:    "rich",
		Type:    "ollama",
		BaseURL: "http://127.0.0.1:1/v1",
		Caps: adapter.Capabilities{
			Stream: true, Vision: true, JSONSchema: true, Tools: true, MaxContext: 32768,
		},
	}))
	reg.Add(openai.New(openai.Options{
		Name:    "thin",
		Type:    "mlx",
		BaseURL: "http://127.0.0.1:2/v1",
		Caps: adapter.Capabilities{
			Stream: true, Vision: false, JSONSchema: false, Tools: false, MaxContext: 32768,
		},
	}))

	cfg := &config.Config{
		Listen: "127.0.0.1:0",
		Backends: []config.Backend{
			{Name: "rich", Type: "ollama", URL: "http://127.0.0.1:1/v1"},
			{Name: "thin", Type: "mlx", URL: "http://127.0.0.1:2/v1"},
		},
		Aliases: map[string]config.Alias{
			"rich-alias": {Backend: "rich", Model: "rich-model"},
			"thin-alias": {Backend: "thin", Model: "thin-model"},
			// A fallback alias has no single capability set: it depends which
			// target ends up serving, so it must not claim one.
			"either": {Fallback: []string{"rich-alias", "thin-alias"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	s := New(Options{Config: cfg, Registry: reg, Log: testLogger()})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "127.0.0.1:1976"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var list ir.ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}

	byID := make(map[string]ir.ModelInfo, len(list.Data))
	for _, m := range list.Data {
		byID[m.ID] = m
	}

	rich, ok := byID["rich-alias"]
	if !ok {
		t.Fatalf("rich-alias missing from response: %s", rec.Body.String())
	}
	if rich.Capabilities == nil {
		t.Fatal("rich-alias has no capabilities block")
	}
	if !rich.Capabilities.Vision || !rich.Capabilities.JSONSchema || !rich.Capabilities.Tools {
		t.Fatalf("rich-alias should advertise vision/schema/tools, got %+v", *rich.Capabilities)
	}
	if rich.ContextLen != 32768 {
		t.Fatalf("context_length=%d, want 32768", rich.ContextLen)
	}

	thin, ok := byID["thin-alias"]
	if !ok {
		t.Fatalf("thin-alias missing from response")
	}
	if thin.Capabilities == nil {
		t.Fatal("thin-alias has no capabilities block")
	}
	if thin.Capabilities.Vision {
		t.Fatal("thin-alias (mlx) must not advertise vision")
	}
	if thin.Capabilities.JSONSchema {
		t.Fatal("thin-alias (mlx) must not advertise json_schema")
	}
	if !thin.Capabilities.Stream {
		t.Fatal("thin-alias must still advertise streaming")
	}

	if either, ok := byID["either"]; ok && either.Capabilities != nil {
		t.Fatalf("a fallback alias must not claim a capability set, got %+v", *either.Capabilities)
	}
}

// TestModelsCapabilitiesAreOrdinaryJSON keeps the payload parseable by strict
// OpenAI clients: the extra fields must be additive, never a replacement for
// the required ones.
func TestModelsCapabilitiesAreOrdinaryJSON(t *testing.T) {
	reg := adapter.NewRegistry()
	reg.Add(openai.New(openai.Options{
		Name: "b", Type: "ollama", BaseURL: "http://127.0.0.1:1/v1",
		Caps: adapter.Capabilities{Stream: true, MaxContext: 4096},
	}))
	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		Backends: []config.Backend{{Name: "b", Type: "ollama", URL: "http://127.0.0.1:1/v1"}},
		Aliases:  map[string]config.Alias{"a": {Backend: "b", Model: "m"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	s := New(Options{Config: cfg, Registry: reg, Log: testLogger()})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "127.0.0.1:1976"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var raw struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created *int64 `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("strict client decode failed: %v", err)
	}
	if raw.Object != "list" || len(raw.Data) != 1 {
		t.Fatalf("unexpected envelope: %+v", raw)
	}
	if raw.Data[0].ID != "a" || raw.Data[0].Object != "model" {
		t.Fatalf("required fields missing: %+v", raw.Data[0])
	}
}
