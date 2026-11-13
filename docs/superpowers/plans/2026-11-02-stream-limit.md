# 流式总时长上限实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为持续输出的流式请求提供涵盖排队、准备与 fallback 的总期限。
**Architecture:** 请求归一化后创建一次 WithTimeoutCause；
stream_idle 用 WithCancelCause 保持独立重置。统一识别 cause、
返回 504 或 SSE 错误，释放资源且禁止超时后的 fallback。
**Tech Stack:** Go context、testing、httptest、yaml.v3。

## 任务 1：先复现缺失的总期限

文件：新建 `internal/api/stream_max_test.go`、
`internal/config/config_test.go`。

- [x] 通过 YAML 注入 stream_max，避免测试因缺少结构字段而无法编译：

  ```go
  if err := yaml.Unmarshal([]byte("timeouts:\n  stream_max: 100ms\n"), s.cfg); err != nil {
      t.Fatal(err)
  }
  ```

  用 pacedBackend 每 20ms 持续输出 60 个 token，验证在 100ms
  左右返回 stream_max_duration 而无 DONE；上游及时取消。
- [x] 对 stallingBackend、无响应头 backend 验证 timeout code 和状态；
  gate 槽位占用时也必须在总期限内返回；Preparer 阻塞时取消并释放 pin。
  首后端用掉大部分期限后 503，备用后端不获得新期限。
- [x] 测试 YAML 默认/正值/负值，通过 yaml.Marshal 后 map 读取值，
  不在旧实现测试中引用尚不存在的 StreamMax 字段。
- [x] 运行：

  ```sh
  /Users/linxu/sdk/go/bin/go test ./internal/api ./internal/config -run 'TestStreamMax' -count=1
  ```

  预期行为断言失败：旧实现无总期限，持续流正常 DONE 或 idle 先触发。

## 任务 2：请求级期限与取消原因

文件：`internal/config/config.go`、`internal/api/chat.go`、
`internal/api/server.go`。

- [x] 新增配置，默认 15m，Validate 仅对零值设默认，负值保持：

  ```go
  StreamMax time.Duration `yaml:"stream_max"`
  // Default: StreamMax: 15 * time.Minute,
  if c.Timeouts.StreamMax == 0 {
      c.Timeouts.StreamMax = 15 * time.Minute
  }
  ```

- [x] 在归一化后、解析路由前设置请求 context：

  ```go
  if req.Stream && s.cfg.Timeouts.StreamMax > 0 {
      ctx, cancel := context.WithTimeoutCause(r.Context(), s.cfg.Timeouts.StreamMax, errStreamMax)
      defer cancel()
      r = r.WithContext(ctx)
  }
  ```

- [x] 定义 sentinel，加入 StatusOf、errorCodeOf、streamErrorCode、
  backendDown。serveTarget 返回后以请求 cause 识别排队/准备超时；
  在未写响应时返回 504，不继续 fallback。
- [x] streamChat 改为 WithCancelCause；idle 回调 cancel(errStreamIdle)；
  ChatStream 与每次 Recv 后优先检查 timeout cause，即使 Recv 返回 EOF
  也不把已经超时的流补成成功。错误分支记录 StatusOf(err) 指标：

  ```go
  func streamTimeoutCause(ctx context.Context) error {
      cause := context.Cause(ctx)
      if errors.Is(cause, errStreamIdle) || errors.Is(cause, errStreamMax) {
          return cause
      }
      return nil
  }
  ```

- [x] 重跑任务 1，预期 PASS；运行现有 stream idle、客户端取消测试。

## 任务 3：文档与回归

文件：`configs/config.example.yaml`、`docs/architecture.md`。
- [x] 示例加入 stream_max: 15m，解释 -1s 禁用，以及包含排队和冷启动；
  架构说明区分 idle 与总期限。
- [x] gofmt 后运行 `go test ./internal/api ./internal/config -race -count=3`、
  `go vet ./...`、`python3 scripts/check_docs.py`。
- [x] 核对 diff，不提交。完成后按顺序进入熔断探测任务。
