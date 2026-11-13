# 请求级 TTFT 计时设计

## 状态与范围

用户已确认采用完整口径。本批仅修正 TTFT 起点与首输出判定，
不改变推理行为、gate、stream_idle/stream_max、fallback 或预算生命周期。
保留此前有意修改的源码和测试，不提交，不替换运行中的服务。

## 已核查的问题

`streamChat` 在 gate 准入和 adapter.Prepare 之后才开始计时，
因此现有 TTFT 不包含排队、预算准备、进程冷启动和前序失败 target。
首条上游 chunk 写出即记样本，纯 role 或 usage chunk 也提前结束计时。
指标名和帮助文本将其表达为请求到首 token，实际口径不一致。
现有直方图标签只有 backend，未增加 alias/model 等维度。

## 方案比较

1. 推荐：保留指标名、标签与桶，将口径修正为“有效请求准备完成到
   首个文本或工具函数输出成功写出的时间”。同时修起点与终点，契合 TTFT 名称。
   历史样本会发生语义变化，必须在帮助文本和结果报告显式记录。
2. 只前移计时起点，仍以首个任意 chunk 为终点：兼容性变化较小，
   但 role-only chunk 仍会把隐藏等待误报为低 TTFT，不作为推荐。
3. 新增另一套首内容指标，保留原指标：历史图表无变化，
   但增加重复指标和两种长期口径，本批不采用。

## 起点与标签

在请求 Validate 与兼容 Normalize 成功后、Resolve 之前取一次 time.Now。
同一 HTTP 请求的所有 target 共享该时间，显式传递到 serveTarget/streamChat。
包含路由、gate 等待、预算/进程准备、失败 target 与 fallback 的耗时。
不包含客户端上传请求体、JSON 解码、Normalize 之前的阶段，
也不代表网络到达客户端或终端显示的时间。

仍按最终产生首个有效输出的 backend 归属样本，
故该 backend 的 TTFT 可能包含前序 target 的失败耗时，文档必须注明。
保留 request_duration 的既有 per-target 计时，不把它悄悄改为请求总时长。
non-stream 请求不记录 TTFT，不改变其超时与计数。

## 终点与错误

只有上游成功 chunk 含以下任一输出，并且 sw.event 返回成功后，才记 TTFT：

- 至少一个 choice 的 Delta.Content 非空，空白文本也属于输出。
- 至少一个 tool call 的 Function.Name 或 Function.Arguments 非空。

纯 role、空 delta、tool id/type/index、usage、finish 与注释不算首输出。
网关生成的补发 head/finish/usage 不产生 TTFT 样本；
只有空响应、错误或取消而没有任何有效输出时，不制造零值样本。
一旦已经输出首文本/工具函数，只记一次，即使随后流错误或取消，
也保留已经真实发生的首输出样本。
HTTP/SSE 正确性与既有禁止写出后 fallback 的边界保持不变。

## 文件与实现约束

修改 `internal/api/chat.go`：请求级起点显式传递、
首文本/工具函数输出的判定与写出后一次记录。
修改 `internal/metrics/metrics.go` 的 TTFT 注释和 HELP 文本，桶/名字/标签不变。
修改 `docs/architecture.md` 说明计时边界和历史口径差异。
创建 `internal/api/ttft_test.go`，不新增依赖或生产测试专用时钟接口。

## TDD 与验收

先复现失败：
准备阶段等待与 gate 等待应纳入 TTFT；
首 role/usage chunk 到真正输出之间的延迟不得被忽略；
纯控制帧成功结束与首输出前错误没有 TTFT 样本。

再验证：多个 choice、文本、工具名称/参数增量、空工具元数据；
失败 target 到备用 target 的总等待归属备用 backend；
输出后错误保留一条样本，多 chunk 不重复；
non-stream、总超时、取消与空流不生成伪首输出。
时间测试使用同步 channel 建立执行顺序与适度延迟，
断言主要使用下界而非易抖动的精确毫秒/上界。

执行定向测试、API/metrics 三轮 race、全量 race、vet、临时构建、
文档链接和 diff 检查。使用隔离可控 provider 验证指标内容，
不将指标变大称为推理退化，不声称真实模型变快。

## 后续预算任务

预算需单独设计驱逐中状态、失败保留记账、同 key 重新 Acquire/Pin、
ReleaseBackend 与回调完成之间的并发行为。
必须结合 supervisor.Stop/OnStop 和 Prepare 顺序，
不能仅在失败回调后盲目恢复旧条目，或直接改成异步驱逐。
本设计不批准上述预算实现。
