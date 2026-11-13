# fm 补充验证报告

日期：2026-11-04。针对上一轮未完成的 Swift 请求和正常路径以外的协议行为，
新增 59 项断言：**49 通过、10 未通过**。
组成是 55 项故障/资源/会话断言、2 项模型菜单断言、2 项尾帧元数据断言。
10 个失败断言是以下问题的不同表现，不是 10 个独立缺陷。
另执行了 3 次真实请求逐帧追踪，以及全量 `go test ./... -race -count=1`，
现有 Go 测试全部通过。

## 新发现

### P1：上游 SSE 错误丢失，阻止 fallback

可控上游发送以下有效错误信封，分别测试首内容之前/之后，
以及有无 `event: error` 标签：

```text
data: {"error":{"message":"injected upstream failure","type":"server_error","code":"injected_error"}}
```

**fmux 的实际输出是 HTTP 200、空 choices chunk、补发 stop/usage 与 `[DONE]`，
原始 message/code 消失。** 对已输出内容的流，HTTP 200 本身可以保留，
问题是错误被丢弃并发出成功终止标记；首内容之前还失去了正常错误响应机会。

配置了可用备用后端的对照结果：

- 上游 HTTP 503：fallback 返回 `alpha`，通过。
- 上游 SSE 错误：备用后端没有恢复请求，fm 返回通用解析错误，未通过。

注意：本轮 fm CLI 在这些异常输入下仍非零退出，
不能声称它静默把部分回答当作成功。问题位于网关错误表达和路由决策：
客户端没有收到原始上游错误，网关输出却带有成功尾帧。
通用客户端是否因此误报成功，本轮没有逐一测试。

