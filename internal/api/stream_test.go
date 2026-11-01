package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linxu/fmux/internal/adapter"
	"github.com/linxu/fmux/internal/adapter/openai"
	"github.com/linxu/fmux/internal/config"
)

// stallingBackend is a backend that opens a stream, sends one token, and then
// says nothing forever. This is the failure mode `stream_idle` exists for: the
// socket is open, the process is alive, and the client would wait for a token
// that is never coming.
func stallingBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[{"id":"stall","object":"model"}]}`)
			return
		}
		// Draining the body lets the server notice the client hanging up;
		// without it the handler would wait forever on a dead connection.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `data: {"id":"c","object":"chat.completion.chunk","model":"stall",`+
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"once"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		// Block until the gateway gives up and cancels the request.
		<-r.Context().Done()
	}))
}

func TestStreamIdleTimeout(t *testing.T) {
	be := stallingBackend(t)
	defer be.Close()

	reg := adapter.NewRegistry()
	reg.Add(openai.New(openai.Options{
		Name:    "stall",
		Type:    "llamacpp",
		BaseURL: be.URL + "/v1",
		Caps:    adapter.Capabilities{Stream: true, MaxContext: 4096},
	}))

	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		Backends: []config.Backend{{Name: "stall", Type: "llamacpp", URL: be.URL + "/v1"}},
		Aliases:  map[string]config.Alias{"s": {Backend: "stall", Model: "stall"}},
		Timeouts: config.Timeouts{StreamIdle: 120 * time.Millisecond},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	s := New(Options{Config: cfg, Registry: reg, Log: testLogger()})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doChat(t, s, `{"model":"s","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	}()

	select {
	case rec := <-done:
		body := rec.Body.String()
		if !strings.Contains(body, "once") {
			t.Errorf("the first token should have reached the client, got %q", body)
		}
		if !strings.Contains(body, "stream_idle") {
			t.Errorf("client should be told the stream stalled, got %q", body)
		}
		if strings.Contains(body, "[DONE]") {
			t.Error("a stalled stream must not be terminated as if it had finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream_idle did not fire; the request hung")
	}
}

// TestStreamIdleBeforeFirstToken checks the other half: if nothing has been
// written yet, the gateway can still answer with a proper 504 instead of a
// half-open 200.
func TestStreamIdleBeforeFirstToken(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[{"id":"stall","object":"model"}]}`)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer be.Close()

	reg := adapter.NewRegistry()
	reg.Add(openai.New(openai.Options{
		Name:    "stall",
		Type:    "llamacpp",
		BaseURL: be.URL + "/v1",
		Caps:    adapter.Capabilities{Stream: true, MaxContext: 4096},
	}))
	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		Backends: []config.Backend{{Name: "stall", Type: "llamacpp", URL: be.URL + "/v1"}},
		Aliases:  map[string]config.Alias{"s": {Backend: "stall", Model: "stall"}},
		Timeouts: config.Timeouts{StreamIdle: 120 * time.Millisecond},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	s := New(Options{Config: cfg, Registry: reg, Log: testLogger()})

	rec := doChat(t, s, `{"model":"s","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body = %s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "stream_idle" {
		t.Errorf("code = %q, want stream_idle", env.Error.Code)
	}
}

// TestRateLimitedIsRateLimitError pins the error type OpenAI clients switch on.
func TestRateLimitedIsRateLimitError(t *testing.T) {
	s, g := gatedServer(t)
	rel, err := g.Acquire(t.Context(), "b1")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	rec := doChat(t, s, `{"model":"only","messages":[{"role":"user","content":"hi"}]}`)
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Type != "rate_limit_error" {
		t.Errorf("type = %q, want rate_limit_error", env.Error.Type)
	}
}
