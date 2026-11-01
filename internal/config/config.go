package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultListen is the gateway's default bind address. It is deliberately
// offset from `fm serve`'s default so both can run side by side.
const DefaultListen = "127.0.0.1:1976"

// Config is the whole gateway configuration.
type Config struct {
	Listen   string             `yaml:"listen"`
	LogLevel string             `yaml:"log_level"`
	Default  string             `yaml:"default"` // alias used when a request names an unknown model
	Backends []Backend          `yaml:"backends"`
	Aliases  map[string]Alias   `yaml:"aliases"`
	Profiles map[string]Profile `yaml:"profiles"`
	Timeouts Timeouts           `yaml:"timeouts"`

	// BudgetMB caps the unified memory the gateway will let resident models
	// occupy. 0 disables eviction and only records usage.
	BudgetMB int `yaml:"budget_mb"`
	// LogDir receives child process logs. Empty means ~/.fmux/logs.
	LogDir string `yaml:"log_dir"`
	// ReapInterval is how often idle supervised processes are checked.
	ReapInterval time.Duration `yaml:"reap_interval"`

	// AllowedHosts extends the Host-header allowlist. The loopback names and
	// the listen host are always allowed; add entries only when the gateway is
	// deliberately bound to something else.
	AllowedHosts []string `yaml:"allowed_hosts"`

	SourcePath string `yaml:"-"`
}

// Backend is one inference server the gateway can talk to.
type Backend struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // ollama | openai | llamacpp | mlx | lmstudio | vllm | sglang | mock
	URL  string `yaml:"url"`
	Kind string `yaml:"kind"` // local | remote; derived from type when empty

	APIKey  string            `yaml:"api_key"`
	Headers map[string]string `yaml:"headers"`

	// Local process management. A backend with a command is started on demand
	// and stopped when idle, so llama.cpp and MLX do not need a manual launch.
	Command        []string          `yaml:"command"`
	Env            map[string]string `yaml:"env"`
	HealthPath     string            `yaml:"health_path"`
	StartupTimeout time.Duration     `yaml:"startup_timeout"`
	StopTimeout    time.Duration     `yaml:"stop_timeout"`
	IdleTimeout    time.Duration     `yaml:"idle_timeout"`

	// MemoryMB is the estimated resident size of one model on this backend. It
	// feeds the budget and LRU eviction.
	MemoryMB int `yaml:"memory_mb"`

	// ModelList pins the models this backend serves, so /v1/models works
	// without a round trip.
	ModelList []string `yaml:"models"`

	// Admission control. A local model answers one request at a time; letting
	// ten requests in does not add throughput, it adds swap.
	//
	// MaxConcurrency 0 means "decide for me": 1 for a supervised backend (one
	// that has a command), unlimited for everything else. A negative value
	// means unlimited explicitly.
	MaxConcurrency int `yaml:"max_concurrency"`
	// QueueDepth is how many requests may wait for a slot before the gateway
	// answers 429. 0 means "never wait" — a saturated backend rejects at once.
	QueueDepth int `yaml:"queue_depth"`
	// QueueTimeout bounds how long a request may wait for a slot.
	QueueTimeout time.Duration `yaml:"queue_timeout"`
}

// Alias maps a client-facing model name onto a concrete backend/model pair.
type Alias struct {
	Backend  string   `yaml:"backend"`
	Model    string   `yaml:"model"`
	Profile  string   `yaml:"profile"`
	Fallback []string `yaml:"fallback"`
}

// Profile is a sampling preset applied when the client does not specify one.
type Profile struct {
	Temperature  *float64 `yaml:"temperature"`
	TopP         *float64 `yaml:"top_p"`
	TopK         *int     `yaml:"top_k"`
	MaxTokens    *int     `yaml:"max_tokens"`
	Stop         []string `yaml:"stop"`
	SystemPrefix string   `yaml:"system_prefix"`
}

// Timeouts bounds outbound calls.
type Timeouts struct {
	Dial       time.Duration `yaml:"dial"`
	Health     time.Duration `yaml:"health"`
	NonStream  time.Duration `yaml:"non_stream"`
	StreamIdle time.Duration `yaml:"stream_idle"`
}

