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
| M7 | Admission control, stream idle, loopback guard | **done** |
| M8 | Drain on shutdown, busy-aware reaping, circuit breaker, latency histograms | **done** |

Verified on macOS 27.2 / M3 Max against a real `fm` binary and a real Ollama instance. See [Verification](#verification).

## Install

### Homebrew

```bash
brew install isLinXu/tap/fmux
brew services start fmux    # run at login, restart on crash
fmux config init            # -> ~/.fmux/config.yaml
fmux doctor                 # config, backends, aliases, port, fm wiring
```

The tap ships a prebuilt binary for the release tag, so no Go toolchain is
needed.

### From source

```bash
git clone https://github.com/isLinXu/fmux && cd fmux
make install                # -> ~/.local/bin/fmux
fmux config init            # -> ~/.fmux/config.yaml
fmux service install        # run at login, restart on crash
fmux doctor                 # config, backends, aliases, port, fm wiring
```

The full operational guide — plist layout, the `launchctl` EIO trap, upgrades,
removal — is in [docs/deployment.md](docs/deployment.md).

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
    max_concurrency: 1                # one GPU, one request at a time
    queue_depth: 4                    # then 429 + Retry-After

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

Idle is measured from the end of the last request, not its start. A request that outlives `idle_timeout` — a long generation on a small machine — holds an in-flight claim on the process, so the reaper skips it and the clock restarts when it finishes. Otherwise a model that is busy working would be killed for being idle.

### Memory budget

```yaml
budget_mb: 98304   # 96 GB on a 128 GB machine
```

Every request records its model's `memory_mb`. When a new model would exceed the budget, the least recently used one is unloaded first — for Ollama that is a `keep_alive: 0` call, for a supervised backend it is a process stop. The budget is advisory: an oversized model is still served, with a warning, because refusing to answer is worse than a brief spike.

Set the budget so it is reached **before** the swap file is. On a 128 GB Mac, 40 GB is a more useful ceiling than 96: a model that fits but leaves the machine swapping runs at a tenth of its speed, and nothing in the gateway can tell you that from the inside. `memory_mb` should come from `ollama ps` or a process's RSS, not from the parameter count — the same 27B weighs 17 GB or 27 GB depending on the context length its checkpoint declares, because the KV cache is sized from that number.

A model that is currently generating is **pinned** and never chosen as a victim. Without that, a request for a second model could unload the model another request is halfway through answering on — the eviction would look successful and the victim request would fail mid-stream.

### Admission control

A local model is not a web service. A 27B checkpoint on a Mac answers one request at a time: the second concurrent request does not halve the latency, it doubles the KV cache and pushes a shared-memory machine toward swap. A router that forwards everything immediately is therefore not neutral — it amplifies exactly the failure mode a local model has.

```yaml
- name: ollama
  max_concurrency: 1   # requests served at once
  queue_depth: 4       # requests allowed to wait; 0 rejects immediately
  queue_timeout: 3m    # how long a waiter may wait before 429
```

Admission runs **before** process start and memory eviction, so a request that is only going to queue never triggers an 18 GB load, and ten simultaneous cold requests cannot all try to start the same backend. A backend the gateway starts itself defaults to `max_concurrency: 1`; everything else defaults to unlimited.

Saturation is a `429` with `Retry-After` and `type: rate_limit_error` — the request was fine, the timing was not. If the alias has a fallback chain, a busy primary hands the request to the next target instead of failing it.

### Failure handling

A fallback chain only helps if the gateway remembers that a target just failed. Otherwise every request pays the dead backend's connect timeout first, forever, and the fallback only ever saves the request — not the latency.

```yaml
# built in, no configuration needed
# 3 consecutive backend failures -> skip that target for 30s
```

Only failures that mean "the backend is not there" count: refused connections, resets, `502`, `503`, `504`. A `400` means the backend is alive and disliked the request; counting it would take a healthy backend out of service for everyone. `429` means busy, not dead. After the cooldown one request is let through as a probe; if it succeeds the circuit closes, if it fails it reopens immediately. If every target is open the gateway tries anyway rather than refusing to answer.

State is visible per backend in `GET /admin/backends` (`circuit.open`, `circuit.consecutive_failures`, `circuit.trips`).

### Loopback only

The gateway binds to `127.0.0.1`, but that alone does not make it private: any web page can POST to loopback, and `text/plain` skips the CORS preflight. A `Host`/`Origin` check therefore runs before anything touches a backend, and DNS rebinding cannot turn a visited site into a client of the local model.

```yaml
allowed_hosts: ["127.0.0.1", "localhost", "::1"]   # extras only; loopback is implicit
```

Every response carries `X-Request-Id`, echoed from the client when present, so one log line maps to one complaint.

### Observability

```bash
fmux ps                     # live process state + resident models
curl -s localhost:1976/admin/processes | jq
curl -s localhost:1976/metrics          # Prometheus text
```

`fmux ps` reads a running gateway, because process state only exists inside the serving process:

```
BACKEND       STATE    PID    BUSY   RESTARTS  LAST USED  NOTE
llamacpp-8b   running  91386  1 req  0         00:11:57
RESIDENT MODEL   BACKEND      SIZE      IN USE
Qwen3-8B-Q4_K_M  llamacpp-8b  6144 MB   1 req
budget: 6144 / 98304 MB
```

`BUSY` is the process's in-flight request count and `IN USE` is the model's pin count. A process that is serving a request is never reaped for being idle, and a pinned model is never evicted to make room — the two columns are the answer to "why is this still running" and "why is the budget not shrinking".

`/metrics` carries two histograms rather than a single average, because on a local model the average hides the thing worth knowing:

| Metric | Why it is separate |
|---|---|
| `fmux_time_to_first_token_milliseconds` | A cold 20 GB load shows up here and nowhere else |
| `fmux_request_duration_milliseconds` | Total wall time, bucketed from 50 ms to 5 min |

A 30-second answer that produced its first token in 200 ms is a healthy model on a long generation. The same 30 seconds with a 25-second first token is a model that had to be loaded from disk. One number cannot tell those apart; two can.

## Running as a service

A gateway that only lives as long as a terminal is not a gateway. `fmux`
manages its own LaunchAgent:

```bash
fmux service install     # write the plist and hand it to launchd
fmux service status      # job state, pid, last exit code, /healthz
fmux service logs -f     # follow ~/Library/Logs/fmux.log
fmux service restart     # reload the config without cutting answers
fmux service uninstall
```

The plist runs `serve --config ~/.fmux/config.yaml`, restarts on crash (not on
clean exit), and pins `NO_PROXY` for loopback so a login-session proxy cannot
break the gateway's own calls to Ollama.

### Restarting without cutting answers

`SIGTERM` drains before it kills. The order is the whole point:

```
SIGTERM -> stop accepting -> wait for in-flight generations -> stop the backends -> exit
```

Killing the child processes first is the tempting version and the wrong one: they are what the in-flight requests are reading from, so every generation halfway done becomes a truncated stream with no error event, and a supervised backend that was killed mid-request has to be loaded from disk again on the retry.

Two details make the drain real rather than decorative:

- `timeouts.drain` (default `2m`) has to cover the longest legitimate answer, not the typical one. When it expires the connection is closed and the log says so at `WARN` — `drain deadline hit, cutting in-flight requests`.
- The plist sets `ExitTimeOut` to `drain + 15s`. launchd's default is 20 seconds, so without it launchd would SIGKILL the process in the middle of the drain it was told to perform.

`fmux service restart` sends `SIGTERM` and waits for the job to actually exit before starting it again, instead of `kickstart -k`, which would skip the drain entirely. A second `SIGTERM` (or Ctrl-C) skips the wait and exits immediately.

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
| `GET /metrics` | Prometheus text: requests, tokens, latency and TTFT histograms, evictions, inflight, queue waits, rejections |
| `GET /admin/backends` | Capability matrix, health, admission policy and circuit state per backend |
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
| 8 concurrent requests, `max_concurrency: 1`, `queue_depth: 4` | 5 served, 3 rejected `429` + `Retry-After: 5` |
| Cross-origin POST / foreign `Host` | `403 origin_not_allowed` / `403 host_not_allowed` |
| Backend that stalls mid-stream | `stream_idle` event, no `[DONE]`, slot released |
| Backend that never sends headers | `504 stream_idle` |
| Second model loaded while the first is generating | first model pinned, not evicted |
| `SIGTERM` with a stream in flight | request completes with `[DONE]`, child stopped after `http drained` |
| Drain deadline exceeded | connection closed, `WARN drain deadline hit, cutting in-flight requests` |
| Request outliving `idle_timeout` | reaper skips the busy process, clock restarts on release |
| Dead primary in a fallback chain, 8 requests | first 3 hit it, the rest go straight to the fallback |
| Backend answering `400` repeatedly | circuit stays closed, every request still attempted |

## Layout

```
cmd/fmux/          CLI: serve, config, service, backends, models, ps, doctor, version
internal/ir/       internal representation (polymorphic content, chunk, usage)
internal/compat/   fm quirk normalization
internal/router/   alias resolution + capability gate
internal/adapter/  adapter interface, OpenAI base, mock
internal/adapter/local/  OpenAI adapter + process/memory pre-flight
internal/supervisor/     child process lifecycle, health probe, idle reap
internal/budget/         unified memory accounting + LRU eviction + in-use pins
internal/gate/           per-backend admission control (semaphore + bounded queue)
internal/breaker/        consecutive-failure circuit per backend
internal/metrics/        Prometheus counters, histograms, inflight and rejection gauges
internal/launchd/        LaunchAgent plist + launchctl state
internal/api/      HTTP layer, SSE writer, loopback guard, admin + metrics endpoints
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
