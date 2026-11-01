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
| M2 | llama.cpp + process supervisor | **done** |
| M3 | MLX / LM Studio, memory budget, LRU unload | **done** |
| M4 | vLLM / SGLang remote, fallback chains | **done** |
| M5 | Metrics, structured logs, bench | **done** |
| M6 | Homebrew formula, LaunchAgent, release pipeline, docs | **done** |

Verified on macOS 27.2 / M3 Max against a real `fm` binary and a real Ollama instance. See [Verification](#verification).

## Install

```bash
git clone https://github.com/linxu/fmux && cd fmux
make install                # -> ~/.local/bin/fmux
fmux config init            # -> ~/.fmux/config.yaml
fmux service install        # run at login, restart on crash
fmux doctor                 # config, backends, aliases, port, fm wiring
```

`brew install linxu/tap/fmux` once a release is tagged. The full operational
guide — plist layout, the `launchctl` EIO trap, upgrades, removal — is in
[docs/deployment.md](docs/deployment.md).

## Wire up `fm`

`fm` keeps its providers in `~/.fm/config.json`. Point the default one at the
gateway and every alias becomes a model:

```bash
fm config add model-provider fmux http://127.0.0.1:1976/v1
fm config set default-model-provider fmux
fm config set default-model qwen3.8:27b

fm models     # ✓ qwen3.8:27b
fm chat
```

No backend handy? `fmux serve --mock` starts a deterministic fake backend, so
the whole path — `fm` → gateway → SSE → terminal — can be exercised without a
single weight on disk.

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

`type` decides the capability matrix and whether the backend is treated as local (process-managed, budget-accounted) or remote (HTTP only).

## Process supervision

A local model on macOS is not just an endpoint. llama.cpp and MLX serve exactly one model per process, loading an 18 GB checkpoint takes tens of seconds, and leaving it resident after the last request is how a 128 GB machine starts swapping. None of that exists in a pure HTTP router, so the gateway manages it:

```yaml
- name: llamacpp-8b
  type: llamacpp
  url: http://127.0.0.1:8080/v1
  command: ["llama-server", "-m", "/models/Qwen3-8B-Q4_K_M.gguf", "--port", "8080"]
  memory_mb: 6144        # feeds the budget
  idle_timeout: 15m      # unload after this long with no requests
  startup_timeout: 120s  # wait this long for /health before giving up
```

The first request to that alias starts the process and waits for its health probe; later requests reuse it; after `idle_timeout` it is stopped and its memory released. A backend with no `command` (Ollama, LM Studio) is left to its own manager.

### Memory budget

```yaml
budget_mb: 98304   # 96 GB on a 128 GB machine
```

Every request records its model's `memory_mb`. When a new model would exceed the budget, the least recently used one is unloaded first — for Ollama that is a `keep_alive: 0` call, for a supervised backend it is a process stop. The budget is advisory: an oversized model is still served, with a warning, because refusing to answer is worse than a brief spike.

### Observability

```bash
fmux ps                     # live process state + resident models
curl -s localhost:1976/admin/processes | jq
curl -s localhost:1976/metrics          # Prometheus text
```

`fmux ps` reads a running gateway, because process state only exists inside the serving process:

```
BACKEND       STATE    PID    RESTARTS  LAST USED  NOTE
llamacpp-8b   running  91386  0         00:11:57
RESIDENT MODEL   BACKEND      SIZE
Qwen3-8B-Q4_K_M  llamacpp-8b  6144 MB
budget: 6144 / 98304 MB
```

## Running as a service

A gateway that only lives as long as a terminal is not a gateway. `fmux`
manages its own LaunchAgent:

```bash
fmux service install     # write the plist and hand it to launchd
fmux service status      # job state, pid, last exit code, /healthz
fmux service logs -f     # follow ~/Library/Logs/fmux.log
fmux service restart     # reload the config
fmux service uninstall
```

The plist runs `serve --config ~/.fmux/config.yaml`, restarts on crash (not on
clean exit), and pins `NO_PROXY` for loopback so a login-session proxy cannot
break the gateway's own calls to Ollama.

`launchctl bootstrap` reports `Input/output error` for a job that is already
loaded, for a malformed plist, and for a shell that is not allowed to spawn
jobs — identically, with no hint which. `service install` checks the job's real
state afterwards instead of trusting the exit code, and prints the one-line
command to run by hand when the plist is valid but the caller is blocked.
Details in [docs/deployment.md](docs/deployment.md#the-launchctl-eio-trap).

## Endpoints

| Path | Purpose |
|---|---|
| `POST /v1/chat/completions` | The only endpoint a client needs |
| `GET /v1/models` | Aliases, plus backend-reported models |
| `GET /healthz` | Liveness |
| `GET /metrics` | Prometheus text: requests, tokens, duration, evictions |
| `GET /admin/backends` | Capability matrix and health per backend |
| `GET /admin/processes` | Supervised processes and memory residency |
| `GET /admin/config` | Effective configuration |

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
| `fm respond --model qwen-json --schema` → Ollama | `{"age": 30, "name": "张三"}`, profile applied |
| Memory budget accounting | `fmux ps` reports `20480 / 98304 MB` resident |
| `fmux service install` when launchd refuses the job | plist written, real state checked, manual command printed |
| Unknown model | `404 model_not_found` |
| Vision request to a non-vision backend | `400 unsupported_capability` |
| Supervised backend (fake llama.cpp) cold start | process started, request served in 0.30 s |
| Same backend, second request | process reused, same PID |
| Idle reap | process stopped, port released, budget back to 0 MB |

## Layout

```
cmd/fmux/          CLI: serve, config, service, backends, models, ps, doctor, version
internal/ir/       internal representation (polymorphic content, chunk, usage)
internal/compat/   fm quirk normalization
internal/router/   alias resolution + capability gate
internal/adapter/  adapter interface, OpenAI base, mock
internal/adapter/local/  OpenAI adapter + process/memory pre-flight
internal/supervisor/     child process lifecycle, health probe, idle reap
internal/budget/         unified memory accounting + LRU eviction
internal/metrics/        Prometheus counters
internal/launchd/        LaunchAgent plist + launchctl state
internal/api/      HTTP layer, SSE writer, admin + metrics endpoints
internal/config/   YAML config
internal/build/    config -> adapters, supervisor, budget
test/conformance/  golden captures + real-fm end-to-end tests
test/fixtures/     fake OpenAI server used by the supervision tests
scripts/           capture_fm.py, install-launchagent.sh, bench.sh
docs/              fm-compat, architecture, adapters, deployment
Formula/fmux.rb    Homebrew formula (also generated by goreleaser for the tap)
.goreleaser.yaml   darwin arm64/amd64 archives + tap update on tag
```

## Development

```bash
make test        # unit + conformance + e2e (skips fm/python tests if absent)
make conformance # only the real-fm tests
make e2e         # only the process-supervision tests
make capture     # re-record golden samples from the installed fm binary
make release-check  # goreleaser check + snapshot build
```

`make capture` is the maintenance lever: after a macOS update, run it and diff. If the wire format moved, the diff says exactly how.

## Non-goals

- Modifying or extending `fm` itself — it lives on the sealed system volume and its eligibility check is backed by a private entitlement.
- Downloading or quantizing models — that is Ollama's / llama.cpp's / HF's job.
- Multi-tenant auth — this binds to loopback for a single user.
- Managing local backends on Linux/Windows — remote HTTP only there.
- Running vLLM or SGLang on macOS — both are CUDA-first with no official Apple Silicon support, so they are remote-only here.

## License

MIT
