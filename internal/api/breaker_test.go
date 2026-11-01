package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linxu/fmux/internal/adapter"
	"github.com/linxu/fmux/internal/adapter/mock"
	"github.com/linxu/fmux/internal/adapter/openai"
	"github.com/linxu/fmux/internal/config"
)

// deadBackend is an HTTP server that always answers 503 and counts how many
// times it was asked. The count is the point: it shows whether the gateway
// stopped trying.
func deadBackend(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"backend is gone"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newFallbackServer(t *testing.T, deadURL string) *Server {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.Add(openai.New(openai.Options{Name: "dead", Type: "vllm", BaseURL: deadURL + "/v1"}))
	reg.Add(mock.New("mock"))

	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		LogLevel: "error",
		Backends: []config.Backend{
			{Name: "dead", Type: "vllm", URL: deadURL + "/v1"},
			{Name: "mock", Type: "mock"},
		},
		Aliases: map[string]config.Alias{
			"auto":     {Backend: "dead", Model: "dead-model", Fallback: []string{"local"}},
			"local":    {Backend: "mock", Model: "mock-model"},
			"lonely":   {Backend: "dead", Model: "dead-model"},
			"mock-any": {Backend: "mock", Model: "mock-model"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	return New(Options{Config: cfg, Registry: reg, Log: testLogger()})
}

func post(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:1976"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestOpenCircuitStopsTryingDeadTarget is the reason the breaker exists. With
// a fallback chain the request always succeeded, but the gateway kept paying
// the dead backend's timeout first, on every single request, forever.
func TestOpenCircuitStopsTryingDeadTarget(t *testing.T) {
	dead, hits := deadBackend(t)
	s := newFallbackServer(t, dead.URL)
	body := `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`

	for i := 0; i < 3; i++ {
		rec := post(t, s, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("dead backend hit %d times during the threshold, want 3", got)
	}

	// The circuit is open now: the fallback must be reached without touching
	// the dead backend at all.
	for i := 0; i < 5; i++ {
		rec := post(t, s, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("after opening: status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("dead backend was retried %d extra times after the circuit opened", got-3)
	}

	// And it is visible in the admin view, otherwise "why is this backend
	// never used" has no answer.
	req := httptest.NewRequest(http.MethodGet, "/admin/backends", nil)
	req.Host = "127.0.0.1:1976"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out struct {
		Backends []struct {
			Backend string `json:"backend"`
			Circuit struct {
				Open  bool `json:"open"`
				Fails int  `json:"consecutive_failures"`
			} `json:"circuit"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, b := range out.Backends {
		if b.Backend == "dead" {
			found = true
			if !b.Circuit.Open {
				t.Errorf("circuit for dead backend is not open: %+v", b.Circuit)
			}
		}
	}
	if !found {
		t.Fatal("dead backend missing from /admin/backends")
	}
}

// TestOpenCircuitStillServesSingleTarget: with nowhere else to go, a request
// must still be attempted. Refusing outright would turn a transient failure
// into a gateway that answers nothing at all.
func TestOpenCircuitStillServesSingleTarget(t *testing.T) {
	dead, hits := deadBackend(t)
	s := newFallbackServer(t, dead.URL)
	body := `{"model":"lonely","messages":[{"role":"user","content":"hi"}]}`

	for i := 0; i < 5; i++ {
		rec := post(t, s, body)
		if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadGateway {
			t.Fatalf("request %d: status = %d, body = %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if got := hits.Load(); got != 5 {
		t.Fatalf("single-target alias was attempted %d times, want 5", got)
	}
}

// TestClientErrorDoesNotTripCircuit: a backend that answers 400 is alive. If
// that opened the circuit, one malformed request would take a healthy backend
// out of service for every client.
func TestClientErrorDoesNotTripCircuit(t *testing.T) {
	var hits atomic.Int64
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer be.Close()

	reg := adapter.NewRegistry()
	reg.Add(openai.New(openai.Options{Name: "pick", Type: "vllm", BaseURL: be.URL + "/v1"}))
	reg.Add(mock.New("mock"))
	cfg := &config.Config{
		Listen: "127.0.0.1:0",
		Backends: []config.Backend{
			{Name: "pick", Type: "vllm", URL: be.URL + "/v1"},
			{Name: "mock", Type: "mock"},
		},
		Aliases: map[string]config.Alias{
			"auto":  {Backend: "pick", Model: "m", Fallback: []string{"local"}},
			"local": {Backend: "mock", Model: "mock-model"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	s := New(Options{Config: cfg, Registry: reg, Log: testLogger()})

	body := `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 6; i++ {
		post(t, s, body)
	}
	if got := hits.Load(); got != 6 {
		t.Fatalf("backend was tried %d times, want 6: a 400 must not open the circuit", got)
	}
}
