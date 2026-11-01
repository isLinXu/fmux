// Package conformance verifies the gateway against the real fm binary.
//
// These tests are the reason the project exists: they pin down what fm actually
// puts on the wire and fail loudly when a macOS update changes it.
package conformance

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/isLinXu/fmux/internal/api"
	"github.com/isLinXu/fmux/internal/build"
	"github.com/isLinXu/fmux/internal/config"
	"github.com/isLinXu/fmux/internal/ir"
)

// recorder is a fake OpenAI backend that remembers what it was asked.
type recorder struct {
	mu       sync.Mutex
	requests []*ir.ChatRequest
	raw      []map[string]any
}

func (r *recorder) snapshot() ([]*ir.ChatRequest, []map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*ir.ChatRequest(nil), r.requests...), append([]map[string]any(nil), r.raw...)
}

func (r *recorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "cap-model", "object": "model", "owned_by": "conformance"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var parsed ir.ChatRequest
		_ = json.Unmarshal(body, &parsed)
		var generic map[string]any
		_ = json.Unmarshal(body, &generic)

		r.mu.Lock()
		r.requests = append(r.requests, &parsed)
		r.raw = append(r.raw, generic)
		r.mu.Unlock()

		text := "pong"
		if parsed.WantsJSONSchema() {
			text = `{"name":"","age":0}`
		}

		if parsed.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			chunk := ir.NewChunk("cap-model")
			chunk.Choices = []ir.ChunkChoice{{Index: 0, Delta: ir.Delta{Role: "assistant", Content: text}}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			stop := "stop"
			tail := ir.NewChunk("cap-model")
			tail.Choices = []ir.ChunkChoice{{Index: 0, FinishReason: &stop}}
			b, _ = json.Marshal(tail)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if parsed.WantsUsageInStream() {
				u := ir.NewChunk("cap-model")
				u.Choices = []ir.ChunkChoice{}
				u.Usage = &ir.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4}
				b, _ = json.Marshal(u)
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
			return
		}

		resp := ir.NewChatResponse("cap-model")
		resp.Choices = []ir.ResponseChoice{{
			Index:        0,
			Message:      ir.Message{Role: ir.RoleAssistant, Content: ir.Content{Text: text, Present: true}},
			FinishReason: strptr("stop"),
		}}
		resp.Usage = &ir.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4}
		writeJSON(w, resp)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func strptr(s string) *string { return &s }

// harness spins up the fake backend plus a gateway pointed at it.
type harness struct {
	gw  *httptest.Server
	rec *recorder
	t   *testing.T
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	rec := &recorder{}
	backend := httptest.NewServer(rec.handler())
	t.Cleanup(backend.Close)

	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		LogLevel: "error",
		Backends: []config.Backend{{
			Name: "cap", Type: "openai", URL: backend.URL + "/v1", Kind: "remote",
		}},
		Aliases: map[string]config.Alias{
			"cap-model": {Backend: "cap", Model: "cap-model"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	reg, err := build.Registry(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	gw := httptest.NewServer(api.New(api.Options{Config: cfg, Registry: reg, Log: log}).Handler())
	t.Cleanup(gw.Close)

	return &harness{gw: gw, rec: rec, t: t}
}

func (h *harness) providerURL() string { return h.gw.URL + "/v1" }

func (h *harness) runFm(args ...string) string {
	h.t.Helper()
	cmd := exec.Command("fm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("fm %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func requireFm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("fm"); err != nil {
		t.Skip("fm CLI not installed; skipping wire-conformance test")
	}
	if os.Getenv("FMUX_SKIP_FM_E2E") != "" {
		t.Skip("FMUX_SKIP_FM_E2E set")
	}
}

// TestFmRespondThroughGateway is the headline test: a real fm invocation goes
// through the gateway and comes back with the backend's answer.
func TestFmRespondThroughGateway(t *testing.T) {
	requireFm(t)
	h := newHarness(t)

	out := h.runFm("respond", "--no-stream", "--model-provider", h.providerURL(), "-m", "cap-model", "hi")
	if !strings.Contains(out, "pong") {
		t.Fatalf("unexpected fm output: %q", out)
	}

	reqs, raw := h.rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("backend saw %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if len(got.Messages) != 1 || got.Messages[0].Role != ir.RoleUser {
		t.Errorf("messages = %+v", got.Messages)
	}
	if got.Messages[0].Content.PlainText() != "hi" {
		t.Errorf("prompt = %q", got.Messages[0].Content.PlainText())
	}
	// fm always streams; the gateway must preserve that when forwarding.
	if stream, _ := raw[0]["stream"].(bool); !stream {
		t.Error("gateway should forward stream=true for fm")
	}
	if _, ok := raw[0]["tools"]; ok {
		t.Error("empty tools array leaked to the backend")
	}
}

// TestFmToolQuirkThroughGateway is the regression test for the malformed
// system message fm emits for --tool.
func TestFmToolQuirkThroughGateway(t *testing.T) {
	requireFm(t)
	h := newHarness(t)

	out := h.runFm("respond", "--no-stream", "--model-provider", h.providerURL(), "-m", "cap-model",
		"--tool", "ocr", "hi")
	if !strings.Contains(out, "pong") {
		t.Fatalf("unexpected fm output: %q", out)
	}

	reqs, _ := h.rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("backend saw %d requests", len(reqs))
	}
	for i, m := range reqs[0].Messages {
		if !m.Content.Present {
			t.Errorf("messages[%d] reached the backend without a content field", i)
		}
	}
	if len(reqs[0].Messages) != 1 {
		t.Errorf("expected the empty system message to be dropped, got %d messages", len(reqs[0].Messages))
	}
}

// TestFmSchemaThroughGateway checks structured output survives the hop.
func TestFmSchemaThroughGateway(t *testing.T) {
	requireFm(t)
	h := newHarness(t)

	schemaPath := h.t.TempDir() + "/person.json"
	schema := `{"type":"object","title":"Person","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"],"additionalProperties":false}`
	if err := os.WriteFile(schemaPath, []byte(schema), 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	out := h.runFm("respond", "--no-stream", "--model-provider", h.providerURL(), "-m", "cap-model",
		"--schema", schemaPath, "zhang 30")
	if !strings.Contains(out, "name") {
		t.Fatalf("expected a JSON object back, got %q", out)
	}

	reqs, raw := h.rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("backend saw %d requests", len(reqs))
	}
	if !reqs[0].WantsJSONSchema() {
		t.Error("response_format was lost in transit")
	}
	rf, ok := raw[0]["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing: %v", raw[0])
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v", rf["type"])
	}
}

// TestFmModelsThroughGateway verifies alias discovery.
func TestFmModelsThroughGateway(t *testing.T) {
	requireFm(t)
	h := newHarness(t)

	out := h.runFm("models", "--model-provider", h.providerURL())
	if !strings.Contains(out, "cap-model") {
		t.Fatalf("fm models did not list the alias: %q", out)
	}
}
