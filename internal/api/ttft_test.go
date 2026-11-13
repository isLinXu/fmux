package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/openai"
	"github.com/isLinXu/fmux/internal/gate"
)

const ttftBody = `{"model":"p","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
const ttftRole = `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"
const ttftText = `data: {"choices":[{"index":0,"delta":{"content":"alpha"}}]}` + "\n\n"
const ttftUsage = `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n"
const ttftToolMetadata = `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call","type":"function","function":{}}]}}]}` + "\n\n"
const ttftFinish = `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"

func ttftBackend(t *testing.T, prefix, output, tail string, delay time.Duration) *httptest.Server {
	t.Helper()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, prefix)
		w.(http.Flusher).Flush()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		_, _ = io.WriteString(w, output+tail)
	}))
	t.Cleanup(be.Close)
	return be
}

func ttftStats(t *testing.T, s *Server, backend string) (count, sum int64, present bool) {
	t.Helper()
	var buf bytes.Buffer
	if err := s.metrics.Render(&buf); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		for _, field := range []string{"count", "sum"} {
			prefix := fmt.Sprintf("fmux_time_to_first_token_milliseconds_%s{backend=%q} ", field, backend)
			if raw, ok := strings.CutPrefix(line, prefix); ok {
				n, err := strconv.ParseInt(raw, 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				if field == "count" {
					count, present = n, true
				} else {
					sum = n
				}
			}
		}
	}
	return
}

func assertTTFT(t *testing.T, s *Server, backend string, count, minimumMS int64) {
	t.Helper()
	gotCount, sum, present := ttftStats(t, s, backend)
	if gotCount != count || (count == 0 && present) || sum < minimumMS {
		t.Errorf("TTFT backend=%s count=%d sum=%d present=%t want count=%d sum>=%d",
			backend, gotCount, sum, present, count, minimumMS)
	}
}

type ttftPreparingAdapter struct {
	adapter.Adapter
	delay time.Duration
}

func (a ttftPreparingAdapter) Prepare(ctx context.Context, _ string) (func(), error) {
	timer := time.NewTimer(a.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return func() {}, ctx.Err()
	case <-timer.C:
		return func() {}, nil
	}
}

func TestTTFTIncludesPreparation(t *testing.T) {
	be := ttftBackend(t, "", ttftText, ttftFinish+"data: [DONE]\n\n", 0)
	s := pacedServer(t, be.URL, time.Second)
	a, _ := s.reg.Get("paced")
	s.reg.Add(ttftPreparingAdapter{a, 80 * time.Millisecond})
	rec := doChat(t, s, ttftBody)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	assertTTFT(t, s, "paced", 1, 60)
}

func TestTTFTIncludesGateWait(t *testing.T) {
	be := ttftBackend(t, "", ttftText, ttftFinish+"data: [DONE]\n\n", 0)
	s := pacedServer(t, be.URL, time.Second)
	g := gate.New(gate.Limits{}, gate.Hooks{}, testLogger())
	g.Configure("paced", gate.Limits{Max: 1, Queue: 1, Timeout: 2 * time.Second})
	s.gate = g
	release, err := g.Acquire(t.Context(), "paced")
	if err != nil {
		t.Fatal(err)
	}
	unblock := sync.OnceFunc(release)
	t.Cleanup(unblock)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doChat(t, s, ttftBody) }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
wait:
	for {
		if g.Stats()[0].Waiting == 1 {
			break
		}
		select {
		case <-deadline.C:
			unblock()
			<-done
			t.Fatal("request never entered gate queue")
		case <-tick.C:
			continue wait
		}
	}
	time.Sleep(80 * time.Millisecond)
	unblock()
	if rec := <-done; rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	assertTTFT(t, s, "paced", 1, 60)
}

func TestTTFTWaitsForTextOrToolOutput(t *testing.T) {
	for _, tc := range []struct{ name, output string }{
		{"text", ttftText},
		{"whitespace", `data: {"choices":[{"delta":{"content":" "}}]}` + "\n\n"},
		{"tool_name", `data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"ocr"}}]}}]}` + "\n\n"},
		{"tool_arguments", `data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}` + "\n\n"},
		{"second_choice", `data: {"choices":[{"index":0,"delta":{}},{"index":1,"delta":{"content":"alpha"}}]}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := ttftBackend(t, ttftRole+ttftUsage+ttftToolMetadata, tc.output,
				ttftText+ttftFinish+"data: [DONE]\n\n", 80*time.Millisecond)
			s := pacedServer(t, be.URL, time.Second)
			if rec := doChat(t, s, ttftBody); rec.Code != 200 {
				t.Fatal(rec.Body.String())
			}
			assertTTFT(t, s, "paced", 1, 60)
		})
	}
}

