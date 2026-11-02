# Launch notes

Talking points for releasing fmux. This is not product documentation — it is
the material you copy from when posting somewhere. Every claim here is backed
by something in the repository; anything that is not is listed under
[What not to say](#what-not-to-say).

## One line

Pick by audience:

- **For people who already use `fm`:** 让 macOS 自带的 `fm` 能驱动任何本地模型。
- **For people who use Ollama:** One OpenAI-compatible endpoint in front of every local inference server on your Mac.
- **For people who build gateways:** A gateway that owns process lifecycle and memory residency, because on macOS a model is not just an endpoint.

The first one is the strongest hook, because nobody else has built it.

## Who feels the pain

| Audience | Their situation | Which angle lands |
|---|---|---|
| Apple Intelligence unavailable in their region | two of `fm`'s three backends are dead ends | "I can finally use `fm`" |
| Already running Ollama | has models, wants a better client | "one command to wire it up" |
| Running several models on one Mac | memory spirals, machine swaps | this is where the engineering story lives |

The first two are acquisition. The third is what convinces engineers. The
README serves both.

## Five claims that hold up

### 1. It is not another LiteLLM — it owns processes

llama.cpp and MLX serve one model per process, and an 18 GB checkpoint takes
tens of seconds to load. A pure HTTP router cannot help: it assumes every
backend is an always-on endpoint. fmux starts backends on demand, health-probes
them, reuses them, and stops them after `idle_timeout`.

> Demo: `fmux ps` shows `BUSY 1 req`, then after the idle window the process is
> gone and the budget is back to zero.

### 2. The memory budget is a real budget

On Apple Silicon the GPU shares RAM with the CPU, so there is no separate VRAM
to queue against — the only option is to unload. `budget_mb` is a hard ceiling
with LRU eviction, and **a model that is currently generating is pinned and
never chosen as a victim**.

This one resonates because almost everyone has hit the failure it prevents:
loading a second model kills the process that was mid-answer.

> Line to use: "Evicting the model another request is halfway through answering
> on looks successful and fails the victim request mid-stream."

### 3. The wire format was captured, not guessed

`fm` has no documentation. So the project records the real binary's request
bodies into `test/conformance/golden/` and replays them on every test run. The
quirks are specific: a `system` message with no `content` key, `"tools": []`
always present, `stream: true` even under `--no-stream`, images transcoded to
JPEG.

This is the strongest signal of engineering honesty — "there was no spec, so I
captured it" is a story, not a feature list.

### 4. Failure classification is deliberate

The circuit breaker only counts failures that mean "the backend is gone":
refused connections, resets, `502`, `503`, `504`. A `400` means the backend is
alive and disliked the request. A `429` means busy. A client cancel says
nothing about the backend. None of those count.

> Line to use: "A gateway that counts `400` toward its breaker takes a healthy
> backend out of service the first time you write a bad prompt."

### 5. The behaviour is verified, not asserted

26 rows of verification in the README, each in the form "what was done → what
happened". That is rare in a personal project and it pre-empts "you just ran
the demo once".

## Copy for each platform

### Hacker News (Show HN)

> **Show HN: fmux – point Apple's built-in fm CLI at any local model**
>
> `fm` ships with macOS and speaks a clean OpenAI dialect, but it can only
> reach three places: the on-device model, Private Cloud Compute, and one custom
> provider URL. On a machine where Apple Intelligence is unavailable, that
> leaves a bare URL with no aliasing, no capability routing and no fallback.
>
> fmux is a gateway for that URL. Ollama, llama.cpp, MLX, LM Studio, vLLM and
> SGLang behind one endpoint and one alias namespace.
>
> The part that turned out to matter on macOS: a local model is not just an
> HTTP endpoint. llama.cpp and MLX serve one model per process, cold loads take
> tens of seconds, and leaving a model resident after the last request is how a
> 128 GB machine starts swapping. So the gateway also owns process lifecycle
> and a unified memory budget with LRU eviction.
>
> `fm` has no documentation, so the request format was captured from the real
> binary and is replayed in the test suite.
>
> macOS only, single user, loopback only — it is designed as local
> infrastructure, not a shared service. MIT.
>
> Repo: https://github.com/isLinXu/fmux

Tone notes: lead with the constraint, not the features. State the macOS-only
limit before anyone asks. Do not oversell.

### V2EX

> **写了个网关，让 macOS 自带的 fm 能用上 Ollama / MLX**
>
> 背景：`fm` 是 macOS 自带的 CLI，但它只能配一个自定义 provider。我这台机器上装了 Ollama、llama.cpp 和 MLX，每次换模型都要改配置。
>
> 所以写了个网关：一条 OpenAI 兼容端点，背后是所有这些后端，按别名路由。
>
> 真正花时间的不是路由，是 Mac 上的内存问题：
> - llama.cpp 和 MLX 一个进程一个模型，18G 的模型冷启动几十秒
> - 请求结束后模型还驻留，128G 的机器就开始 swap
> - 两个模型同时生成，KV cache 直接翻倍
>
> 所以它管进程的生命周期（按需启、空闲回收），也管统一内存预算（超了就 LRU 卸载，正在生成的钉住不卸）。
>
> `fm` 没有文档，请求格式是抓包抓出来的，6 个格式怪癖都钉进测试里了。
>
> 只支持 macOS，单用户，只听 loopback。MIT。
>
> https://github.com/isLinXu/fmux

Tone notes: 中文技术社区吃「具体数字 + 踩坑」。避免「优雅」「强大」这类词。

### 即刻 / 微博

> Mac 上装了三个推理后端，但 `fm` 只能配一个 provider。
>
> 写了个网关把它们接到一个地址上。顺手解决了那个没人提的问题：跑第二个模型会把正在回答的进程干掉。
>
> 现在它会先等你这条答完，再卸载。

Tone notes: 短、有画面、以一个具体后果结尾。配 `fmux ps` 的截图。

### X / Twitter

> `fm` (macOS built-in) can only reach one custom provider.
>
> So I built a gateway: one OpenAI endpoint in front of Ollama, llama.cpp, MLX, LM Studio, vLLM and SGLang.
>
> It also owns process lifecycle and a memory budget — because on macOS, loading a second model kills the one that's mid-answer.
>
> [architecture diagram]

## What not to say

These will get you corrected in public, and the correction will be quoted more
than the original claim.

| Don't say | Why |
|---|---|
| "加速推理" / "faster inference" | fmux does not touch Metal, weights or kernels. The speed is MLX's and llama.cpp's. It only decides *when* a process runs. |
| "支持 Linux" / "cross-platform" | Local backend management is macOS-only. Linux can only reach remote HTTP backends, and that is not a shipped build. |
| "生产可用" / "production ready" | Single user, loopback only, no auth. That is a design choice, not a gap — but the wrong words invite the wrong criticism. |
| "支持 X 个模型" | It does not ship models and does not download them. It routes to whatever your backends already have. |
| Milestone lists (M0–M8) | That is a development log. Readers do not care, and it makes the project look unfinished. |

## Anticipated pushback

**"Why not just use LiteLLM / a generic proxy?"**
A pure HTTP router assumes always-on endpoints. On a Mac that is wrong in four
ways: one model per process, slow cold loads, swap pressure, and CPU/GPU
sharing one memory pool. Those four are the reason the process and budget
layers exist.

**"Why not modify `fm` itself?"**
It lives on the sealed system volume and its eligibility check is backed by a
private entitlement. Not possible, and not a fight worth having.

**"Is this just a wrapper?"**
The wire-format work is the tell: six `fm` quirks were captured from the binary
and are replayed in CI. A wrapper does not need golden tests.

**"macOS only is a limitation."**
It is the premise. The problems fmux solves — one process per model, unified
memory, launchd — do not exist in the same shape anywhere else.

## Assets

| Asset | Where | Use |
|---|---|---|
| Architecture diagram | `docs/assets/fmux-arch-light.svg` (+ dark) | top of every post |
| `fmux ps` output | run it with a model loaded | shows `BUSY` / `IN USE`, the two columns that explain "why is this still running" |
| `fm chat` recording | 30s: cold start → tokens → idle reap | the demo that proves the process management is real |
| Verification table | README | answers "did you actually test this" |

## If you only get one paragraph

> My Mac has Ollama, llama.cpp and MLX installed, but `fm` only accepts a single
> custom provider. So I wrote a gateway: one OpenAI-compatible endpoint in front
> of all of them, with alias routing, on-demand process start, and a unified
> memory budget. It remembers which backend just died, and it finishes the
> answer that is in flight before shutting anything down.
>
> I did not touch `fm` — it has no documentation, so I captured its real request
> bodies and pinned six format quirks in golden tests.
>
> macOS only, because "one process per model on shared memory" is a Mac problem.
