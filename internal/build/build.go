// Package build wires configuration into concrete adapters.
package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/local"
	"github.com/isLinXu/fmux/internal/adapter/mock"
	"github.com/isLinXu/fmux/internal/adapter/openai"
	"github.com/isLinXu/fmux/internal/budget"
	"github.com/isLinXu/fmux/internal/config"
	"github.com/isLinXu/fmux/internal/gate"
	"github.com/isLinXu/fmux/internal/metrics"
	"github.com/isLinXu/fmux/internal/supervisor"
)

// Bundle is everything the HTTP layer needs to serve requests.
type Bundle struct {
	Registry   *adapter.Registry
	Supervisor *supervisor.Supervisor
	Budget     *budget.Manager
	Gate       *gate.Gate
	Metrics    *metrics.Registry
}

// CapabilitiesFor returns the capability matrix for a backend type.
//
// These defaults encode what each server actually supports, not what its
// documentation claims. They are intentionally conservative: over-promising
// here means a 400 from the backend instead of a clear gateway error.
func CapabilitiesFor(typ string) adapter.Capabilities {
	switch typ {
	case "ollama":
		return adapter.Capabilities{
			Stream: true, Vision: true, JSONSchema: true, Tools: true,
			UsageInStream: true, MaxContext: 32768,
		}
	case "llamacpp":
		// llama.cpp serves one model per process and reports no usage in
		// streaming mode unless --metrics is on; its JSON schema support is a
		// GBNF grammar, which is stricter than OpenAI's, so keep it.
		return adapter.Capabilities{
			Stream: true, Vision: false, JSONSchema: true, Tools: true,
			UsageInStream: false, MaxContext: 32768,
		}
	case "mlx":
		// mlx_lm.server ignores response_format and tools entirely.
		return adapter.Capabilities{
			Stream: true, Vision: false, JSONSchema: false, Tools: false,
			UsageInStream: false, MaxContext: 32768,
		}
	case "lmstudio":
		return adapter.Capabilities{
			Stream: true, Vision: true, JSONSchema: true, Tools: true,
			UsageInStream: true, MaxContext: 32768,
		}
	case "vllm", "sglang":
		return adapter.Capabilities{
			Stream: true, Vision: true, JSONSchema: true, Tools: true,
			UsageInStream: true, MaxContext: 131072,
		}
	case "mock":
		return adapter.Capabilities{
			Stream: true, Vision: true, JSONSchema: true, Tools: true,
			UsageInStream: true, MaxContext: 8192,
		}
	default: // generic OpenAI-compatible
		return adapter.Capabilities{
			Stream: true, Vision: true, JSONSchema: true, Tools: true,
			UsageInStream: true, MaxContext: 32768,
		}
	}
}

// Build constructs the registry, supervisor, budget and metrics for a config.
func Build(cfg *config.Config, log *slog.Logger) (*Bundle, error) {
	if log == nil {
		log = slog.Default()
	}
	reg := adapter.NewRegistry()
	sup := supervisor.New(log)
	met := metrics.New()

	bud := budget.New(cfg.BudgetMB, func(ctx context.Context, k budget.Key) error {
		met.Evicted()
		return evict(ctx, cfg, sup, k)
	}, log)

	// A stopped process holds no memory, so its residency records must go too.
	sup.SetOnStop(func(name string) { bud.ReleaseBackend(name) })

	// Admission control. The hooks feed /metrics without the gate package
	// having to know what a metric is.
	gt := gate.New(gate.Limits{}, gate.Hooks{
		Wait:     func(backend string, d time.Duration) { met.QueueWait(backend, d) },
		Rejected: func(backend, reason string) { met.Rejected(backend, reason) },
		Inflight: func(backend string, n int) { met.SetInflight(backend, n) },
	}, log)

	client := &http.Client{Timeout: 0}
	for _, b := range cfg.Backends {
		gt.Configure(b.Name, gate.Limits{
			Max:     b.MaxConcurrency,
			Queue:   b.QueueDepth,
			Timeout: b.QueueTimeout,
		})
		if b.Type == "mock" {
			reg.Add(mock.New(b.Name))
			continue
		}
		if b.URL == "" {
			return nil, fmt.Errorf("backend %q: url is required", b.Name)
		}

		opts := openai.Options{
			Name:    b.Name,
			Type:    b.Type,
			BaseURL: b.URL,
			APIKey:  b.APIKey,
			Headers: b.Headers,
			Caps:    CapabilitiesFor(b.Type),
			Client:  client,
		}

		if len(b.Command) > 0 {
			sup.Register(supervisor.Spec{
				Name:           b.Name,
				Command:        b.Command,
				Env:            b.Env,
				HealthURL:      HealthURL(b),
				StartupTimeout: b.StartupTimeout,
				StopTimeout:    b.StopTimeout,
				IdleTimeout:    b.IdleTimeout,
				LogPath:        filepath.Join(cfg.ResolvedLogDir(), b.Name+".log"),
			})
		}

		if config.KindOf(b.Type) == "local" {
			reg.Add(local.New(local.Options{
				OpenAI:     opts,
				Supervisor: sup,
				Budget:     bud,
				SizeMB:     b.MemoryMB,
			}))
			continue
		}
		reg.Add(openai.New(opts))
	}
	if len(reg.Names()) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	return &Bundle{Registry: reg, Supervisor: sup, Budget: bud, Gate: gt, Metrics: met}, nil
}

// Registry builds just the adapter registry. Commands that only inspect
// configuration (backends, models, doctor) use this and never start processes.
func Registry(cfg *config.Config) (*adapter.Registry, error) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := Build(cfg, quiet)
	if err != nil {
		return nil, err
	}
	return b.Registry, nil
}

// HealthURL derives the URL a supervised process is probed on.
//
// llama.cpp exposes /health; everything else answers on /v1/models. A backend
// may override this with health_path, absolute or relative to the origin.
func HealthURL(b config.Backend) string {
	path := b.HealthPath
	if path == "" {
		switch b.Type {
		case "llamacpp":
			path = "/health"
		default:
			path = "/v1/models"
		}
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	origin := b.URL
	if i := strings.Index(origin, "/v1"); i >= 0 {
		origin = origin[:i]
	}
	return strings.TrimRight(origin, "/") + path
}

// evict unloads a model so another one can take its memory.
func evict(ctx context.Context, cfg *config.Config, sup *supervisor.Supervisor, k budget.Key) error {
	b, ok := cfg.BackendByName(k.Backend)
	if !ok {
		return nil
	}
	switch b.Type {
	case "ollama":
		return unloadOllama(ctx, b.URL, k.Model)
	}
	if len(b.Command) > 0 {
		return sup.Stop(k.Backend)
	}
	return nil
}

// unloadOllama asks Ollama to drop a model immediately (keep_alive: 0).
func unloadOllama(ctx context.Context, baseURL, model string) error {
	origin := baseURL
	if i := strings.Index(origin, "/v1"); i >= 0 {
		origin = origin[:i]
	}
	body, err := json.Marshal(map[string]any{"model": model, "keep_alive": 0})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ollama unload returned %s", resp.Status)
	}
	return nil
}

// DialTimeout returns the dial timeout, with a sane floor.
func DialTimeout(cfg *config.Config) time.Duration {
	if cfg.Timeouts.Dial <= 0 {
		return 5 * time.Second
	}
	return cfg.Timeouts.Dial
}