证据：`wire.error_before/after`、`wire.event_error_before/after`、
`fallback.sse_error_should_recover`。
源码定位：[openai.go:259-267](../internal/adapter/openai/openai.go#L259-L267)
只解码 `ir.Chunk`，没有检查 error 信封；
同文件 [238-239](../internal/adapter/openai/openai.go#L238-L239)
忽略 event 标签。建议优先补失败回归，再修错误识别、传播及首内容前的 fallback。

### P2：补发尾帧改变 completion ID 和时间戳

当上游不提供 usage，或直接 EOF 且没有 finish/DONE 时，网关补发尾帧。
对已捕获的输出做独立结构校验，发现：

```text
content.id = chatcmpl-followup
finish.id  = chatcmpl-...另一 ID（需要补发 finish 的路径）
usage.id   = chatcmpl-...又一 ID
```

补发帧的 `created` 也从上游时间戳变为当前时间。
同一 OpenAI completion 的 chunk 应共享 ID 和创建时间，
不能把内容、finish、usage 表示成多个 completion。
model 别名在这些输出中保持一致。

证据：`metadata.eof`、`metadata.missing_usage`；
对应功能断言 `wire.eof`、`wire.missing_usage` 原本通过，
说明只验证有内容与 `[DONE]` 不足以验证协议完整性。
源码：`internal/api/chat.go` 补发 head/tail/final 的路径分别调用 `ir.NewChunk`。
建议捕获一次 completion 元数据并在补发帧中复用。
当前 fm 内容控制测试能通过；其他 SDK 是否实际被破坏未验证。

### P2：不支持标准多行 SSE data

把同一事件的 JSON 分在两条 `data:` 行中，行间拼接换行后是合法 JSON：

```text
data: {"id":"chatcmpl-followup",
data: "object":"chat.completion.chunk", ...完整剩余 JSON...}

```

fm 直连返回 `Failed to parse generated content`；
经过 fmux 返回 HTTP 502，错误指出第一条 data 行的 JSON 不完整。
这里的 `...` 仅用于展示，实际测试发送完整合法 JSON。
常规 OpenAI 单行事件、CRLF、注释和分片传输通过，不应把此限制泛化为
所有 SSE 均不可用。

证据：`direct.multiline`、`gateway.multiline`。
源码：[openai.go:252-254](../internal/adapter/openai/openai.go#L252-L254)
在第一条 data 行后立即解码。建议完整组装事件，同时保留既有单行兼容路径。

## Swift 请求已得到完整样本

使用临时观察代理转发到正在运行的网关，保留 FM User-Agent，
采用相同别名与 prompt，但通过命令行指定代理地址。
PTY 捕获终端输出，独立记录网关 SSE 首内容、finish、usage 与 `[DONE]`。
不是对原始无代理请求的严格性能基准，时间包含本地代理与整个请求链路。
SSE 列以代理收到请求为起点，终端/总时长列以启动 fm 为起点，
不能直接用两列差值计算纯客户端或网络延迟；这些时点也不等同于现有 TTFT 指标。

| 样本 | 网关首内容 | 终端首输出 | 总时长 | 终止状态 |
|---|---:|---:|---:|---|
| `qwen3.8:27b` + `What is Swift?` | 39.361 秒 | 39.877 秒 | 100.111 秒 | exit 0、stop、DONE |
| `qwen-fast` + 同一问题 | 24.129 秒 | 24.338 秒 | 45.100 秒 | 测试主动取消，尚无 finish/DONE |
| `qwen-fast` + 编程语言两句式提示 | 14.858 秒 | 14.887 秒 | 26.935 秒 | exit 0、stop、DONE |

第一条样本也以用户提供的那句话开头，随后完整输出后续回答。
三个样本中，终端内容与代理实际收到的 content 均一致，包括主动取消时的前缀。
**本次完整样本没有证实截断，先前 35 秒上限也不足以覆盖这次首内容等待。**
这不排除其他请求、其他时间或其他模型出现截断。
回答原文只用于传输一致性校验，没有评估其事实准确性。

第一条的 HTTP 响应头约 13.831 秒到达，首内容又等待约 25.530 秒。
仅凭这些时点不能区分排队、冷加载、隐藏思考或具体算子耗时。
`/no_think` 是本轮提示文本，不是已经证实的后端配置开关；
两句式提示改变了多个条件，不能把加速归因于某个单独参数。

可复用本轮完成的调用形态：

```bash
fm respond --model-provider fmux --model qwen-fast \
  --instructions 'Be concise. Answer directly without reasoning.' \
  "Explain Apple's Swift programming language in two sentences. /no_think"
```

## 已通过的补充检查

- UTF-8 字符跨小分片、中文/emoji/换行、CRLF、注释、约 70k 字符大帧。
- 未提供 usage/finish 和普通 EOF 的内容结束兼容路径。
  这是宽容性验证，不代表普通 EOF 能证明模型完整生成。
- HTTP 401/429/503 的 fm 非零退出；malformed JSON 的错误暴露。
- 流静默 timeout 会给出错误，且不会发出成功 `[DONE]`。
- 取消从网关传到可控后端；真实取消样本最后也没有保留 in-flight pin。
- 8 个并发请求、2 个服务槽位、2 个等待槽位：4 个 200、4 个 429，
  可控后端峰值并发为 2。
- 三轮 transcript 恢复保留 prior turns，`/clear` 后的请求不带旧 turn。
- 结束后 inflight 为 0；SIGTERM 时正在执行的请求得到 200，
  隔离网关随后退出 0。

## 既有模型菜单问题复现

可控目录返回 `normal` 与 `backup`，`fm models` 正常列出二者；
聊天内 `/model backup` 和 `/model normal` 仍显示
`Unknown model ... Available: system, pcc.`。
使用不含连字符、冒号的 ID 排除了这些符号导致失败的猜测。
没有提交生成提示，目录测试记录 POST 数为 0。
这是既有 CLI 菜单限制的复现，不是新增网关缺陷。

## 交付与边界

故障套件使用当前源码新编译的、随机端口运行的隔离 fmux，
不是向正在运行的用户服务注入坏响应。真实 Swift 测试只请求既有本地模型。
未更改默认配置、接受许可、请求 PCC 云端生成或替换现有服务，
没有修改生产代码或创建提交。临时进程已清理。
现有 Go race 测试通过，但这些新黑盒断言失败，表明现有覆盖仍有缺口。

优先级建议：先修 SSE error/fallback，再修尾帧元数据与多行事件解析；
不要先用并发性能改造掩盖正确性问题。
完整证据见 [JSON](fm-followup-results.json)，
可复测脚本和所测网关构建见 [证据包](fm-followup-evidence.zip)。
