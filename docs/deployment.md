# Deployment

Everything here assumes macOS. The gateway binds to loopback and serves one
user; there is no service account, no TLS and no auth to configure.

## 1. Install

**From source** (works today, no release needed):

```bash
git clone https://github.com/linxu/fmux && cd fmux
make install          # -> ~/.local/bin/fmux
```

`make install` builds with the version stamped from `git describe` and copies
the binary to `~/.local/bin`. Make sure that directory is on `PATH`:

```bash
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc
```

**Homebrew** (after the tap exists):

```bash
brew install linxu/tap/fmux
# or, before the first tagged release:
brew install --HEAD ./Formula/fmux.rb
```

**Just the binary**: download the `darwin_arm64` tarball from the releases
page, drop `fmux` anywhere on `PATH`. Nothing else is required — the config
file is optional and the gateway runs on defaults without one.

## 2. Configure

```bash
fmux config init      # writes ~/.fmux/config.yaml, refuses to overwrite
fmux config show      # the effective config, after defaults are applied
```

The minimum viable file is a backend plus an alias:

```yaml
backends:
  - name: ollama
    type: ollama
    url: http://127.0.0.1:11434/v1
    models: ["qwen3.8:27b"]
    memory_mb: 20480

aliases:
  qwen3.8:27b: { backend: ollama, model: "qwen3.8:27b" }
```

`memory_mb` only matters once `budget_mb` is set; without a budget it is
recorded but never enforced.

## 3. Keep it running

A background shell dies with its parent, so the gateway is meant to run under
launchd. Two equivalent routes:

```bash
# route A: the built-in LaunchAgent
fmux service install     # writes ~/Library/LaunchAgents/com.fmux.gateway.plist

# route B: brew services (needs the Homebrew install)
brew services start fmux
```

Route A writes:

| Field | Value |
|---|---|
| Label | `com.fmux.gateway` |
| Program | the binary you ran `service install` from |
| Arguments | `serve --config ~/.fmux/config.yaml` |
| KeepAlive | `SuccessfulExit: false` — restarts on crash, not on clean exit |
| ExitTimeOut | `timeouts.drain + 15s` (135 by default) |
| Log | `~/Library/Logs/fmux.log` |
| Env | `NO_PROXY=127.0.0.1,localhost` |

`ExitTimeOut` is not decoration. launchd waits **20 seconds** after `SIGTERM`
before `SIGKILL`, and the gateway spends that window draining in-flight
generations. Without the key, launchd would kill the process in the middle of
the drain it was asked to perform, and the answer would be truncated anyway.

Then:

```bash
fmux service status      # job state, pid, last exit code, /healthz
fmux service logs -f     # follow the log
fmux service restart     # drains, then reloads the config
fmux service uninstall   # bootout + delete the plist
```

`restart` sends `SIGTERM` and waits for the job to leave the domain before
starting it again — deliberately not `kickstart -k`, which kills the process
outright and would make the drain pointless. If a long generation is still
running after `drain + 15s`, the command says so and you can retry; a second
`SIGTERM` skips the wait.

### The `launchctl` EIO trap

`launchctl bootstrap` answers **`Bootstrap failed: 5: Input/output error`** in
three unrelated situations, and the message never says which one:

1. the job is already loaded — this is the common case, and it is not an error;
2. the plist is malformed — `plutil -lint` catches it;
3. the calling process is not allowed to spawn jobs (a sandboxed shell, an
   agent runner, some terminal multiplexers).

