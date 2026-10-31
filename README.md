# fmux

One OpenAI-compatible endpoint in front of every local inference server on macOS, so `fm` — Apple's Foundation Models CLI — can drive all of them.

```
fm CLI  ──┐
curl    ──┼──▶  fmux :1976/v1  ──▶  ollama / llama.cpp / MLX / LM Studio / vLLM / SGLang
SDKs    ──┘
```

## Why this exists

`fm` ships with macOS and speaks a clean OpenAI-compatible dialect, but it can only reach three places: the on-device Apple model, Private Cloud Compute, and a single custom provider. On a machine where Apple Intelligence is unavailable (region-gated, no model assets), the first two are dead ends, and the third is a bare URL — no aliasing, no capability routing, no fallback.

`fmux` turns that single URL into a proper gateway. `fm` stays the client; the gateway becomes the abstraction layer.

A second reason, and the one that shapes the code: `fm`'s wire format has quirks that strict backends reject. Every one of them is captured in `test/conformance/golden/` from the real binary and covered by tests.

## Status

| Milestone | Scope | State |
|---|---|---|
| M0 | Wire fidelity, HTTP layer, SSE, golden tests | **done** |
| M1 | Ollama adapter, alias routing, capability matrix | **done** |
| M2 | llama.cpp + process supervisor | not started |
| M3 | MLX / LM Studio, memory budget, LRU unload | not started |
| M4 | vLLM / SGLang remote, fallback chains | partial (adapter works, no fallback tests) |
| M5 | Metrics, structured logs, bench | not started |
| M6 | Homebrew, LaunchAgent, docs | partial (LaunchAgent script included) |

Verified on macOS 27.2 / M3 Max against a real `fm` binary and a real Ollama instance. See [Verification](#verification).

## Quick start

```bash
go build -o bin/fmux ./cmd/fmux

# 1. write a config
./bin/fmux config init        # -> ~/.fmux/config.yaml

# 2. start the gateway
./bin/fmux serve

# 3. point fm at it
fm config add model-provider fmux http://127.0.0.1:1976/v1
fm config set default-model-provider fmux
fm config set default-model qwen3.8:27b

fm models     # ✓ qwen3.8:27b
fm chat
```

No config file? `fmux serve --mock` starts a deterministic fake backend so you can exercise the whole path without any model.

## Configuration

```yaml
listen: 127.0.0.1:1976

backends:
  - name: ollama
    type: ollama                      # ollama|llamacpp|mlx|lmstudio|vllm|sglang|openai|mock
    url: http://127.0.0.1:11434/v1
    models: ["qwen3.8:27b"]

  - name: vllm-lab
    type: vllm
    url: http://10.0.0.20:8000/v1
    api_key: ${VLLM_KEY}

aliases:
  qwen3.8:27b: { backend: ollama, model: "qwen3.8:27b" }
  qwen-fast:   { backend: ollama, model: "qwen3.8:27b", profile: fast }
  auto:        { fallback: ["qwen3.8:27b", "qwen-remote"] }

profiles:
  fast: { temperature: 0.2, max_tokens: 512 }
```

`type` decides the capability matrix and whether the backend is treated as local (process-managed, later) or remote (HTTP only).

## The `fm` quirks this handles

Captured from the real binary, replayed in every test run:

| Quirk | Consequence if unhandled |
|---|---|
| `{"role":"system"}` with **no `content` key** (`fm --tool ocr`) | vLLM / SGLang answer 400 |
| `"tools": []` always present | Backends that gate on tools think function calling is on |
| `stream: true` even under `--no-stream` | Non-streaming code paths never execute |
| `stream_options.include_usage` always set | Backends that ignore it emit no usage at all |
| Images transcoded to JPEG data URLs | Vision backends expecting PNG reject them |
| No sampling controls at all | `temperature` must come from a gateway-side profile |

## Verification

Measured on the machine above, real `fm` binary, real Ollama:

| Path | Result |
|---|---|
| `fm respond` → gateway → mock | pass, 0.07 s |
| `fm respond --tool ocr` → gateway | pass, malformed system message dropped before forwarding |
| `fm respond --schema` → gateway → Ollama | `{"name": "张三", "age": 30}` |
| `fm respond` (streaming) → gateway → Ollama | tokens arrive incrementally |
| `fm chat` → gateway → Ollama | `turn 1 · ↑65 · ↓33` |
| `fm models` → gateway | lists both aliases |
| Alias routing (`qwen-fast` → `qwen3.8:27b`) | pass |
| Unknown model | `404 model_not_found` |
| Vision request to a non-vision backend | `400 unsupported_capability` |

## Layout

```
cmd/fmux/          CLI: serve, config, version
internal/ir/       internal representation (polymorphic content, chunk, usage)
internal/compat/   fm quirk normalization
internal/router/   alias resolution + capability gate
internal/adapter/  adapter interface, OpenAI base, mock
internal/api/      HTTP layer, SSE writer
internal/config/   YAML config
internal/build/    config -> adapters
test/conformance/  golden captures + real-fm end-to-end tests
scripts/           capture_fm.py, install-launchagent.sh, bench.sh
```

## Development

```bash
make test        # unit + conformance (skips fm tests if fm is absent)
make conformance # only the real-fm tests
make capture     # re-record golden samples from the installed fm binary
```

`make capture` is the maintenance lever: after a macOS update, run it and diff. If the wire format moved, the diff says exactly how.

## Non-goals

- Modifying or extending `fm` itself — it lives on the sealed system volume and its eligibility check is backed by a private entitlement.
- Downloading or quantizing models — that is Ollama's / llama.cpp's / HF's job.
- Multi-tenant auth — this binds to loopback for a single user.
- Managing local backends on Linux/Windows — remote HTTP only there.

## License

MIT
