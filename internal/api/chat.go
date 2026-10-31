package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/linxu/fmux/internal/compat"
	"github.com/linxu/fmux/internal/ir"
	"github.com/linxu/fmux/internal/router"
)

const maxBodyBytes = 64 << 20

// handleChat is the single entry point for completions.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "could not read request body: "+err.Error())
		return
	}

	var req ir.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "malformed JSON body: "+err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	req.ClientIsFm = compat.LooksLikeFm(r.Header, &req)
	quirks, err := compat.Normalize(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(quirks) > 0 {
		s.log.Debug("compat", "client_is_fm", req.ClientIsFm, "quirks", quirks, "model", req.Model)
	}

	plan, err := s.rt.Resolve(req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}

	var lastErr error
	for i, target := range plan.Targets {
		cp := req.Clone()
		cp.ResolvedAlias = plan.Alias

		degradations, err := router.Prepare(cp, target, plan.Profile)
		if err != nil {
			// A capability mismatch on the primary target is a client error.
			if i == 0 {
				writeError(w, http.StatusBadRequest, "unsupported_capability", err.Error())
				return
			}
			lastErr = err
			continue
		}
		for _, d := range degradations {
			s.log.Warn("degraded", "backend", d.Backend, "reason", d.Reason)
		}

		if cp.Stream {
			wrote, err := s.streamChat(r.Context(), w, cp, target, plan.Alias)
			if err == nil {
				return
			}
			lastErr = err
			if wrote {
				// Headers are already on the wire; nothing sane left to send.
				s.log.Error("stream aborted", "backend", target.Backend, "err", err)
				return
			}
		} else {
			if err := s.chatOnce(r.Context(), w, cp, target, plan.Alias); err != nil {
				lastErr = err
			} else {
				return
			}
		}
		s.log.Warn("target failed", "backend", target.Backend, "model", target.Model, "err", lastErr)
	}

	if lastErr == nil {
		lastErr = errors.New("no backend available")
	}
	writeError(w, StatusOf(lastErr), "backend_error", lastErr.Error())
}

// chatOnce performs a non-streaming completion.
func (s *Server) chatOnce(ctx context.Context, w http.ResponseWriter, req *ir.ChatRequest, t router.Target, alias string) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeouts.NonStream)
	defer cancel()

	resp, err := t.Adapter.Chat(ctx, req)
	if err != nil {
		return err
	}
	resp.Model = alias
	if resp.Usage == nil {
		resp.Usage = estimateUsage(req, collectedText(resp))
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// streamChat proxies a streaming completion, guaranteeing the tail that fm and
// other OpenAI clients expect: a finish chunk, an optional usage chunk, and
// the [DONE] sentinel.
func (s *Server) streamChat(ctx context.Context, w http.ResponseWriter, req *ir.ChatRequest, t router.Target, alias string) (bool, error) {
	st, err := t.Adapter.ChatStream(ctx, req)
	if err != nil {
		return false, err
	}
	defer st.Close()

	sw := newSSEWriter(w)
	firstPending := true
	finishSeen := false
	usageEmitted := false
	var usage *ir.Usage
	var text []byte

	for {
		ch, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if !sw.started {
				return false, err
			}
			sw.errorEvent(err.Error())
			return true, err
		}

		if ch.Usage != nil {
			usage = ch.Usage
		}
		for i := range ch.Choices {
			if ch.Choices[i].FinishReason != nil {
				finishSeen = true
			}
			if firstPending && len(ch.Choices) > 0 {
				if ch.Choices[i].Delta.Role == "" {
					ch.Choices[i].Delta.Role = "assistant"
				}
				firstPending = false
			}
			text = append(text, ch.Choices[i].Delta.Content...)
		}
		ch.Model = alias
		if !req.WantsUsageInStream() {
			ch.Usage = nil
		} else if ch.Usage != nil && len(ch.Choices) == 0 {
			usageEmitted = true
		}
		if err := sw.event(ch); err != nil {
			return true, err
		}
	}

	if firstPending {
		head := ir.NewChunk(alias)
		head.Choices = []ir.ChunkChoice{{Index: 0, Delta: ir.Delta{Role: "assistant", Content: ""}}}
		if err := sw.event(head); err != nil {
			return true, err
		}
	}

	if !finishSeen {
		tail := ir.NewChunk(alias)
		stop := "stop"
		tail.Choices = []ir.ChunkChoice{{Index: 0, Delta: ir.Delta{}, FinishReason: &stop}}
		if err := sw.event(tail); err != nil {
			return true, err
		}
	}

	if req.WantsUsageInStream() && !usageEmitted {
		if usage == nil {
			usage = estimateUsage(req, string(text))
			s.log.Debug("usage estimated", "backend", t.Backend)
		}
		final := ir.NewChunk(alias)
		final.Choices = []ir.ChunkChoice{}
		final.Usage = usage
		if err := sw.event(final); err != nil {
			return true, err
		}
	}

	if err := sw.done(); err != nil {
		return true, err
	}
	return true, nil
}

// sseWriter writes Server-Sent Events with explicit flushing.
type sseWriter struct {
	w       http.ResponseWriter
	f       http.Flusher
	started bool
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, f: f}
}

func (s *sseWriter) start() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
}

func (s *sseWriter) event(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.start()
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}

func (s *sseWriter) errorEvent(msg string) {
	s.start()
	payload, _ := json.Marshal(errorEnvelope{Error: errorBody{Message: msg, Type: "api_error", Code: "stream_error"}})
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", payload)
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *sseWriter) done() error {
	s.start()
	if _, err := io.WriteString(s.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}

func collectedText(resp *ir.ChatResponse) string {
	var b []byte
	for _, c := range resp.Choices {
		b = append(b, c.Message.Content.PlainText()...)
	}
	return string(b)
}

// estimateUsage is a rough fallback when a backend reports no usage. It is
// deliberately conservative: ~4 bytes per token for latin text, ~1 token per
// CJK rune.
func estimateUsage(req *ir.ChatRequest, completion string) *ir.Usage {
	p := 0
	for _, m := range req.Messages {
		p += estimateTokens(m.Content.PlainText())
	}
	c := estimateTokens(completion)
	return &ir.Usage{PromptTokens: p, CompletionTokens: c, TotalTokens: p + c}
}

func estimateTokens(s string) int {
	n, latin := 0, 0
	for _, r := range s {
		if r < 128 {
			latin++
		} else {
			n++
		}
	}
	return n + latin/4 + 1
}
