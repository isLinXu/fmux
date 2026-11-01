package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/mock"
	"github.com/isLinXu/fmux/internal/adapter/openai"
	"github.com/isLinXu/fmux/internal/config"
	"github.com/isLinXu/fmux/internal/ir"
	"github.com/isLinXu/fmux/internal/router"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newServer builds a gateway over the mock backend plus one alias.
func newServer(t *testing.T) (*Server, *mock.Adapter) {
	t.Helper()
	m := mock.New("mock")
	reg := adapter.NewRegistry()
	reg.Add(m)

	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		LogLevel: "error",
		Backends: []config.Backend{{Name: "mock", Type: "mock", Kind: "local"}},
		Aliases: map[string]config.Alias{
			"mock-model": {Backend: "mock", Model: "mock-model"},
			"mock-fast":  {Backend: "mock", Model: "mock-model", Profile: "fast"},
		},
		Profiles: map[string]config.Profile{"fast": {MaxTokens: intp(64)}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	return New(Options{Config: cfg, Registry: reg, Log: testLogger()}), m
}

func intp(v int) *int { return &v }

// doChat sends a completion request the way a local client would: httptest
// defaults Host to example.com, which the guard rejects by design.
func doChat(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:1976"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// doLocal issues any request with a loopback Host.
func doLocal(s *Server, method, path, body string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Host = "127.0.0.1:1976"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestChatNonStream(t *testing.T) {
	s, _ := newServer(t)
	rec := doChat(t, s, `{"model":"mock-model","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp ir.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q", resp.Object)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d", len(resp.Choices))
	}
	if got := resp.Choices[0].Message.Content.PlainText(); got != "mock: hi" {
		t.Errorf("content = %q", got)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens == 0 {
		t.Error("usage should be populated")
	}
}

// TestChatStream verifies the exact SSE tail that OpenAI clients depend on:
// a finish chunk, a usage chunk, then [DONE].
func TestChatStream(t *testing.T) {
	s, _ := newServer(t)
	rec := doChat(t, s, `{"model":"mock-model","stream":true,
		"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"hello there"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) < 3 {
		t.Fatalf("expected several events, got %d: %v", len(events), events)
	}
	if events[len(events)-1] != "[DONE]" {
		t.Errorf("last event = %q, want [DONE]", events[len(events)-1])
	}

	var content strings.Builder
	var sawFinish, sawUsage bool
	for _, e := range events[:len(events)-1] {
		var ch ir.Chunk
		if err := json.Unmarshal([]byte(e), &ch); err != nil {
			t.Fatalf("decode chunk %q: %v", e, err)
		}
		if ch.Object != "chat.completion.chunk" {
			t.Errorf("chunk object = %q", ch.Object)
		}
		for _, c := range ch.Choices {
			content.WriteString(c.Delta.Content)
			if c.FinishReason != nil {
				sawFinish = true
			}
		}
		if ch.Usage != nil && len(ch.Choices) == 0 {
			sawUsage = true
			if ch.Usage.TotalTokens == 0 {
				t.Error("usage chunk has zero tokens")
			}
		}
	}
	if content.String() != "mock: hello there" {
		t.Errorf("streamed content = %q", content.String())
	}
	if !sawFinish {
		t.Error("no finish_reason chunk was emitted")
	}
	if !sawUsage {
		t.Error("include_usage was requested but no usage chunk was emitted")
	}
}

// TestChatFmToolQuirk replays the exact body fm emits for `--tool ocr`, which
// contains {"role":"system"} with no content key.
func TestChatFmToolQuirk(t *testing.T) {
	golden := filepath.Join("..", "..", "test", "conformance", "golden", "tool.json")
	body, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	// The golden names cap-model; point it at the mock alias instead.
	body = bytes.ReplaceAll(body, []byte(`"cap-model"`), []byte(`"mock-model"`))

	s, m := newServer(t)
	rec := doChat(t, s, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	last := m.LastRequest()
	if last == nil {
		t.Fatal("mock saw no request")
	}
	for i, msg := range last.Messages {
		if !msg.Content.Present {
			t.Errorf("backend received messages[%d] with no content", i)
		}
	}
	if len(last.Messages) != 1 || last.Messages[0].Role != ir.RoleUser {
		t.Errorf("system message should have been dropped, got %+v", last.Messages)
	}
	if last.Tools != nil {
		t.Error("empty tools array should not be forwarded")
	}
}

func TestUnknownModel(t *testing.T) {
	s, _ := newServer(t)
	rec := doChat(t, s, `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "model_not_found" {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestMalformedJSON(t *testing.T) {
	s, _ := newServer(t)
	rec := doChat(t, s, `{"model":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestCapabilityReject proves the gateway fails fast instead of forwarding a
// request the backend cannot serve.
func TestCapabilityReject(t *testing.T) {
	blind := openai.New(openai.Options{
		Name: "blind", Type: "mlx", BaseURL: "http://127.0.0.1:1/v1",
		Caps: adapter.Capabilities{Stream: true, Vision: false, MaxContext: 4096},
	})
	reg := adapter.NewRegistry()
	reg.Add(blind)
	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		Backends: []config.Backend{{Name: "blind", Type: "mlx", URL: "http://127.0.0.1:1/v1"}},
		Aliases:  map[string]config.Alias{"v": {Backend: "blind", Model: "m"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	s := New(Options{Config: cfg, Registry: reg, Log: testLogger()})

	rec := doChat(t, s, `{"model":"v","messages":[{"role":"user","content":[
		{"type":"text","text":"what"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "unsupported_capability" {
		t.Errorf("code = %q", env.Error.Code)
	}
}

// TestJSONSchemaDegrade checks that a backend without native structured output
// still receives a schema-shaped instruction, and that the request is rewritten.
func TestJSONSchemaDegrade(t *testing.T) {
	plain := openai.New(openai.Options{
		Name: "plain", Type: "mlx", BaseURL: "http://127.0.0.1:1/v1",
		Caps: adapter.Capabilities{Stream: true, JSONSchema: false, MaxContext: 4096},
	})
	reg := adapter.NewRegistry()
	reg.Add(plain)
	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		Backends: []config.Backend{{Name: "plain", Type: "mlx", URL: "http://127.0.0.1:1/v1"}},
		Aliases:  map[string]config.Alias{"p": {Backend: "plain", Model: "m"}},
	}
	_ = cfg.Validate()

	// Prepare is exercised directly: the backend is unreachable, but the
	// rewrite happens before any HTTP call.
	req := &ir.ChatRequest{
		Model:    "p",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: ir.Content{Text: "zhang 30", Present: true}}},
		ResponseFormat: &ir.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &ir.JSONSchemaSpec{
				Name:   "Person",
				Strict: true,
				Schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
			},
		},
	}
	_ = New(Options{Config: cfg, Registry: reg, Log: testLogger()})
	plan, err := router.New(cfg, reg).Resolve("p")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	deg, err := router.Prepare(req, plan.Targets[0], plan.Profile)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(deg) != 1 {
		t.Fatalf("expected one degradation, got %d", len(deg))
	}
	if req.ResponseFormat != nil {
		t.Error("response_format should be cleared for backends without support")
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != ir.RoleSystem {
		t.Fatal("a schema instruction should be prepended as a system message")
	}
	if !strings.Contains(req.Messages[0].Content.PlainText(), "JSON") {
		t.Errorf("system prompt = %q", req.Messages[0].Content.PlainText())
	}
}

func TestModelsEndpoint(t *testing.T) {
	s, _ := newServer(t)
	rec := doLocal(s, http.MethodGet, "/v1/models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var list ir.ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Object != "list" || len(list.Data) != 2 {
		t.Fatalf("unexpected list: %+v", list)
	}
	names := []string{list.Data[0].ID, list.Data[1].ID}
	if names[0] != "mock-fast" || names[1] != "mock-model" {
		t.Errorf("aliases = %v", names)
	}
}

func TestHealth(t *testing.T) {
	s, _ := newServer(t)
	rec := doLocal(s, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// parseSSE splits a captured SSE body into its data payloads.
func parseSSE(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		out = append(out, strings.TrimPrefix(line, "data: "))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan sse: %v", err)
	}
	return out
}