`fmux service install` distinguishes them: after a failed bootstrap it asks
launchd for the job's real state, and only reports failure if the job is
genuinely absent. When it is absent *and* the plist is valid, the plist is
already on disk and launchd loads it at next login. To start it right away,
run the one line the command prints, from Terminal.app:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.fmux.gateway.plist
```

## 4. Point `fm` at the gateway

```bash
fm config add model-provider fmux http://127.0.0.1:1976/v1
fm config set default-model-provider fmux
fm config set default-model qwen3.8:27b
```

Verify:

```bash
fm models       # every alias shows under the fmux provider
fm respond "用一句话说明什么是统一内存架构"
fmux doctor     # config, backends, aliases, port, fm provider — one screen
```

`fmux doctor` is the fastest way to find the broken link. Its `fm-provider`
line reads `~/.fm/config.json` and says whether the default provider actually
points at this gateway's port.

## 5. Troubleshooting

**`curl` to the gateway fails, but `fm` works.** A proxy is set in the
environment and `curl` honours it for loopback. Go's HTTP client — and
therefore `fmux` and `fm` — bypasses proxies for `127.0.0.1` automatically.
Use `curl --noproxy '*'` when testing by hand, or export
`NO_PROXY=127.0.0.1,localhost`. The LaunchAgent sets it for the gateway.

**`listen 127.0.0.1:1976: address already in use`.** Something else owns the
port; `lsof -nP -iTCP:1976 -sTCP:LISTEN` names it. `fmux serve --listen
127.0.0.1:1977` moves it, but then `fm`'s provider URL has to move too.

**`404 model_not_found`.** The model is not an alias and `default` is empty.
That is intentional: a typo should fail loudly instead of silently landing on
whatever model happens to be loaded.

**`400 unsupported_capability`.** The request needs vision, tools or a JSON
schema and the backend's capability matrix says no. `fmux backends` prints the
matrix per backend.

**`fm models` shows the provider but no models.** `/v1/models` failed upstream.
Check `fmux backends` for `HEALTH=down`, then the backend's own port.

**Model reloads on every request.** Ollama's own keep-alive is doing that, not
the gateway. `OLLAMA_KEEP_ALIVE=30m` on the Ollama service is the fix; the
gateway's `idle_timeout` only applies to backends it supervises itself.

**`429 backend_busy`.** Admission control did its job: the backend is at
`max_concurrency` and `queue_depth` requests are already waiting. The response
carries `Retry-After`. If this is a common outcome the limit is wrong for the
workload — raise `queue_depth`, or accept that one GPU serves one request.
`curl -s localhost:1976/metrics | grep fmux_rejected` shows whether it is
happening.

**`403 host_not_allowed` / `403 origin_not_allowed`.** The request came from
something that is not a local client — a browser page, a DNS-rebound hostname,
or a proxy that rewrote `Host`. Add the name to `allowed_hosts` only if the
gateway is deliberately bound somewhere else.

**`504 stream_idle` or a truncated stream ending in an error event.** The
backend accepted the request and then stopped producing tokens for
`timeouts.stream_idle`. A local model that is swapping looks exactly like this.
Check memory pressure first; raise the timeout only if the model is genuinely
slow.

**Config edits do nothing.** `serve` reads the file once at startup. Use
`fmux service restart` (or `brew services restart fmux`).

**A backend stopped being tried.** `curl -s localhost:1976/admin/backends`
shows `circuit.open: true` with `consecutive_failures` and `last_error`. Three
consecutive connection failures or `502/503/504` open it for 30 seconds. It
closes by itself after one successful probe; if it stays open, the backend
really is down. Restart the backend rather than the gateway.

**A restart seems to hang.** It is draining. `fmux service status` shows the
pid still alive and `curl -s localhost:1976/healthz | jq .inflight` counts the
requests it is waiting for. After `timeouts.drain` the gateway cuts them and
logs `drain deadline hit, cutting in-flight requests`. If you would rather not
wait, send `SIGTERM` again.

**The log says `drain deadline hit` under launchd but the request had barely
started.** The plist predates the `ExitTimeOut` key — rerun
`fmux service install`, then `launchctl bootout gui/$(id -u)/com.fmux.gateway`
and bootstrap it again.

## 6. Upgrades and removal

```bash
# source install
git pull && make install && fmux service restart

# homebrew
brew upgrade fmux && brew services restart fmux

# removal
fmux service uninstall        # or: brew services stop fmux
rm -rf ~/.fmux                # config and logs; the model stores are untouched
fm config remove model-provider fmux
```

Removing the gateway never touches Ollama's model store, llama.cpp's GGUF
files or the model caches — the gateway owns no weights.
