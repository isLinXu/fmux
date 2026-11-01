package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/mock"
	"github.com/isLinXu/fmux/internal/config"
	"github.com/isLinXu/fmux/internal/gate"
)

// gatedServer builds a two-backend gateway where b1 admits one request and b2
// is unlimited, plus an alias that falls back from b1 to b2.
func gatedServer(t *testing.T) (*Server, *gate.Gate) {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.Add(mock.New("b1"))
	reg.Add(mock.New("b2"))

	cfg := &config.Config{
		Listen: "127.0.0.1:0",
		Backends: []config.Backend{
			{Name: "b1", Type: "mock", Kind: "local"},
			{Name: "b2", Type: "mock", Kind: "local"},
		},
		Aliases: map[string]config.Alias{
			"only": {Backend: "b1", Model: "m"},
			"fb":   {Backend: "b1", Model: "m", Fallback: []string{"m@b2"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	g := gate.New(gate.Limits{}, gate.Hooks{}, testLogger())
	g.Configure("b1", gate.Limits{Max: 1, Queue: 0, Timeout: time.Second})
	g.Configure("b2", gate.Limits{Max: 0})

	return New(Options{Config: cfg, Registry: reg, Gate: g, Log: testLogger()}), g
}

func TestSaturatedBackendReturns429(t *testing.T) {
	s, g := gatedServer(t)

	rel, err := g.Acquire(context.Background(), "b1")
	if err != nil {
		t.Fatal(err)
	}

	rec := doChat(t, s, `{"model":"only","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("a 429 must tell the client how long to wait")
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "backend_busy" {
		t.Errorf("code = %q, want backend_busy", env.Error.Code)
	}

	// The slot is freed, so the same request must now succeed.
	rel()
	rec = doChat(t, s, `{"model":"only","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("after release: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestFallbackOnSaturation is the point of putting admission control inside the
// target loop rather than in front of it: a busy local model should hand the
// request to the next configured target, not fail it.
func TestFallbackOnSaturation(t *testing.T) {
	s, g := gatedServer(t)

	rel, err := g.Acquire(context.Background(), "b1")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	rec := doChat(t, s, `{"model":"fb","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestGateReleasedAfterResponse(t *testing.T) {
	s, g := gatedServer(t)
	for i := 0; i < 5; i++ {
		rec := doChat(t, s, `{"model":"only","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, rec.Code)
		}
	}
	if st := g.Stats()[0]; st.Inflight != 0 {
		t.Fatalf("inflight after five requests = %d, want 0", st.Inflight)
	}
}

func TestForeignHostRejected(t *testing.T) {
	s, _ := newServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Host = "attacker.example.com"
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "host_not_allowed" {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestCrossOriginRejected(t *testing.T) {
	s, _ := newServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Host = "127.0.0.1:1976"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://attacker.example.com")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "origin_not_allowed" {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestLoopbackOriginAllowed(t *testing.T) {
	s, _ := newServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Host = "127.0.0.1:1976"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:1976")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestRequestIDIsEchoed(t *testing.T) {
	s, _ := newServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "127.0.0.1:1976"
	req.Header.Set("X-Request-Id", "abc-123")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got != "abc-123" {
		t.Errorf("echoed id = %q", got)
	}

	// Without a client-supplied id the gateway must mint one, so every log
	// line can still be correlated.
	if got := doLocal(s, http.MethodGet, "/healthz", "").Header().Get("X-Request-Id"); got == "" {
		t.Error("no request id was generated")
	}
}

func TestHostWithPortIsNormalized(t *testing.T) {
	s, _ := newServer(t)
	for _, host := range []string{"127.0.0.1:1976", "localhost:1976", "[::1]:1976", "127.0.0.1"} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q rejected with %d", host, rec.Code)
		}
	}
}
