# Metrics Export Implementation Plan

> **For agentic workers:** Use executing-plans inline. Keep this independent of the SSE repair. Do not commit or replace the running service.

**Goal:** 按已批准设计保证 token 样本唯一、正确聚合并返回 writer 错误。

**Architecture:** 锁内复制独立快照，锁外排序与聚合。
bufio.Writer 保留输出错误，最终 Flush 返回错误，不改变统计 API 与标签。

**Tech Stack:** Go 标准库、testing、race、benchmark。

---

## Task 1: 失败回归与旧实现基准

文件：创建 `internal/metrics/export_test.go`。

- [x] 多 backend/model/status 输入，断言 token 总和且每组标签只有一条样本。
  核心数据为同一模型 status 200 的 10/20 tokens 与 502 的 3/4 tokens，
  期望总量 13/24，请求数仍分别导出。
- [x] 错误 writer 返回 sentinel，短写 writer 返回 `len(p)-1, nil`；
  断言 Render 返回 sentinel 或 io.ErrShortWrite，nil Registry 不写入。
- [x] 并发 Observe/TTFT/wait/reject/inflight 与 Render，
  每次输出指标名加完整标签集唯一，最终计数精确。
- [x] benchmark 填充 8 个 backend、16 个 model、2 种 status，
  单线程 Render(io.Discard) 与 RunParallel 记录/抓取，报告 allocations。
- [x] 运行 `/Users/linxu/sdk/go/bin/go test ./internal/metrics -count=1`，
  确认重复 token 与 writer 错误丢失；运行
  `/Users/linxu/sdk/go/bin/go test ./internal/metrics -run '^$' -bench BenchmarkExport -benchmem -count=3`
  保存旧结果到临时目录。

## Task 2: 最小实现

文件：修改 `internal/metrics/metrics.go`。

- [x] 锁内直接复制 request key/counter，再解锁排序 snapshot。
  histogram 继续使用 copyHists 深复制，不改变其桶语义。
- [x] 在排序 snapshot 上按 backend/model 相邻分组，累加 promptTok/complTok。
  request family 仍用原 snapshot，两个 token family 改用聚合 snapshot。
- [x] 把传入 writer 包装为 bufio.NewWriter，保持原输出语句，
  返回 Flush()；不用忽略错误的新 writer。
- [x] 运行 metrics tests 与 race，检查稳定排序、特殊标签、零 token。

## Task 3: 对比与交付

- [x] 使用相同 benchmark 参数记录新结果，如实报告时间和 allocations。
- [x] 全量 race、vet、构建、docs、diff 检查通过。
- [x] 更新优化结果，明确没有修改 TTFT 起点、budget 驱逐或锁分片。
