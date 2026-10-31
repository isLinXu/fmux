# fm wire compatibility

Everything here was captured from the real `fm` binary on macOS 27.2. Nothing is
inferred from documentation, because there is no documentation.

The samples live in `test/conformance/golden/` and are replayed by
`internal/compat/fm_test.go` and `test/conformance/conformance_test.go`.

## How to re-capture

```bash
make capture        # runs scripts/capture_fm.py
```

The script starts a recording server, drives seven `fm` invocations against it,
and writes each request body to `test/conformance/golden/<case>.json`. After a
macOS update, run it and `git diff` — the diff is the changelog.

## Observed behaviour

### 1. A system message with no `content` key

`fm respond --tool ocr 'hi'` produces:

```json
{
  "messages": [
    {"role": "system"},
    {"role": "user", "content": "hi"}
  ]
}
```

There is no `content` field at all — not `""`, not `null`. Go's `encoding/json`
happily leaves a zero value; strict validators on vLLM and SGLang reject the
whole request with 400.

**Handling**: `ir.Content` tracks `Present` separately from its value, and
`compat.Normalize` drops any system/developer message whose content is absent or
empty. Recorded as `missing-content-field` + `dropped-empty-system`.

### 2. `tools: []` on every request

Even a plain prompt carries `"tools": []`. It does not mean function calling is
enabled — it is a zero-value array that happened to be serialized.

**Handling**: an empty non-nil tools slice is set to nil before forwarding, so
backends that branch on `tools != null` do not take the tool path. Recorded as
`normalized-empty-tools`.

### 3. `stream: true` unconditionally

`--no-stream` is a client-side buffering flag. The request on the wire is always
`"stream": true`, with `"stream_options": {"include_usage": true}`.

**Handling**: the gateway treats fm as a streaming client and never assumes a
non-streaming path. The non-streaming branch exists for curl and SDKs.

### 4. No sampling controls

There is no `temperature`, `top_p`, `max_tokens`, or `seed` option, and
`fm respond -g` (greedy) fails outright:

```
Error: Invalid request: Greedy sampling is not supported
```

**Handling**: sampling is a gateway-side concern. Bind a `profile` to an alias;
the gateway fills in defaults for any field the client left nil.

### 5. Images arrive as JPEG data URLs

`--image foo.png` is transcoded to JPEG and embedded as
`data:image/jpeg;base64,...`. The gateway strips nothing — it forwards the data
URL intact and lets the backend decode it. The capability matrix decides whether
images are allowed at all.

### 6. Structured output is standard

`--schema` maps cleanly onto:

```json
"response_format": {
  "type": "json_schema",
  "json_schema": {"name": "Person", "strict": true, "schema": { ... }}
}
```

**Handling**: forwarded untouched when the backend declares `json_schema`
support. Otherwise the gateway rewrites it into a system-level constraint and
records a `degraded` log line — never silently.

## Detection

`compat.LooksLikeFm` checks the User-Agent first (`com.apple.FoundationModels`
is what actually arrives) and falls back to the structural fingerprint:
`stream=true` + non-nil empty `tools` + `stream_options.include_usage`.

Detection only affects logging today. The normalization is unconditional because
every fix it applies is also correct for ordinary clients.

## Deliberately not handled

- `fm count-tokens` — hard-bound to the on-device model, fails with
  `ModelManagerError 1026` even when a custom provider is configured.
- `fm serve`'s own proxy behaviour — it is a client of the gateway, not a peer.
- Private Cloud Compute availability checks — region-gated by a private
  entitlement; nothing a client can influence.
