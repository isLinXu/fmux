package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/openai"
	"github.com/isLinXu/fmux/internal/ir"
)

const upstreamPrefix = `data: {"id":"upstream-id","created":1700000000,"model":"upstream","choices":[{"index":0,"delta":{"content":"alpha"}}]}` + "\n\n"
const upstreamFinish = `data: {"id":"upstream-id","created":1700000000,"model":"upstream","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
const upstreamFailure = `data: {"error":{"message":"injected failure","type":"server_error","code":"injected_error","param":"model"}}` + "\n\n"

func newSSETestServer(t *testing.T, wire string) (*Server, *atomic.Int64) {
	t.Helper()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	t.Cleanup(be.Close)
	var hits atomic.Int64
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.ReplaceAll(upstreamPrefix, "alpha", "backup")+
			upstreamFinish+"data: [DONE]\n\n")
	}))
	t.Cleanup(backup.Close)
	s := newFallbackServer(t, be.URL)
	for name, url := range map[string]string{"dead": be.URL, "mock": backup.URL} {
		s.reg.Add(openai.New(openai.Options{
			Name: name, BaseURL: url + "/v1",
			Caps: adapter.Capabilities{Stream: true, UsageInStream: true},
		}))
	}
	return s, &hits
}

func upstreamRequest(model string) string {
	return fmt.Sprintf(`{"model":%q,"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, model)
}

func TestUpstreamStreamFallback(t *testing.T) {
	s, hits := newSSETestServer(t, upstreamFailure+"data: [DONE]\n\n")
	rec := post(t, s, upstreamRequest("auto"))
	if rec.Code != 200 || hits.Load() != 1 || !strings.Contains(rec.Body.String(), "backup") ||
		!strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("status=%d backup hits=%d body=%s", rec.Code, hits.Load(), rec.Body.String())
	}
	for _, report := range s.brk.Snapshot() {
		if report.Fails != 0 || report.Open {
			t.Fatalf("application error marked backend down: %+v", report)
		}
	}
}

func TestUpstreamStreamErrorFields(t *testing.T) {
	for _, code := range []string{`"injected_error"`, "42", "null"} {
		for _, prefix := range []string{"", upstreamPrefix,
			`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"} {
			t.Run(fmt.Sprintf("code=%s/started=%t", code, prefix != ""), func(t *testing.T) {
				wire := prefix + strings.Replace(upstreamFailure, `"injected_error"`, code, 1)
				s, hits := newSSETestServer(t, wire)
				model := "lonely"
				if prefix != "" {
					model = "auto"
				}
				rec := post(t, s, upstreamRequest(model))
				want := 502
				if prefix != "" {
					want = 200
				}
				body := rec.Body.String()
				if rec.Code != want || hits.Load() != 0 {
					t.Fatalf("status=%d want=%d backup hits=%d body=%s", rec.Code, want, hits.Load(), body)
				}
				for _, field := range []string{`"message":"injected failure"`, `"type":"server_error"`,
					`"code":` + code, `"param":"model"`} {
					if !strings.Contains(body, field) {
						t.Errorf("lost error field %s: %s", field, body)
					}
				}
				if strings.Contains(body, "[DONE]") || strings.Contains(body, `"finish_reason":"stop"`) ||
					strings.Contains(body, `"usage":`) {
					t.Errorf("error emitted success tail: %s", body)
				}
				if prefix == upstreamPrefix && !strings.Contains(body, "alpha") {
					t.Errorf("lost content prefix: %s", body)
				}
			})
		}
	}
}

func TestUpstreamStreamErrorDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
	}{
		{"empty_envelope", "data: {\"error\":{}}\n\n"},
		{"event_without_envelope", "event: error\ndata: plain failure\n\n"},
		{"event_without_data", "event: error\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newSSETestServer(t, tc.wire)
			rec := post(t, s, upstreamRequest("lonely"))
			var envelope errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if rec.Code != 502 || envelope.Error.Message == "" || envelope.Error.Type != "api_error" ||
				envelope.Error.Code != "backend_error" {
				t.Errorf("missing safe defaults: status=%d error=%+v", rec.Code, envelope.Error)
			}
		})
	}
}

func TestStreamMetadata(t *testing.T) {
	const usage = `data: {"id":"upstream-id","created":1700000000,"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}` + "\n\n"
	for _, tc := range []struct {
		name, wire string
		upstream   bool
	}{
		{"eof", upstreamPrefix, true},
		{"missing_usage", upstreamPrefix + upstreamFinish + "data: [DONE]\n\n", true},
		{"missing_finish", upstreamPrefix + usage + "data: [DONE]\n\n", true},
		{"usage_only", usage, true},
		{"empty", "data: [DONE]\n\n", false},
		{"missing_fields", `data: {"choices":[{"delta":{"content":"alpha"}}]}` + "\n\n", false},
		{"conflict", upstreamPrefix + strings.ReplaceAll(strings.ReplaceAll(upstreamFinish,
			"upstream-id", "other-id"), "1700000000", "1800000000"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newSSETestServer(t, tc.wire)
			rec := post(t, s, upstreamRequest("lonely"))
			chunks := parseSSEChunks(t, rec.Body.String())
			if rec.Code != 200 || len(chunks) < 2 || !strings.Contains(rec.Body.String(), "[DONE]") {
				t.Fatalf("status=%d chunks=%d body=%s", rec.Code, len(chunks), rec.Body.String())
			}
			first := chunks[0]
			if first.ID == "" || first.Created == 0 || first.Object != "chat.completion.chunk" || first.Model != "lonely" {
				t.Fatalf("invalid metadata: %+v", first)
			}
			if tc.upstream && (first.ID != "upstream-id" || first.Created != 1700000000) {
				t.Errorf("lost upstream metadata: %+v", first)
			}
			for _, ch := range chunks {
				if ch.ID != first.ID || ch.Created != first.Created || ch.Object != first.Object || ch.Model != first.Model {
					t.Errorf("metadata changed: first=%+v chunk=%+v", first, ch)
				}
			}
			var reported *ir.Usage
			for _, ch := range chunks {
				if ch.Usage != nil {
					reported = ch.Usage
				}
			}
			if reported == nil || reported.TotalTokens != reported.PromptTokens+reported.CompletionTokens {
				t.Errorf("missing/inconsistent usage: %+v", reported)
			}
			if (tc.name == "usage_only" || tc.name == "missing_finish") &&
				(reported == nil || *reported != (ir.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4})) {
				t.Errorf("upstream usage changed: %+v", reported)
			}
		})
	}
}

func parseSSEChunks(t *testing.T, body string) []ir.Chunk {
	t.Helper()
	var chunks []ir.Chunk
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var ch ir.Chunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ch); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, ch)
	}
	return chunks
}
