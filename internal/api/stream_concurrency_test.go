package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/openai"
	"github.com/isLinXu/fmux/internal/config"
)

// pacedBackend emits n tokens with a fixed gap between them, then finishes
// cleanly. It is the healthy counterpart to stallingBackend: a stream that is
// slow but alive, which stream_idle must not mistake for a stuck one.
func pacedBackend(t *testing.T, n int, gap time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[{"id":"paced","object":"model"}]}`)
			return
		}
		// Drain the body so the server notices a client hang-up.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)

		for i := 0; i < n; i++ {
			fmt.Fprintf(w, `data: {"id":"c","object":"chat.completion.chunk","model":"paced",`+
				`"choices":[{"index":0,"delta":{"content":"t%d"},"finish_reason":null}]}`+"\n\n", i)
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
		}
		fmt.Fprint(w, `data: {"id":"c","object":"chat.completion.chunk","model":"paced",`+
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
}

func pacedServer(t *testing.T, backendURL string, idle time.Duration) *Server {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.Add(openai.New(openai.Options{
		Name:    "paced",
		Type:    "llamacpp",
		BaseURL: backendURL + "/v1",
		Caps:    adapter.Capabilities{Stream: true, MaxContext: 4096},
	}))

	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		Backends: []config.Backend{{Name: "paced", Type: "llamacpp", URL: backendURL + "/v1"}},
		Aliases:  map[string]config.Alias{"p": {Backend: "paced", Model: "paced"}},
		Timeouts: config.Timeouts{StreamIdle: idle},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	return New(Options{Config: cfg, Registry: reg, Log: testLogger()})
}

// TestStreamIdleTimerResetsOnEveryChunk pins the reset path.
//
// The idle bound is only useful if each arriving chunk buys another full
// window. A timer armed once at the start, or reset on the wrong event, would
// kill a healthy but slow generation halfway through -- the worst possible
// failure, because the client has already been told the request succeeded.
func TestStreamIdleTimerResetsOnEveryChunk(t *testing.T) {
	const (
		tokens = 6
		gap    = 60 * time.Millisecond
	)
	be := pacedBackend(t, tokens, gap)
	defer be.Close()

	// Idle window is longer than one gap but far shorter than the whole
	// stream, so only a per-chunk reset keeps it alive.
	s := pacedServer(t, be.URL, 3*gap)

	rec := doChat(t, s, `{"model":"p","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := rec.Body.String()

	if strings.Contains(body, "stream_idle") {
		t.Fatalf("a stream that kept producing tokens was declared idle: %q", body)
	}
	for i := 0; i < tokens; i++ {
		if !strings.Contains(body, fmt.Sprintf(`t%d`, i)) {
			t.Fatalf("token t%d never reached the client; body=%q", i, body)
		}
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("a completed stream must end with [DONE]; body=%q", body)
	}
}

// TestClientDisconnectDoesNotTripBreaker is the other half of failure
// classification: the caller walking away says nothing about the backend, so
// it must not count toward opening the circuit. A gateway that trips on every
// interrupted `fm chat` would take a healthy backend out of service.
func TestClientDisconnectDoesNotTripBreaker(t *testing.T) {
	// A long, healthy stream: slow enough that the client gets to leave first.
	be := pacedBackend(t, 200, 20*time.Millisecond)
	defer be.Close()

	s := pacedServer(t, be.URL, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"p","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Host = "127.0.0.1:1976"
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rec, req)
	}()

	// Let a few tokens through, then hang up.
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the client disconnected")
	}

	for _, rep := range s.brk.Snapshot() {
		if rep.Open || rep.Fails > 0 {
			t.Fatalf("client disconnect was counted against %q: open=%v failures=%d",
				rep.Backend, rep.Open, rep.Fails)
		}
	}
}

// TestConcurrentStreamsAreIsolated runs several streams at once through one
// gateway. Each has its own idle timer, context and writer, so a stall in one
// must not cancel another; -race checks the shared accounting.
func TestConcurrentStreamsAreIsolated(t *testing.T) {
	be := pacedBackend(t, 4, 10*time.Millisecond)
	defer be.Close()

	s := pacedServer(t, be.URL, 2*time.Second)

	const streams = 12
	var wg sync.WaitGroup
	results := make([]string, streams)

	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"p","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			req.Host = "127.0.0.1:1976"
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			results[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()

	for i, body := range results {
		if !strings.Contains(body, "[DONE]") {
			t.Fatalf("stream %d did not complete; body=%q", i, body)
		}
		if strings.Contains(body, "stream_idle") {
			t.Fatalf("stream %d was cancelled while others were serving; body=%q", i, body)
		}
	}
}
