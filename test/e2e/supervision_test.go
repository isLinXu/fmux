// Package e2e exercises the gateway with a real child process, which is the
// part unit tests cannot reach: process spawn, health probe, reuse, and reap.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isLinXu/fmux/internal/api"
	"github.com/isLinXu/fmux/internal/build"
	"github.com/isLinXu/fmux/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

type harness struct {
	ts     *httptest.Server
	srv    *api.Server
	bundle *build.Bundle
	url    string
}

func newHarness(t *testing.T, port int) *harness {
	t.Helper()
	return newHarnessEnv(t, port, nil)
}

func newHarnessEnv(t *testing.T, port int, env map[string]string) *harness {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	fixture, err := filepath.Abs("../fixtures/fake_openai.py")
	if err != nil {
		t.Fatalf("fixture path: %v", err)
	}

	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		LogLevel: "error",
		BudgetMB: 4096,
		LogDir:   t.TempDir(),
		Backends: []config.Backend{{
			Name:           "fake",
			Type:           "llamacpp",
			URL:            fmt.Sprintf("http://127.0.0.1:%d/v1", port),
			Command:        []string{"python3", fixture, fmt.Sprint(port), "fake-model"},
			Env:            env,
			MemoryMB:       1024,
			StartupTimeout: 15 * time.Second,
			StopTimeout:    5 * time.Second,
			IdleTimeout:    time.Minute,
		}},
		Aliases: map[string]config.Alias{"tiny": {Backend: "fake", Model: "fake-model"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	bundle, err := build.Build(cfg, quietLogger())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	srv := api.New(api.Options{
		Config:     cfg,
		Registry:   bundle.Registry,
		Supervisor: bundle.Supervisor,
		Budget:     bundle.Budget,
		Metrics:    bundle.Metrics,
		Log:        quietLogger(),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		bundle.Supervisor.Shutdown(context.Background())
	})
	return &harness{ts: ts, srv: srv, bundle: bundle, url: ts.URL}
}

func (h *harness) chat(t *testing.T, model string) string {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
	resp, err := http.Post(h.url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Choices) == 0 {
		t.Fatalf("no choices in %s", raw)
	}
	return out.Choices[0].Message.Content
}

func (h *harness) process(t *testing.T) (state string, pid int) {
	t.Helper()
	resp, err := http.Get(h.url + "/admin/processes")
	if err != nil {
		t.Fatalf("processes: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Processes []struct {
			State string `json:"state"`
			PID   int    `json:"pid"`
		} `json:"processes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Processes) != 1 {
		t.Fatalf("processes = %+v", out.Processes)
	}
	return out.Processes[0].State, out.Processes[0].PID
}

// TestSupervisedBackendLifecycle is the M2/M3 acceptance test: a backend that
// is not running gets started by the first request, reused by the second, and
// its memory is accounted for while it lives.
func TestSupervisedBackendLifecycle(t *testing.T) {
	port := freePort(t)
	h := newHarness(t, port)

	if state, _ := h.process(t); state != "stopped" {
		t.Fatalf("initial state = %q, want stopped", state)
	}
	if used := h.bundle.Budget.UsedMB(); used != 0 {
		t.Fatalf("initial budget used = %d, want 0", used)
	}

	if got := h.chat(t, "tiny"); !strings.Contains(got, "fake backend") {
		t.Fatalf("completion = %q", got)
	}
	state, pid := h.process(t)
	if state != "running" || pid == 0 {
		t.Fatalf("after first request: state=%q pid=%d", state, pid)
	}
	if used := h.bundle.Budget.UsedMB(); used != 1024 {
		t.Fatalf("budget used = %d, want 1024", used)
	}

	// Second request must reuse the process rather than spawn another.
	if got := h.chat(t, "tiny"); !strings.Contains(got, "fake backend") {
		t.Fatalf("second completion = %q", got)
	}
	if _, pid2 := h.process(t); pid2 != pid {
		t.Fatalf("pid changed on reuse: %d -> %d", pid, pid2)
	}

	// Stopping the process must release its memory accounting too.
	if err := h.bundle.Supervisor.Stop("fake"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if state, _ := h.process(t); state != "stopped" {
		t.Fatalf("after stop: state = %q", state)
	}
	if used := h.bundle.Budget.UsedMB(); used != 0 {
		t.Fatalf("budget used after stop = %d, want 0", used)
	}
}

// TestMetricsRecordRequests checks the counters the /metrics endpoint exposes.
func TestMetricsRecordRequests(t *testing.T) {
	h := newHarness(t, freePort(t))
	h.chat(t, "tiny")

	resp, err := http.Get(h.url + "/metrics")
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	for _, want := range []string{
		`fmux_requests_total{backend="fake",model="fake-model",status="200"} 1`,
		"fmux_prompt_tokens_total",
		"fmux_uptime_seconds",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q\n%s", want, text)
		}
	}
}

// TestShutdownDrainsBeforeStoppingChildren is the regression test for the
// shutdown ordering bug.
//
// The gateway used to stop its child backends first and drain HTTP second.
// That reads as harmless until you notice the children are what in-flight
// requests are reading from: a generation that was halfway done would be
// truncated with no error event, and because the process was gone the client's
// retry paid a full cold start. Here the fake backend holds the stream open
// for two seconds, shutdown is triggered while it is open, and the request has
// to come back complete.
func TestShutdownDrainsBeforeStoppingChildren(t *testing.T) {
	port := freePort(t)
	h := newHarnessEnv(t, port, map[string]string{"FAKE_STREAM_DELAY": "2"})

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body := `{"model":"tiny","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post(h.url+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		done <- result{body: string(raw), err: err}
	}()

	// Wait until the request is genuinely inside the gateway before draining.
	deadline := time.Now().Add(10 * time.Second)
	for h.srv.Inflight() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request never reached the gateway")
		}
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	api.DrainAndStop(quietLogger(), h.ts.Config, h.srv, h.bundle.Supervisor, 15*time.Second)
	drained := time.Since(start)

	got := <-done
	if got.err != nil {
		t.Fatalf("in-flight request failed during shutdown: %v", got.err)
	}
	if !strings.Contains(got.body, "served by the fake backend") {
		t.Fatalf("response was truncated:\n%s", got.body)
	}
	if !strings.Contains(got.body, "[DONE]") {
		t.Fatalf("stream ended without [DONE]:\n%s", got.body)
	}
	if drained < time.Second {
		t.Fatalf("shutdown returned in %s, before the 2s stream could finish", drained)
	}
	if st := h.bundle.Supervisor.Status()[0]; st.State != "stopped" {
		t.Fatalf("child process state after shutdown = %q, want stopped", st.State)
	}
	if n := h.srv.Inflight(); n != 0 {
		t.Fatalf("inflight after shutdown = %d", n)
	}
}
