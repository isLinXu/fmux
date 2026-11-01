package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/openai"
	"github.com/isLinXu/fmux/internal/budget"
	"github.com/isLinXu/fmux/internal/compat"
	"github.com/isLinXu/fmux/internal/gate"
	"github.com/isLinXu/fmux/internal/ir"
	"github.com/isLinXu/fmux/internal/router"
)

const maxBodyBytes = 64 << 20

// errStreamIdle marks a stream the gateway gave up on because the backend
// stopped producing tokens.
var errStreamIdle = errors.New("stream idle timeout")

// errorCodeOf names the failure in the OpenAI error envelope.
func errorCodeOf(err error) string {
	switch {
	case errors.Is(err, gate.ErrBusy), errors.Is(err, gate.ErrQueueFull):
		return "backend_busy"
	case errors.Is(err, errStreamIdle):
		return "stream_idle"
	}
	return "backend_error"
}

// streamErrorCode names a failure that happened after the SSE headers were
// already on the wire, where the only remaining option is an error event.
func streamErrorCode(err error) string {
	if errors.Is(err, errStreamIdle) {
		return "stream_idle"
	}
	return "stream_error"
}

// backendDown reports whether err is evidence that the backend itself is gone,
// as opposed to the request being wrong or the backend being merely busy.
//
// The distinction matters because the answer is remembered for a cooldown: a
// 400 that opens the circuit would take a working backend out of service for
// every client, while a refused connection that does not open it makes every
// request pay the same timeout.
func backendDown(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, gate.ErrBusy), errors.Is(err, gate.ErrQueueFull):
		// Saturated, not dead. Backing off here would be wrong: the next
		// request may well get a slot.
		return false
	case errors.Is(err, errStreamIdle):
		// The backend accepted the connection and then went quiet. Suspicious,
		// but a cold restart costs a minute; one stall is not a pattern.
		return false
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller left, or a legitimately long generation hit its bound.
		return false
	}
	var he *openai.HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	// Whatever is left is a transport failure: refused, reset, no route.
	return true
}

// handleChat is the single entry point for completions.
//
// The per-target order is deliberate and load-bearing:
//
//	gate -> prepare -> chat
//
// Admission first, because starting a 18 GB process or evicting a resident
// model for a request that will only queue is wasted work, and because N
// concurrent cold requests would otherwise all try to start the same backend.
// Prepare second, because once a slot is held the caller owns the backend and
// may safely block on a cold start or an eviction.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)

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
	reqID := RequestID(r.Context())
	if len(quirks) > 0 {
		s.log.Debug("compat", "req_id", reqID, "client_is_fm", req.ClientIsFm, "quirks", quirks, "model", req.Model)
	}

	plan, err := s.rt.Resolve(req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}

	// Skip targets whose circuit is open, but only when something else can
	// take the request. A gateway that refuses everything because one backend
	// is down is worse than one that retries a dead backend.
	targets := plan.Targets
	if len(targets) > 1 {
		live := make([]router.Target, 0, len(targets))
		for _, t := range targets {
			if s.brk.Allow(t.Backend) {
				live = append(live, t)
			}
		}
		if len(live) > 0 {
			targets = live
		}
	}

	var lastErr error
	for i, target := range targets {
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
			s.log.Warn("degraded", "backend", d.Backend, "reason", d.Reason, "req_id", reqID)
		}

		wrote, err := s.serveTarget(r.Context(), w, cp, target, plan.Alias, reqID)
		if err == nil {
			s.brk.Succeed(target.Backend)
			return
		}
		if backendDown(err) {
			// Remember it, so the next request does not pay this timeout too.
			s.brk.Fail(target.Backend, err.Error())
			s.log.Warn("backend marked down", "backend", target.Backend,
				"err", err, "req_id", reqID)
		} else {
			// The backend answered, it just did not like the request.
			s.brk.Succeed(target.Backend)
		}
		lastErr = err
		if wrote {
			// Headers are already on the wire; nothing sane left to send.
			s.log.Error("stream aborted", "backend", target.Backend, "err", err, "req_id", reqID)
			return
		}
		// A dead client cannot be helped by a second backend.
		if r.Context().Err() != nil {
			return
		}
		s.log.Warn("target failed", "backend", target.Backend, "model", target.Model, "err", err, "req_id", reqID)
	}

	if lastErr == nil {
		lastErr = errors.New("no backend available")
	}
	status := StatusOf(lastErr)
	if status == http.StatusTooManyRequests {
		// Every target was busy rather than broken. Tell the client how long
		// the wait is expected to be instead of inviting an immediate retry.
		w.Header().Set("Retry-After", "5")
	}
	writeError(w, status, errorCodeOf(lastErr), lastErr.Error())
}

