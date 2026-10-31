# Architecture

## Layers

```
client      fm CLI / fm serve / curl / SDK / editor plugin
              │  OpenAI HTTP (SSE)
gateway     ┌──────────────────────────────────────────────┐
            │ api      routing, SSE writer, error shape    │
            │ compat   fm quirk normalization              │
            │ router   alias -> targets, capability gate   │
            │ adapter  per-backend translation             │
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
       ├─ rewrite req.Model = target.Model
       ├─ stream ? ChatStream : Chat
       └─ success -> return; failure -> next target
```

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

Client disconnect cancels `r.Context()`, which cancels the outbound request and
closes the backend stream — verified in the logs as
`Post .../chat/completions: context canceled`.

## What is deliberately absent

- No request queue. Local backends serialize internally; queuing twice adds
  latency without adding throughput.
- No response cache. Sampling makes identical prompts diverge, and a cache that
  ignores `temperature` is a correctness bug.
- No model download. Ollama and llama.cpp already own that.
