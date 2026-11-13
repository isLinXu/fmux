# 网关协议与指标修复验证

日期：2026-11-04。分支：`optimize/request-reliability`。
本批按用户确认的设计修复网关 SSE 协议，再完成已批准的指标导出优化。
之前未提交的优化保持；没有提交、安装或替换正在运行的服务。

## 完成的修复

1. **SSE 错误与 fallback**：适配器完整组装事件，识别 error 信封与
   `event: error`，不再把错误解码为空 choices 的成功 chunk。
   API 保留 message/type/code/param，包括 numeric/null code。
   尚未向客户端写出响应时沿用 fallback；角色帧或内容帧已写出后不切换后端，
   只输出错误信封，不补 stop、usage 或成功 `[DONE]`。
   HTTP 200 内的应用错误不自动作为后端不可达证据，不从 code 推测 HTTP 状态。
2. **Completion 元数据**：采用首条成功 chunk 的 ID/created，
   缺失时只生成一次；转发与补发 head/finish/usage 共用 ID/created/object/model。
   后续上游冲突字段统一归一化，输出 model 仍为客户端别名。
3. **多行 SSE**：按空行完成事件，data 行之间拼接换行。
   保持单行、CRLF、注释、跨传输分片、中文、大帧与最后事件 EOF 的兼容。
4. **指标导出**：请求数仍按 backend/model/status 输出，
   token 按 backend/model 汇总且每组标签只出现一次。
   锁内复制独立快照，排序与聚合移到锁外；
   histogram 桶仍深复制，输出错误通过缓冲器 Flush 返回，包括短写。
   指标名、标签契约、统计 API 和 nil receiver 行为保持。

源文件：[SSE 解析器](../internal/adapter/openai/sse.go)、
[API 流处理](../internal/api/chat.go)、
[错误信封](../internal/api/server.go)、
[指标导出](../internal/metrics/metrics.go)。

## 回归与隔离复测

遵循先失败再实现：
旧实现上复现错误被解码成成功 chunk、多行首行提前解析；
适配器修复后，API 回归仍复现错误字段覆盖与 breaker 误分类；
元数据测试复现补帧 ID/created 变化；
指标测试复现重复 token 样本和 writer 错误丢失。
实现后，以上新回归及既有测试均通过。

自动化回归文件：
[SSE](../internal/adapter/openai/sse_test.go)、
[API](../internal/api/sse_correctness_test.go)、
[指标](../internal/metrics/export_test.go)。

使用当前源码编译 `fmux-check`，随机端口、隔离 HOME 和可控 provider，
复用原来的故障套件，不向用户正在运行的网关注入异常。

| 验证 | 结果 |
|---|---|
| 原 55 项故障/资源/会话断言 | 54 通过，1 失败 |
| EOF、missing usage 元数据断言 | 2/2 通过 |
| 实际 `/metrics` 输出样本唯一性 | 通过 |
| 本批合计 | 58 项，57 通过，1 失败 |
| 全量 `go test ./... -race -count=1` | 通过，含 conformance 与 e2e |
| `go vet ./...`、临时构建 | 通过 |
| 文档链接、脚本语法、`git diff --check` | 通过 |
| 证据包 ZIP 完整性、隔离进程清理 | 通过 |

原 55 项中以下六项从失败转为通过：
`gateway.multiline`、`wire.error_before`、`wire.error_after`、
`wire.event_error_before`、`wire.event_error_after`、
`fallback.sse_error_should_recover`。
两个 metadata 断言也从失败转为通过。
取消传播、2 槽位/2 队列的并发限流、多轮 transcript、clear、
inflight 归零和 SIGTERM drain 均通过。

唯一失败是 **`direct.multiline`**：
Apple `/usr/bin/fm` 直连多行 SSE provider 仍无法解析；
同一 provider 经修复后的网关则通过。
这是系统 CLI 的外部限制，没有通过改测试期望掩盖。
模型菜单问题本批未重新测试，保留上一轮外部 CLI 限制结论。

## 指标微基准

Apple M3 Max，darwin/arm64，默认 GOMAXPROCS=16。
同一 benchmark 填充 8 backend × 16 model × 2 status，三轮取中位数。
单次 Render 写向 io.Discard；混合基准并发 Observe，
每个 worker 每 32 次记录抓取一次到 bytes.Buffer。

| 基准 | 旧 ns/op | 新 ns/op | 旧 B/op | 新 B/op | allocations/op |
|---|---:|---:|---:|---:|---:|
| Render | 304,781 | 201,087 | 67,507 | 69,011 | 2,175 → 1,665 |
| RecordAndScrape | 5,325 | 5,659 | 6,216 | 6,142 | 69 → 53 |

单次导出耗时中位数约下降 34%，分配次数下降约 23%，分配字节增加约 2%。
混合记录/抓取耗时中位数约增加 6%，不能宣称并发热路径加速。
三轮样本存在明显波动，未做统计显著性结论。
正确聚合减少了输出样本行数，因此耗时变化不能全部归因于锁外排序。
这些是导出微基准，不是推理速度、TTFT 或端到端吞吐测量。

首次新实现基准与其他验证负载并行运行，原日志保留在证据包，
但不用于本表；随后在其他本批验证结束后独立重测。
旧实现基准在生产修复前测量，没有回退用户的源码。

## 交付与剩余边界

[结构化结果](fmux-repair-results.json) 包含全部断言、前后基准原始三轮、
全量 race 日志与所测构建 SHA-256。
[证据包](fmux-repair-evidence.zip) 包含复测脚本、可控配置、日志、
新构建和本批源码快照；不包含用户默认 fm 配置或凭证。
复测脚本以自身目录为隔离工作目录，需同目录 `fmux-check`；
重新运行 fault_tests.py 后再运行 check_metadata.py。

原始 [补充验证报告](fm-followup-verification.md) 与失败证据保留不变，
本报告描述的是修复后的源码与隔离实例，不是现有运行服务的状态。
本批不重跑真实模型性能测量、不接受 Apple 许可或请求 PCC 云端生成。

TTFT 计时起点与预算驱逐失败记账仍需各自的设计及回归；
本批未修改这些生命周期，也未实施锁分片或异步驱逐。