// Default returns a config that works with a stock Ollama install.
func Default() *Config {
	return &Config{
		Listen:   DefaultListen,
		LogLevel: "info",
		Backends: []Backend{{
			Name: "ollama",
			Type: "ollama",
			URL:  "http://127.0.0.1:11434/v1",
		}},
		Aliases: map[string]Alias{},
		Profiles: map[string]Profile{
			"fast": {
				Temperature: ptr(0.2),
				MaxTokens:   ptr(512),
			},
			"quality": {
				Temperature: ptr(0.7),
			},
		},
		Timeouts: Timeouts{
			Dial:       5 * time.Second,
			Health:     2 * time.Second,
			NonStream:  10 * time.Minute,
			StreamIdle: 5 * time.Minute,
		},
	}
}

func ptr[T any](v T) *T { return &v }

// DefaultPath returns the standard config location.
func DefaultPath() string {
	if p := os.Getenv("FMUX_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".fmux", "config.yaml")
}

// Load reads and validates a config file. A missing file yields Default().
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c := Default()
			c.SourcePath = path
			return c, c.Validate()
		}
		return nil, err
	}
	c := Default()
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.SourcePath = path
	return c, c.Validate()
}

// Validate normalizes and checks the config.
func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	// A config built in code (tests, embedders) has no Default() behind it, and
	// a zero timeout would make every request fail instantly.
	if c.Timeouts.Dial <= 0 {
		c.Timeouts.Dial = 5 * time.Second
	}
	if c.Timeouts.Health <= 0 {
		c.Timeouts.Health = 2 * time.Second
	}
	if c.Timeouts.NonStream <= 0 {
		c.Timeouts.NonStream = 10 * time.Minute
	}
	if c.Timeouts.StreamIdle <= 0 {
		c.Timeouts.StreamIdle = 5 * time.Minute
	}
	seen := map[string]bool{}
	for i := range c.Backends {
		b := &c.Backends[i]
		if b.Name == "" {
			return fmt.Errorf("backends[%d]: name is required", i)
		}
		if seen[b.Name] {
			return fmt.Errorf("backends: duplicate name %q", b.Name)
		}
		seen[b.Name] = true
		if b.Type == "" {
			b.Type = "openai"
		}
		if b.Kind == "" {
			b.Kind = KindOf(b.Type)
		}
		if b.URL == "" && b.Type != "mock" {
			return fmt.Errorf("backend %q: url is required", b.Name)
		}
		b.URL = strings.TrimRight(b.URL, "/")
		if len(b.Command) > 0 {
			b.Kind = "local"
		}
		if b.StartupTimeout <= 0 {
			b.StartupTimeout = 90 * time.Second
		}
		if b.StopTimeout <= 0 {
			b.StopTimeout = 10 * time.Second
		}
		// A backend the gateway starts itself serves exactly one model from
		// one process, so a second concurrent request can only hurt. A backend
		// with its own manager (Ollama, LM Studio, a remote vLLM) already has
		// a concurrency policy, and overriding it silently would be rude.
		if b.MaxConcurrency == 0 && len(b.Command) > 0 {
			b.MaxConcurrency = 1
		}
		if b.MaxConcurrency > 0 && b.QueueDepth == 0 {
			b.QueueDepth = 8
		}
		if b.QueueTimeout <= 0 {
			b.QueueTimeout = 2 * time.Minute
		}
	}
	for name, a := range c.Aliases {
		if a.Backend == "" && len(a.Fallback) == 0 {
			return fmt.Errorf("alias %q: needs backend or fallback", name)
		}
		if a.Backend != "" && !seen[a.Backend] {
			return fmt.Errorf("alias %q: unknown backend %q", name, a.Backend)
		}
		for _, f := range a.Fallback {
			if _, ok := c.Aliases[f]; !ok && !strings.Contains(f, "@") {
				return fmt.Errorf("alias %q: fallback %q is not a known alias", name, f)
			}
		}
	}
	if c.Default != "" {
		if _, ok := c.Aliases[c.Default]; !ok && !strings.Contains(c.Default, "@") {
			return fmt.Errorf("default %q is not a known alias", c.Default)
		}
	}
	return nil
}

// BackendByName returns the backend with the given name.
func (c *Config) BackendByName(name string) (Backend, bool) {
	for _, b := range c.Backends {
		if b.Name == name {
			return b, true
		}
	}
	return Backend{}, false
}

// KindOf derives local/remote from the backend type.
func KindOf(typ string) string {
	switch typ {
	case "ollama", "llamacpp", "mlx", "lmstudio", "mock":
		return "local"
	default:
		return "remote"
	}
}

// ResolvedLogDir returns the directory that holds child process logs.
func (c *Config) ResolvedLogDir() string {
	if c.LogDir != "" {
		return c.LogDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".fmux", "logs")
}
