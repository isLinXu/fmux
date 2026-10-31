# Adapters

Every backend implements one interface:

```go
type Adapter interface {
	Name() string
	Type() string
	Caps() Capabilities
	Models(ctx context.Context) ([]ir.ModelInfo, error)
	Chat(ctx context.Context, req *ir.ChatRequest) (*ir.ChatResponse, error)
	ChatStream(ctx context.Context, req *ir.ChatRequest) (Stream, error)
	Health(ctx context.Context) error
}
```

## Mapping table

| Backend | Wire protocol | Endpoint | Vision | json_schema | tools | usage in stream | Process model |
|---|---|---|---|---|---|---|---|
| Ollama | OpenAI-compatible | `:11434/v1` | yes | yes | yes | yes | always-on daemon, `keep_alive` unload |
| llama.cpp | OpenAI-compatible | `:8080/v1` | with `--mmproj` | grammar-constrained | yes | no | one process per model, supervised |
| MLX-LM | OpenAI-compatible | `:8081/v1` | no | no | no | no | one process per model, slow cold start, supervised |
| LM Studio | OpenAI-compatible | `:1234/v1` | yes | yes | yes | yes | JIT model loading, external |
| vLLM | OpenAI-compatible | `:8000/v1` | yes | yes | yes | yes | remote, CUDA host |
| SGLang | OpenAI-compatible | `:30000/v1` | yes | yes | yes | yes | remote, CUDA host |
| mock | in-process | — | yes | yes | yes | yes | none |

The capability values are the ones that matter, not the marketing claims.
`build.CapabilitiesFor` is the single source of truth.

## Why vLLM and SGLang are remote-only

Both are CUDA-first. There is no official macOS ARM build, so `type: vllm`
implies `kind: remote`: no supervisor, no local process, just HTTP to another
machine. This is a feature — it lets a 32B model on a Linux box participate in
the same alias namespace as an 8B model on the laptop.

## What actually differs between backends

The HTTP layer is identical everywhere; only these diverge:

| Difference | Where it bites | Handled by |
|---|---|---|
| Model naming (`qwen3.8:27b` vs a GGUF path vs a HF repo id) | Alias resolution | `aliases.<name>.model` |
| Usage in stream support | Clients see no token counts | Gateway synthesizes a usage chunk |
| Native structured output | 400 on `response_format` | Prompt-rewrite degradation |
| Vision availability | 400 or garbage | Capability gate |
| Context window | Silent truncation | `max_context` check |
| Process lifetime | Model not loaded on first call | `internal/supervisor` |
| Resident memory | Two 18 GB models swap the machine | `internal/budget` (LRU unload) |

## The supervised adapter

`internal/adapter/local` wraps `openai.Adapter` and adds one method:

```go
func (a *Adapter) Prepare(ctx context.Context, model string) error
```

The HTTP layer calls it before every request. It starts the child process if
needed (`supervisor.Ensure`) and records the model's memory footprint
(`budget.Acquire`), which may unload a different model first. A backend with no
`command` skips the process step; a remote backend does not use this adapter at
all.

Two consequences worth knowing:

- **Cold start is inside the request.** The first call to a supervised backend
  blocks for the whole model load. That is deliberate — the alternative is
  returning 503 and making every client retry. `startup_timeout` bounds it.
- **Alias model names must match what the process serves.** llama.cpp and MLX
  ignore the model field but some builds validate it; point the alias at the
  exact name in `command`.

## Adding a backend

1. Add a `type` to `build.CapabilitiesFor` with its real capabilities.
2. If it speaks OpenAI, you are done — `type` alone is enough:

```yaml
- name: my-server
  type: openai
  url: http://127.0.0.1:9000/v1
```

3. If it does not speak OpenAI, implement `adapter.Adapter` in
   `internal/adapter/<name>/`, embedding nothing, and register it in
   `build.Registry`. The interface has six methods; a non-OpenAI backend is
   typically 200–300 lines.

The bar to clear before adding a backend: does it have a capability the existing
ones lack, or a wire difference that cannot be expressed as config? If neither,
use `type: openai`.

## Testing a new adapter

`test/conformance` shows the pattern: a fake backend that records what it
received, a real client driving the gateway, and assertions on both the response
and the recorded request. For a new adapter, swap the fake backend for the real
one and keep the assertions — they encode what the client needs, not what the
backend happens to do.