func TestTTFTControlOnlyHasNoSample(t *testing.T) {
	for _, tc := range []struct{ name, wire string }{
		{"empty", ""},
		{"role", ttftRole},
		{"usage", ttftUsage},
		{"empty_tool", ttftToolMetadata},
		{"finish", ttftFinish},
		{"empty_delta", "data: {\"choices\":[{\"delta\":{}}]}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := ttftBackend(t, "", tc.wire, "data: [DONE]\n\n", 0)
			s := pacedServer(t, be.URL, time.Second)
			if rec := doChat(t, s, ttftBody); rec.Code != 200 {
				t.Fatal(rec.Body.String())
			}
			assertTTFT(t, s, "paced", 0, 0)
		})
	}
}

func TestTTFTFallbackKeepsRequestStart(t *testing.T) {
	be := ttftBackend(t, "", "", upstreamFailure, 80*time.Millisecond)
	s := newFallbackServer(t, be.URL)
	s.reg.Add(openai.New(openai.Options{
		Name: "dead", BaseURL: be.URL + "/v1", Caps: adapter.Capabilities{Stream: true},
	}))
	rec := doChat(t, s, strings.Replace(ttftBody, `"p"`, `"auto"`, 1))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "mock:") {
		t.Fatal(rec.Body.String())
	}
	assertTTFT(t, s, "dead", 0, 0)
	assertTTFT(t, s, "mock", 1, 60)
}

func TestTTFTErrorsDoNotInventOrDuplicateSamples(t *testing.T) {
	for _, tc := range []struct {
		name, prefix string
		count        int64
	}{
		{"before", "", 0},
		{"after_role", ttftRole, 0},
		{"after_text", ttftText, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := ttftBackend(t, tc.prefix, "", upstreamFailure, 0)
			s := pacedServer(t, be.URL, time.Second)
			rec := doChat(t, s, ttftBody)
			if !strings.Contains(rec.Body.String(), "injected_error") || strings.Contains(rec.Body.String(), "[DONE]") {
				t.Fatal(rec.Body.String())
			}
			assertTTFT(t, s, "paced", tc.count, 0)
		})
	}
}

func TestTTFTNonStreamHasNoSample(t *testing.T) {
	s, _ := newServer(t)
	rec := doChat(t, s, `{"model":"mock-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	assertTTFT(t, s, "mock", 0, 0)
}

type ttftFailingWriter struct{ *httptest.ResponseRecorder }

func (w ttftFailingWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"content":"alpha"`)) {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}

func TestTTFTFailedOutputWriteHasNoSample(t *testing.T) {
	be := ttftBackend(t, ttftRole, ttftText, "data: [DONE]\n\n", 0)
	s := pacedServer(t, be.URL, time.Second)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(ttftBody))
	req.Host = "127.0.0.1:1976"
	s.Handler().ServeHTTP(ttftFailingWriter{httptest.NewRecorder()}, req)
	assertTTFT(t, s, "paced", 0, 0)
}

func TestTTFTMaximumDurationBeforeOutputHasNoSample(t *testing.T) {
	be := ttftBackend(t, ttftRole, ttftText, "data: [DONE]\n\n", 500*time.Millisecond)
	s := pacedServer(t, be.URL, time.Second)
	setStreamMax(t, s, "40ms")
	rec := doChat(t, s, ttftBody)
	if !strings.Contains(rec.Body.String(), "stream_max_duration") {
		t.Fatal(rec.Body.String())
	}
	assertTTFT(t, s, "paced", 0, 0)
}

func TestTTFTCancellationBeforeOutputHasNoSample(t *testing.T) {
	be := ttftBackend(t, ttftRole, ttftText, "data: [DONE]\n\n", time.Second)
	s := pacedServer(t, be.URL, time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(ttftBody)).WithContext(ctx)
	req.Host = "127.0.0.1:1976"
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	assertTTFT(t, s, "paced", 0, 0)
}
