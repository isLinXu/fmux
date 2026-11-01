# Architecture

## Layers

```
client      fm CLI / fm serve / curl / SDK / editor plugin
              │  OpenAI HTTP (SSE)
gateway     ┌──────────────────────────────────────────────┐
            │ api        routing, SSE writer, guard, metrics│
            │ compat     fm quirk normalization            │
            │ router     alias -> targets, capability gate │
            │ gate       per-backend admission control      │
            │ supervisor child process lifecycle           │
            │ budget     unified memory + LRU eviction     │
            │ adapter    per-backend translation           │
            └──────────────────────────────────────────────┘
              │  OpenAI HTTP
backends    ollama · llama.cpp · MLX · LM Studio · vLLM · SGLang
```

The gateway owns the abstraction. `fm` is a client — it cannot host the
abstraction layer itself, because it ships on the sealed system volume and
accepts only a base URL as configuration.

## Request lifecycle

```
POST /v1/chat/completions
  │
  ├─ guard                             (Host / Origin must be loopback)
  ├─ decode into ir.ChatRequest        (polymorphic content, absent fields)
  ├─ Validate                          (model + at least one message)
  ├─ compat.Normalize                  (drop empty system msgs, nil out tools:[])
  ├─ router.Resolve                    (alias -> ordered []Target)
  │
  └─ for each target in order:
       ├─ router.Prepare               (capability gate + profile defaults)
       │    ├─ images  & !vision   -> 400, stop
       │    ├─ tools   & !tools    -> 400, stop
       │    └─ schema  & !jsonschema -> rewrite to prompt, log degraded
       ├─ gate.Acquire                  (slot or bounded wait; 429 on overflow)
       ├─ budget.Pin                    (this model must not be evicted now)
       ├─ adapter.Preparer.Prepare     (start process, acquire memory)
       ├─ rewrite req.Model = target.Model
       ├─ stream ? ChatStream : Chat   (stream_idle bounds a silent backend)
       └─ success -> return; failure -> next target
```

The order of `gate` before `Prepare` is load-bearing. Admission first, because
starting an 18 GB process or evicting a resident model for a request that will
only queue is wasted work, and because N concurrent cold requests would
otherwise all try to start the same backend. Prepare second, because once a slot
is held the caller owns the backend and may safely block on a cold start.

## Process and memory model

This is the part that does not exist in a pure HTTP router, and the reason the
project is macOS-specific rather than another LiteLLM.

**Processes are demand-started.** A backend with a `command` is not started at
boot. The first request to one of its aliases calls `supervisor.Ensure`, which
spawns the process and polls its health endpoint until it answers or
`startup_timeout` expires. Later requests reuse the process. A background
watcher stops it once `idle_timeout` passes with no requests, which also releases
its budget entry through the supervisor's `onStop` hook.

**Memory is a single pool.** On Apple Silicon the GPU shares RAM with the CPU,
so "VRAM" is not a separate budget. `budget_mb` caps the sum of `memory_mb` over
resident models; acquiring a new one evicts the least recently used first —
`keep_alive: 0` for Ollama, a process stop for supervised backends. Eviction is
advisory: an oversized model is still admitted with a warning, because a wrong
estimate should not turn into a failed request.

**In-use models are pinned.** LRU alone is not safe: the least recently used
model may be the one currently generating on another backend, and unloading it
would kill a request that is halfway through an answer. Every request pins its
model for its lifetime, and eviction skips pinned residents. If every candidate
is pinned the load proceeds over budget and logs why — the same
advisory-not-authoritative trade-off as above, but now with an accurate reason.

**Cold starts are inside the request.** The alternative is a 503 plus
client-side retry logic, which every OpenAI client implements differently. The
cost is a slow first request and a `startup_timeout` to bound it.

