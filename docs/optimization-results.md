# 首批优化完成记录

日期：2026-11-02。分支：`optimize/request-reliability`。
按确认顺序完成 #3、#6、#5、#19；未提交，未替换已运行服务。

## 1. JSON 单次编码

在 OpenAI 适配器内使用临时 wire 结构，删除 JSON map 编解码往返。
显式 stream 值、usage 策略、其他请求字段及原请求不变性通过
32 组实际 HTTP 转发组合测试；非法 RawMessage 不发起后端请求。

Apple M3 Max / darwin arm64 / Go 1.27.3，
`BenchmarkMarshalWithStream` 三轮测量中位数：

| 请求 | 优化前 | 优化后 | 编码耗时下降 | allocations/op |
|---|---:|---:|---:|---:|
| 64 字节文本 | 2.163 us | 0.832 us | 61.5% | 21 → 6 |
| 256 KiB 上下文 | 0.717 ms | 0.297 ms | 58.6% | 30 → 7 |
| 1 MiB 图片 data URL | 2.700 ms | 1.149 ms | 57.4% | 38 → 10 |

以上仅为请求编码微基准，不是推理速度或端到端延迟提升。
原始中间日志在 `/tmp/fmux-serialization-before.txt` 和
`/tmp/fmux-serialization-after.txt`。

## 2. 流式请求总期限

新增 `timeouts.stream_max`，默认 15m；零值使用默认，负值禁用。
计时从请求归一化后开始，涵盖排队、冷启动及全部 fallback；
不包含读取客户端请求体。

总期限与逐 chunk 重置的 stream_idle 独立，先触发者生效。
SSE 前超时返回 504；SSE 后发送 `stream_max_duration` 错误，
不补 `[DONE]`，取消上游并释放资源，不在总超时后继续 fallback。
测试覆盖持续输出、无响应头、无首 chunk、排队、准备阶段、
fallback 共用期限、idle 优先、显式禁用与配置默认值。

## 3. 熔断恢复探测

原实现已有丢失探测在下一 cooldown 后恢复，未重复实现该机制。
修复了另一个实证问题：初筛调用 Allow，提前占用未被尝试的备用探测。
现在 Available 无副作用查询，能力检查后、实际尝试前才预约。
保留单目标及全部开路时的强制尝试契约。
未尝试 fallback 不消费探测、并发单探测及既有熔断测试通过。

## 4. Clone 完整隔离

独立复制嵌套内容、图片、工具参数、schema、RawMessage、
Stop、采样指针和其他可变字段，保留 nil/空切片形状、
Content.Present 及内部 bookkeeping。无 JSON 往返。
24 组字段隔离、并发副本以及真实 fallback 修改隔离测试通过。

## 验证

- `go test ./... -race -count=1`：通过。
- API/config、API/breaker、Clone 相关测试分别重复三轮 race 验证：通过。
- `go vet ./...`、`go build -trimpath`、文档链接及 diff 格式检查：通过。
- 真实 fm 的 respond/tool/schema/models 四项 conformance：通过，未跳过。
- 进程 lifecycle、metrics、shutdown drain 三项 e2e：通过，未跳过。

验证使用 mock/测试后端，没有对真实 Ollama、MLX 或大型模型做性能测量。
构建验证产物位于 `/tmp/fmux-optimized`，不会自动安装到用户环境。

## 后续协议与指标修复

2026-11-04 已完成 SSE 错误传播/fallback、多行事件组装、
completion 元数据统一，以及此前批准的指标导出修复。
仍未提交或替换服务，原先四项优化保持。

新回归、全量 race、vet 与隔离构建通过。
58 项隔离黑盒断言中 57 通过，唯一失败是系统 fm 直连多行 SSE，
同一输入经修复后的网关已经通过。
指标 token 标签集唯一且按 backend/model 聚合，writer 错误不再丢失；
导出排序与聚合在锁外执行。

指标微基准中单次导出耗时中位数下降约 34%，但混合记录/抓取耗时
中位数增加约 6%，不宣称推理吞吐或并发热路径提升。
TTFT 起点与预算驱逐失败记账不在本批修改范围。
详见 [修复验证报告](fmux-repair-verification.md)、
[结构化结果](fmux-repair-results.json) 与 [证据包](fmux-repair-evidence.zip)。

## 请求级 TTFT 修复

2026-11-09 按用户确认的完整口径修复 TTFT：
校验/归一化后开始，同一请求包含排队、准备与前序 fallback，
只在首文本或工具函数输出成功写出后记录一次。
role/usage/finish、空流及首输出前错误/取消不制造样本。
指标名、backend 标签与桶不变，历史口径不同，数值不能直接比较。

先失败的回归已通过，API/metrics 三轮 race、全量 race、vet、
隔离构建与 6/6 HTTP/SSE 指标验证通过。
本项不改变 request_duration、预算策略或模型推理行为，
不提交或替换服务，也不声称真实模型速度提升。
详见 [TTFT 验证报告](ttft-verification.md) 与 [证据包](ttft-evidence.zip)。
预算驱逐失败记账与并发生命周期仍是独立后续任务。
