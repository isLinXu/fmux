package build

import (
	"testing"

	"github.com/linxu/fmux/internal/config"
)

func TestHealthURL(t *testing.T) {
	cases := []struct {
		name string
		b    config.Backend
		want string
	}{
		{
			name: "llamacpp defaults to /health",
			b:    config.Backend{Type: "llamacpp", URL: "http://127.0.0.1:8080/v1"},
			want: "http://127.0.0.1:8080/health",
		},
		{
			name: "openai-compatible defaults to /v1/models",
			b:    config.Backend{Type: "mlx", URL: "http://127.0.0.1:8081/v1"},
			want: "http://127.0.0.1:8081/v1/models",
		},
		{
			name: "explicit relative path",
			b:    config.Backend{Type: "llamacpp", URL: "http://127.0.0.1:8080/v1", HealthPath: "/healthz"},
			want: "http://127.0.0.1:8080/healthz",
		},
		{
			name: "absolute URL wins",
			b:    config.Backend{Type: "llamacpp", URL: "http://127.0.0.1:8080/v1", HealthPath: "http://127.0.0.1:9999/ping"},
			want: "http://127.0.0.1:9999/ping",
		},
		{
			name: "url without /v1 suffix",
			b:    config.Backend{Type: "openai", URL: "http://10.0.0.5:8000"},
			want: "http://10.0.0.5:8000/v1/models",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HealthURL(c.b); got != c.want {
				t.Fatalf("HealthURL = %q, want %q", got, c.want)
			}
		})
	}
}

// TestBuildRegistersSupervisorOnlyForCommands keeps the contract honest:
// an externally managed backend must not gain a process spec.
func TestBuildRegistersSupervisorOnlyForCommands(t *testing.T) {
	cfg := &config.Config{
		Listen: "127.0.0.1:0",
		Backends: []config.Backend{
			{Name: "ollama", Type: "ollama", URL: "http://127.0.0.1:11434/v1"},
			{Name: "local", Type: "llamacpp", URL: "http://127.0.0.1:8080/v1",
				Command: []string{"llama-server", "--port", "8080"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	b, err := Build(cfg, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	names := b.Supervisor.Names()
	if len(names) != 1 || names[0] != "local" {
		t.Fatalf("supervised = %v, want [local]", names)
	}
	if b.Registry == nil || len(b.Registry.Names()) != 2 {
		t.Fatalf("registry = %v", b.Registry.Names())
	}
}