**Three levels of process, one owner each.** The gateway itself is a launchd
job (`com.fmux.gateway`, managed by `fmux service`), because a gateway that
dies with its terminal is not a gateway. The inference servers it supervises
are its children, started and stopped by request pressure and `idle_timeout`.
And Ollama and LM Studio are owned by nobody here — they have their own
managers, so the gateway only talks to their HTTP API and lets them decide what
stays resident. The rule is: one owner per process, and the gateway never
fights another manager for the same server.

## Design decisions

**Alias first, model second.** Clients name aliases; backends receive their own
model identifiers. `Prepare` rewrites `req.Model` on the way out, and responses
are rewritten back to the alias on the way in. Without this the alias leaks into
the backend and you get `model 'qwen-fast' not found`.

**Fail fast, degrade visibly.** Vision and tool calls cannot be faked, so a
capability mismatch is a 400 before any HTTP call. JSON schema can be faked with
a prompt constraint, so it degrades — but every degradation produces a log line.
There is no silent path.

**The tail of a stream is not optional.** OpenAI clients expect a chunk carrying
`finish_reason`, then (if requested) a usage-only chunk with `choices: []`, then
`data: [DONE]`. Backends are inconsistent about all three, so the gateway
synthesizes whatever the backend omitted. When usage is missing entirely it is
estimated and flagged in the debug log — an approximation that is labelled as
one, not a fabrication.

**Adapter base class over per-backend forks.** `adapter/openai` implements the
protocol once. A new backend is a `Options` struct plus a capability row, not a
copy of the HTTP code. `adapter/mock` proves the interface is implementable
without any network at all.

**Single binary, no daemon dependency.** The gateway is one static Go binary,
runnable under `launchd` with no runtime. Adapters are compiled in; there is no
plugin loading, no Python environment, no version skew.

## Concurrency model

One goroutine per request. Streaming copies chunks from the backend response
straight to the client with a flush per event; nothing is buffered beyond a
single chunk. `http.Server.WriteTimeout` is deliberately unset because a slow
27B model can legitimately take minutes to finish.

**Concurrency is bounded per backend, not globally.** `gate` holds one
semaphore per backend with a bounded wait queue:

| Setting | Meaning |
|---|---|
| `max_concurrency: 1` | one request served at a time (the default for a backend the gateway starts) |
| `queue_depth: 4` | four requests may wait; the fifth gets `429` |
| `queue_timeout: 3m` | a waiter gives up with `429` after this long |

The bound belongs to the backend, not the gateway, because the resource being
protected is one GPU and one KV cache. A remote vLLM with continuous batching
genuinely serves dozens at once; a single llama.cpp process does not. A global
limit would have to be wrong for one of them.

**Two clocks bound a stream.** `stream_idle` (default 5m) is reset on every
chunk. When it fires the outbound request is cancelled, which unblocks the body
read — no goroutine is left watching a dead stream. If the first token had
already gone out the client gets an `error` event with `code: stream_idle` and
no `[DONE]`; if nothing had been written it gets a proper `504`. A silent
backend and a slow backend look identical on the socket, so the timeout is the
only thing that distinguishes them.

Client disconnect cancels `r.Context()`, which cancels the outbound request and
closes the backend stream — verified in the logs as
`Post .../chat/completions: context canceled`.

## Trust boundary

The gateway is loopback-only, and that is enforced rather than assumed. A page
in any browser can POST to `127.0.0.1`, and a `text/plain` body skips the CORS
preflight entirely; without a `Host` check, DNS rebinding makes every visited
site a client of the local model. `guard` compares the `Host` header and any
`Origin` against the loopback names (plus `allowed_hosts`) before routing, and
answers `403` otherwise. The cost is one map lookup per request.

Every request carries `X-Request-Id` — echoed when the client supplies one,
generated otherwise — and that id appears in every log line for the request, so
a client-side complaint maps to exactly one trace.

## What is deliberately absent

- No global request queue. The queue is per backend and bounded, because the
  contention being managed is one model on one GPU, not a fleet.
- No response cache. Sampling makes identical prompts diverge, and a cache that
  ignores `temperature` is a correctness bug.
- No model download. Ollama and llama.cpp already own that.
