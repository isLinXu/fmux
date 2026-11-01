package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/linxu/fmux/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Inspect and create the configuration file",
	}

	c.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(config.DefaultPath())
		},
	})

	c.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Write a starter config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := config.DefaultPath()
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists", path)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(ExampleYAML), 0o644); err != nil {
				return err
			}
			fmt.Println("wrote", path)
			return nil
		},
	})

	c.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(config.DefaultPath())
			if err != nil {
				return err
			}
			b, err := yaml.Marshal(cfg)
			if err != nil {
				return err
			}
			fmt.Print(string(b))
			return nil
		},
	})

	return c
}

// ExampleYAML is the starter config written by `fmux config init`.
const ExampleYAML = `# fmux gateway configuration
listen: 127.0.0.1:1976
log_level: info

# Alias used when a client asks for a model name that is not declared below.
# Leave empty to make unknown names a hard error.
default: ""

# Unified memory the resident models may occupy, in MB. When a new model would
# exceed it, the least recently used one is unloaded first. 0 disables eviction
# and only records usage. On a 128 GB Mac, ~98304 is a reasonable ceiling.
budget_mb: 0

# How often idle supervised processes are checked.
reap_interval: 30s

backends:
  # Ollama keeps its own model store; the gateway only talks to its API.
  - name: ollama
    type: ollama
    url: http://127.0.0.1:11434/v1
    models: ["qwen3.8:27b"]
    memory_mb: 18432
    # Admission control. Ollama batches requests itself, so the default is
    # unlimited; set a number when one model must not be asked twice at once.
    # max_concurrency: 1
    # queue_depth: 4
    # queue_timeout: 3m

  # A backend with a command is started on demand and stopped when idle, so
  # llama.cpp and MLX need no manual launch. One process serves one model.
  # - name: llamacpp-8b
  #   type: llamacpp
  #   url: http://127.0.0.1:8080/v1
  #   command: ["llama-server", "-m", "/models/Qwen3-8B-Q4_K_M.gguf",
  #             "--port", "8080", "-c", "32768"]
  #   memory_mb: 6144
  #   idle_timeout: 15m
  #   startup_timeout: 120s

  # - name: mlx
  #   type: mlx
  #   url: http://127.0.0.1:8081/v1
  #   command: ["mlx_lm.server", "--model", "mlx-community/Qwen3-8B-4bit",
  #             "--port", "8081"]
  #   memory_mb: 5120
  #   idle_timeout: 15m

  # LM Studio exposes an OpenAI-compatible server on 1234 and loads models on
  # demand, so it needs no command.
  # - name: lmstudio
  #   type: lmstudio
  #   url: http://127.0.0.1:1234/v1

  # Remote backends are HTTP-only: no process management.
  # - name: vllm-lab
  #   type: vllm
  #   url: http://10.0.0.20:8000/v1
  #   api_key: ${VLLM_KEY}

aliases:
  qwen3.8:27b:
    backend: ollama
    model: "qwen3.8:27b"
  qwen-fast:
    backend: ollama
    model: "qwen3.8:27b"
    profile: fast
  # auto tries the local model first, then the remote one.
  # auto:
  #   fallback: ["qwen3.8:27b", "qwen-remote"]

profiles:
  fast:
    temperature: 0.2
    max_tokens: 512
  quality:
    temperature: 0.7

timeouts:
  dial: 5s
  health: 2s
  non_stream: 10m
  stream_idle: 5m
`
