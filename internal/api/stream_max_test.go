package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/openai"
	"github.com/isLinXu/fmux/internal/budget"
	"github.com/isLinXu/fmux/internal/config"
	"github.com/isLinXu/fmux/internal/gate"
	"gopkg.in/yaml.v3"
)

const streamMaxBody = `{"model":"p","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func setStreamMax(t *testing.T, s *Server, limit string) {
	t.Helper()
	if err := yaml.Unmarshal([]byte("timeouts:\n  stream_max: "+limit+"\n"), s.cfg); err != nil {
		t.Fatal(err)
	}
}

func assertStreamMax(t *testing.T, s *Server, rec *httptest.ResponseRecorder, started bool, elapsed time.Duration) {
	t.Helper()
	if elapsed > 700*time.Millisecond {
		t.Errorf("total deadline did not bound request: %s", elapsed)
	}
	if !strings.Contains(rec.Body.String(), `"code":"stream_max_duration"`) {
		t.Errorf("missing total-duration error: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("timed-out stream must not emit DONE")
	}
	wantStatus := http.StatusGatewayTimeout
	if started {
		wantStatus = http.StatusOK
	}
	if rec.Code != wantStatus {
		t.Errorf("status=%d want=%d", rec.Code, wantStatus)
	}
	if s.Inflight() != 0 {
		t.Errorf("handler inflight=%d", s.Inflight())
	}
	for _, rep := range s.brk.Snapshot() {
		if rep.Open || rep.Fails != 0 {
			t.Errorf("duration timeout tripped breaker: %+v", rep)
		}
	}
}

func TestStreamMaxBoundsActiveStream(t *testing.T) {
	be := pacedBackend(t, 60, 20*time.Millisecond)
	defer be.Close()
	s := pacedServer(t, be.URL, time.Second)
	setStreamMax(t, s, "100ms")
	g := gate.New(gate.Limits{}, gate.Hooks{}, testLogger())
	g.Configure("paced", gate.Limits{Max: 1})
	s.gate = g
	s.bud = budget.New(0, nil, testLogger())
	_ = s.bud.Acquire(t.Context(), budget.Key{Backend: "paced", Model: "paced"}, 1)
	start := time.Now()
	rec := doChat(t, s, streamMaxBody)
	assertStreamMax(t, s, rec, true, time.Since(start))
	if !strings.Contains(rec.Body.String(), "t0") {
		t.Error("first token did not arrive")
	}
	if got := g.Stats()[0].Inflight; got != 0 {
		t.Errorf("gate slot leaked: %d", got)
	}
	for _, resident := range s.bud.Snapshot() {
		if resident.Pins != 0 {
			t.Errorf("model pin leaked: %+v", resident)
		}
	}
}

func TestStreamMaxBeforeResponseHeaders(t *testing.T) {
	cancelled := make(chan struct{})
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer be.Close()
	s := pacedServer(t, be.URL, time.Second)
	setStreamMax(t, s, "100ms")
	start := time.Now()
	rec := doChat(t, s, streamMaxBody)
	assertStreamMax(t, s, rec, false, time.Since(start))
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Error("upstream request was not cancelled")
	}
}

func TestStreamMaxBeforeFirstChunk(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer be.Close()
	s := pacedServer(t, be.URL, time.Second)
	setStreamMax(t, s, "100ms")
	start := time.Now()
	rec := doChat(t, s, streamMaxBody)
	assertStreamMax(t, s, rec, false, time.Since(start))
}

func TestStreamMaxBoundsQueue(t *testing.T) {
	s, _ := newServer(t)
	setStreamMax(t, s, "100ms")
	g := gate.New(gate.Limits{}, gate.Hooks{}, testLogger())
	g.Configure("mock", gate.Limits{Max: 1, Queue: 1, Timeout: time.Second})
	s.gate = g
	release, err := g.Acquire(t.Context(), "mock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	start := time.Now()
	rec := doChat(t, s, strings.Replace(streamMaxBody, `"p"`, `"mock-model"`, 1))
	assertStreamMax(t, s, rec, false, time.Since(start))
	stat := g.Stats()[0]
	if stat.Waiting != 0 || stat.Inflight != 1 {
		t.Errorf("queue did not release correctly: %+v", stat)
	}
}

type waitingPreparer struct{ adapter.Adapter }

func (a waitingPreparer) Prepare(ctx context.Context, model string) (func(), error) {
	select {
	case <-ctx.Done():
		return func() {}, ctx.Err()
	case <-time.After(time.Second):
		return func() {}, fmt.Errorf("startup did not finish")
	}
}

func TestStreamMaxBoundsPreparation(t *testing.T) {
	s, m := newServer(t)
	s.reg.Add(waitingPreparer{m})
	s.bud = budget.New(0, nil, testLogger())
	setStreamMax(t, s, "100ms")
	start := time.Now()
	rec := doChat(t, s, strings.Replace(streamMaxBody, `"p"`, `"mock-model"`, 1))
	assertStreamMax(t, s, rec, false, time.Since(start))
	key := budget.Key{Backend: "mock", Model: "mock-model"}
	_ = s.bud.Acquire(t.Context(), key, 1)
	if got := s.bud.Snapshot()[0].Pins; got != 0 {
		t.Errorf("preparation leaked pin: %d", got)
	}
}

func TestStreamMaxSharedByFallbacks(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(120 * time.Millisecond):
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer first.Close()
	second := pacedBackend(t, 10, 20*time.Millisecond)
	defer second.Close()
	s := pacedServer(t, second.URL, time.Second)
	s.reg.Add(openai.New(openai.Options{
		Name: "first", BaseURL: first.URL + "/v1", Caps: adapter.Capabilities{Stream: true},
	}))
	s.cfg.Aliases["auto"] = config.Alias{Backend: "first", Model: "m", Fallback: []string{"p"}}
	setStreamMax(t, s, "200ms")
	start := time.Now()
	rec := doChat(t, s, strings.Replace(streamMaxBody, `"p"`, `"auto"`, 1))
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("fallback reset the deadline: %s", elapsed)
	}
	if !strings.Contains(rec.Body.String(), `"code":"stream_max_duration"`) ||
		strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("fallback exceeded shared deadline: %s", rec.Body.String())
	}
}

func TestStreamMaxDoesNotOverrideEarlierIdle(t *testing.T) {
	be := stallingBackend(t)
	defer be.Close()
	s := pacedServer(t, be.URL, 50*time.Millisecond)
	setStreamMax(t, s, "1s")
	rec := doChat(t, s, streamMaxBody)
	if !strings.Contains(rec.Body.String(), `"code":"stream_idle"`) {
		t.Fatalf("earlier idle timeout not preserved: %s", rec.Body.String())
	}
}

func TestStreamMaxDisabledAllowsCompletion(t *testing.T) {
	be := pacedBackend(t, 5, 20*time.Millisecond)
	defer be.Close()
	s := pacedServer(t, be.URL, time.Second)
	setStreamMax(t, s, "-1s")
	rec := doChat(t, s, streamMaxBody)
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("disabled limit interrupted stream: %s", rec.Body.String())
	}
}
