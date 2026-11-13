# 请求级 TTFT 修复验证

日期：2026-11-09。分支：`optimize/request-reliability`。
本批按用户确认的完整口径修复 TTFT，保留前序优化。
没有提交、安装或替换运行中的服务。

## 新计时口径

起点在请求 Validate/compat.Normalize 成功后、路由前，只取一次；
包含路由、gate 排队、预算/进程准备和全部前序失败 target 的等待。
不包含请求体上传、解码和归一化，也不是客户端接收或终端显示耗时。

终点是首个上游 chunk 含非空文本，或工具 Function.Name/Arguments，
且网关写入成功。空白文本属于有效输出；role、usage、finish、
空 delta、工具 id/type/index 不属于首输出。
只有控制帧或空流时无样本，补发 completion 尾帧不制造样本。
输出后的异常保留已发生的一条样本；写出失败、输出前取消/超时不记伪样本。

指标名、backend 标签和毫秒桶不变；样本归属真正产生首输出的 backend，
可能包含前序 backend 的失败耗时。
request_duration 仍是既有的 per-target adapter 调用时长，
non-stream 不新增 TTFT。

**迁移注意：历史样本不可直接比较。**
前移起点和排除控制帧都会使 TTFT 可能变大；
这本身不能证明推理退化。本批没有真实模型速度基准或性能提升结论。

源码：[请求流与判定](../internal/api/chat.go)、
[指标说明](../internal/metrics/metrics.go)、
[架构边界](architecture.md#ttft-measurement)。
设计与计划：[设计](superpowers/specs/2026-11-09-ttft-design.md)、
[实施计划](superpowers/plans/2026-11-09-ttft.md)。

## TDD 与回归

新增 [ttft_test.go](../internal/api/ttft_test.go)，先观察失败再修改生产代码。
旧实现中，80ms 准备/排队只记录约 1ms，role/usage 等控制帧约 0ms；
纯控制响应、role 后异常、写出失败和首输出前取消/超时也记了错误样本。
失败日志保留在证据包，修复后所有定向回归通过。

覆盖准备与 gate 等待、role/usage 到文本的延迟、工具名称/参数、
多个 choice、空白文本、仅控制帧、空流、fallback、前后错误、
non-stream、写出失败、stream_max 和取消。
测试使用真实 gate、httptest 后端、SSE 写入和 Registry.Render，
不新增生产时钟注入接口。
准备/排队测试是可控 80ms 延迟，不是大型模型冷启动测量。

## 隔离 HTTP/SSE 复测

当前源码新构建、随机监听端口、独立配置与 HOME，可控本地 provider。
6 个场景全部通过，以下是每次请求前后实际指标的差值：

| 场景 | TTFT count 增量 | TTFT sum 增量 | 归属 |
|---|---:|---:|---|
| 先控制帧，再延迟 80ms 输出文本 | 1 | 83ms | source |
| 先控制帧，再延迟 80ms 输出工具 | 1 | 84ms | source |
| 只有控制帧 | 0 | 0ms | 无样本 |
| 只有控制帧后发生错误 | 0 | 0ms | 无样本 |
| 文本后发生错误 | 1 | 0ms | source |
| 首 target 延迟失败，备用输出 | 1 | 81ms | backup |

文本后错误的 0ms 是小于 1ms 的实际时间按现有毫秒规则取整，
不是空响应伪造的零值样本：它的 count 增量为 1。
备用路径没有向 source 记录 TTFT 样本。
所有成功场景正常结束，错误场景保留错误信封且无成功 `[DONE]`。

脚本首轮 1 秒就绪窗口未等到网关；后续显式指定隔离 listen，
采用最长 5 秒的短间隔健康检查，通过后才执行请求断言。
此启动探测不用于 TTFT 数值或性能判断。

## 完整验证与交付

- 定向 TestTTFT：通过。
- API/metrics 三轮 race：通过。
- 全量 `go test ./... -race -count=1`：通过，含 conformance 和 e2e。
- `go vet ./...`、临时 `go build -trimpath`：通过。
- 文档链接、补丁格式、脚本语法及 ZIP 完整性：通过。

[结构化结果](ttft-results.json) 包含指标原始输出、失败/通过测试日志、
全量和三轮 race 日志与所测构建 SHA-256。
[证据包](ttft-evidence.zip) 包含独立验证脚本、配置、日志、
所测二进制和本批源码快照，不包含用户默认 fm 配置或凭证。
脚本与 `fmux-check` 放在同一工作目录即可复测。
前序证据包不重写，继续代表它们当时测试的源码版本。

## 预算后续边界

预算本批仅核查，未修改。后续修复需要同时明确：

- 驱逐未完成/失败时不能丢掉实际驻留记账，不能盲目恢复旧条目覆盖新代。
- 驱逐计数不能在回调成功之前增加，未托管后端的 no-op 不能算实际卸载。
- 同 key 的 Pin/Acquire、整 backend Stop/ReleaseBackend 与重新启动
  必须有明确的并发边界，避免“记账正确但正在使用的模型被停掉”。

保持当前预算的 advisory 超预算准入策略；不直接改为严格拒绝、
全局锁下停机或异步驱逐。预算需独立设计确认后再实施。