// serveTarget admits one request to one backend, prepares it, and serves it.
// It releases the admission slot before returning, whatever happened.
func (s *Server) serveTarget(ctx context.Context, w http.ResponseWriter, req *ir.ChatRequest, t router.Target, alias, reqID string) (bool, error) {
	release, err := s.gate.Acquire(ctx, t.Backend)
	if err != nil {
		return false, err
	}
	defer release()

	// Pin before preparing: once the model is resident it must not be evicted
	// to make room for someone else's model while this request is still
	// generating. The pin is taken first so a concurrent load cannot slip in
	// between the budget accounting and the first token.
	if s.bud != nil {
		key := budget.Key{Backend: t.Backend, Model: t.Model}
		s.bud.Pin(key)
		defer s.bud.Unpin(key)
	}

	// Local backends may need to start a process or free memory first. The
	// release drops the in-flight claim, so the idle reaper cannot stop this
	// process while the request is still generating.
	if p, ok := t.Adapter.(adapter.Preparer); ok {
		release, err := p.Prepare(ctx, t.Model)
		if err != nil {
			return false, err
		}
		defer release()
	}

	if req.Stream {
		return s.streamChat(ctx, w, req, t, alias)
	}
	return false, s.chatOnce(ctx, w, req, t, alias)
}

// chatOnce performs a non-streaming completion.
func (s *Server) chatOnce(ctx context.Context, w http.ResponseWriter, req *ir.ChatRequest, t router.Target, alias string) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeouts.NonStream)
	defer cancel()

	start := time.Now()
	resp, err := t.Adapter.Chat(ctx, req)
	if err != nil {
		s.observe(t, http.StatusBadGateway, nil, time.Since(start))
		return err
	}
	resp.Model = alias
	if resp.Usage == nil {
		resp.Usage = estimateUsage(req, collectedText(resp))
	}
	s.observe(t, http.StatusOK, resp.Usage, time.Since(start))
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// observe records a completed request for /metrics.
func (s *Server) observe(t router.Target, status int, u *ir.Usage, d time.Duration) {
	var prompt, completion int
	if u != nil {
		prompt, completion = u.PromptTokens, u.CompletionTokens
	}
	s.metrics.Observe(t.Backend, t.Model, status, prompt, completion, d)
}

// streamChat proxies a streaming completion, guaranteeing the tail that fm and
// other OpenAI clients expect: a finish chunk, an optional usage chunk, and
// the [DONE] sentinel.
func (s *Server) streamChat(ctx context.Context, w http.ResponseWriter, req *ir.ChatRequest, t router.Target, alias string) (bool, error) {
	start := time.Now()

	// A backend that stops producing tokens is not slow, it is stuck: the
	// process is alive, the socket is open, and nothing will ever arrive.
	// `stream_idle` is the only bound on that, and it is enforced by
	// cancelling the upstream request — the HTTP client then unblocks the
	// body read, so no goroutine is left behind watching a dead stream.
	idle := s.cfg.Timeouts.StreamIdle
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		idleTimer *time.Timer
		idleFired atomic.Bool
	)
	if idle > 0 {
		idleTimer = time.AfterFunc(idle, func() {
			idleFired.Store(true)
			cancel()
		})
		defer idleTimer.Stop()
	}

	st, err := t.Adapter.ChatStream(ctx, req)
	if err != nil {
		// Nothing has been written yet, so this can still be an honest 504
		// instead of a half-open 200. A backend that accepts the connection
		// and then says nothing is the same failure as one that goes quiet
		// mid-stream; the client should see the same code either way.
		if idleFired.Load() {
			err = fmt.Errorf("%w: %s sent no response within %s", errStreamIdle, t.Backend, idle)
			s.observe(t, http.StatusGatewayTimeout, nil, time.Since(start))
			return false, err
		}
		s.observe(t, http.StatusBadGateway, nil, time.Since(start))
		return false, err
	}
	defer st.Close()

	sw := newSSEWriter(w)
	firstPending := true
	finishSeen := false
	usageEmitted := false
	ttftSeen := false
	var usage *ir.Usage
	var text []byte

	for {
		ch, err := st.Recv()
		if idleTimer != nil && err == nil {
			idleTimer.Reset(idle)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if idleFired.Load() {
				// The client is still connected and waiting; it deserves an
				// explanation rather than a silent truncation.
				err = fmt.Errorf("%w: no token from %s for %s", errStreamIdle, t.Backend, idle)
				s.observe(t, http.StatusGatewayTimeout, nil, time.Since(start))
				if !sw.started {
					return false, err
				}
				sw.errorEvent(streamErrorCode(err), err.Error())
				return true, err
			}
			if !sw.started {
				return false, err
			}
			sw.errorEvent(streamErrorCode(err), err.Error())
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
		if !ttftSeen {
			// The first byte the client actually receives. On a local model
			// this is where a cold load shows up, and it is invisible in the
			// total duration once the generation itself is long.
			ttftSeen = true
			s.metrics.ObserveTTFT(t.Backend, time.Since(start))
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
	s.observe(t, http.StatusOK, usage, time.Since(start))
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

func (s *sseWriter) errorEvent(code, msg string) {
	s.start()
	payload, _ := json.Marshal(errorEnvelope{Error: errorBody{Message: msg, Type: "api_error", Code: code}})
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
