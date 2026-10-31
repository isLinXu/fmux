// Package build wires configuration into concrete adapters.
package build

import (
	"fmt"
	"net/http"
	"time"

	"github.com/linxu/fmux/internal/adapter"
	"github.com/linxu/fmux/internal/adapter/mock"
	"github.com/linxu/fmux/internal/adapter/openai"
	"github.com/linxu/fmux/internal/config"
)

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
		return adapter.Capabilities{
			Stream: true, Vision: false, JSONSchema: true, Tools: true,
			UsageInStream: false, MaxContext: 32768,
		}
	case "mlx":
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

// Registry constructs every adapter described by the config.
func Registry(cfg *config.Config) (*adapter.Registry, error) {
	reg := adapter.NewRegistry()
	client := &http.Client{Timeout: 0}

	for _, b := range cfg.Backends {
		switch b.Type {
		case "mock":
			reg.Add(mock.New(b.Name))
			continue
		}
		if b.URL == "" {
			return nil, fmt.Errorf("backend %q: url is required", b.Name)
		}
		reg.Add(openai.New(openai.Options{
			Name:    b.Name,
			Type:    b.Type,
			BaseURL: b.URL,
			APIKey:  b.APIKey,
			Headers: b.Headers,
			Caps:    CapabilitiesFor(b.Type),
			Client:  client,
		}))
	}
	if len(reg.Names()) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	return reg, nil
}

// DialTimeout returns the dial timeout, with a sane floor.
func DialTimeout(cfg *config.Config) time.Duration {
	if cfg.Timeouts.Dial <= 0 {
		return 5 * time.Second
	}
	return cfg.Timeouts.Dial
}
