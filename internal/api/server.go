package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/linxu/fmux/internal/adapter"
	"github.com/linxu/fmux/internal/adapter/openai"
	"github.com/linxu/fmux/internal/breaker"
	"github.com/linxu/fmux/internal/budget"
	"github.com/linxu/fmux/internal/config"
	"github.com/linxu/fmux/internal/gate"
	"github.com/linxu/fmux/internal/metrics"
	"github.com/linxu/fmux/internal/router"
	"github.com/linxu/fmux/internal/supervisor"
)

// Version is stamped at build time.
var Version = "0.0.0-dev"

// Server holds the HTTP dependencies.
type Server struct {
	cfg     *config.Config
	reg     *adapter.Registry
	rt      *router.Router
	sup     *supervisor.Supervisor
	bud     *budget.Manager
	gate    *gate.Gate
	brk     *breaker.Breaker
	metrics *metrics.Registry
	log     *slog.Logger
	started time.Time

	// inflight counts chat requests that have not finished. Shutdown uses it
	// to tell "drained cleanly" from "we just cut off N generations".
	inflight atomic.Int64
}

// Inflight reports how many chat requests are still being served. It is the
// number that decides whether a shutdown is safe to finish.
func (s *Server) Inflight() int64 { return s.inflight.Load() }

// Options carries everything a server needs. Every field except Config and
// Registry is optional, which keeps the zero-config test path simple.
type Options struct {
	Config     *config.Config
	Registry   *adapter.Registry
	Supervisor *supervisor.Supervisor
	Budget     *budget.Manager
	Gate       *gate.Gate
	Breaker    *breaker.Breaker
	Metrics    *metrics.Registry
	Log        *slog.Logger
}

// New builds a server.
func New(o Options) *Server {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	met := o.Metrics
	if met == nil {
		met = metrics.New()
	}
	g := o.Gate
	if g == nil {
		// No gate configured means every backend is unlimited, which is the
		// right default for tests and for remote-only setups.
		g = gate.New(gate.Limits{}, gate.Hooks{}, log)
	}
	brk := o.Breaker
	if brk == nil {
		brk = breaker.New(breaker.Options{})
	}
	return &Server{
		cfg:     o.Config,
		reg:     o.Registry,
		rt:      router.New(o.Config, o.Registry),
		sup:     o.Supervisor,
		bud:     o.Budget,
		gate:    g,
		brk:     brk,
		metrics: met,
		log:     log,
		started: time.Now(),
	}
}

// Handler returns the routed http.Handler.
//
// Order matters: the panic recoverer is outermost so a crash inside the
// request-id or guard middleware is still caught, and the guard runs before
// anything that touches a backend.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	mux.HandleFunc("GET /admin/backends", s.handleBackends)
	mux.HandleFunc("GET /admin/processes", s.handleProcesses)
	mux.HandleFunc("GET /admin/config", s.handleConfig)
	mux.HandleFunc("GET /admin/version", s.handleVersion)

	return s.recoverer(requestID(s.guard(s.accessLog(mux))))
}

// accessLog records method, path, status and duration.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http",
			"req_id", RequestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"dur_ms", time.Since(start).Milliseconds(),
			"ua", r.UserAgent(),
		)
	})
}

// recoverer turns a panic into a 500 instead of killing the connection.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic", "err", fmt.Sprint(v), "path", r.URL.Path, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal_error", fmt.Sprintf("panic: %v", v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        Version,
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"backends":       len(s.reg.Names()),
		"inflight":       s.Inflight(),
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": Version,
		"go":      runtime.Version(),
	})
}

// errorEnvelope mirrors OpenAI's error shape so clients parse it natively.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	typ := "invalid_request_error"
	switch {
	case status >= 500:
		typ = "api_error"
	case status == http.StatusTooManyRequests:
		// 429 is the one 4xx that is not the client's fault: the request was
		// well formed and the backend is simply occupied.
		typ = "rate_limit_error"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		typ = "permission_error"
	}
	writeJSON(w, status, errorEnvelope{Error: errorBody{Message: msg, Type: typ, Code: code}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"message":"encode failure"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// StatusOf extracts an HTTP status from an adapter error when possible.
func StatusOf(err error) int {
	switch {
	case errors.Is(err, gate.ErrBusy), errors.Is(err, gate.ErrQueueFull):
		// The backend is healthy and busy; the request should be retried.
		return http.StatusTooManyRequests
	case errors.Is(err, errStreamIdle):
		return http.StatusGatewayTimeout
	}
	var he *openai.HTTPError
	if errors.As(err, &he) && he.StatusCode > 0 {
		return he.StatusCode
	}
	return http.StatusBadGateway
}
