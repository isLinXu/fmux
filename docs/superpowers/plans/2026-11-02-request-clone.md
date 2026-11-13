# 请求 Clone 深拷贝实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** fallback 适配器修改自己的请求时，不污染原请求或其他副本。
**Architecture:** 用 slices.Clone 保留 nil/空切片，用 clonePtr 复制指针。
显式遍历嵌套 Message、ContentPart、ToolCall、Tool、ResponseFormat，
不做 JSON 往返。
**Tech Stack:** Go slices、testing、reflect、race detector。

## 任务 1：隔离回归

文件：新建 `internal/ir/request_test.go` 和
`internal/api/clone_test.go`。
- [x] 构造包含所有可变字段的请求，逐字段修改第一份 clone，
  reflect.DeepEqual 检查原请求与第二份 clone 未变化：

  ```go
  r := populatedRequest()
  want := populatedRequest()
  other := r.Clone()
  cp := r.Clone()
  cp.Messages[0].Content.Parts[1].ImageURL.URL = "changed"
  if !reflect.DeepEqual(r, want) || !reflect.DeepEqual(other, want) {
      t.Fatal("clone shares mutable fields")
  }
  ```

- [x] 检查 nil/非 nil 空切片形状、bookkeeping；
  对独立副本并发修改工具参数、图片、采样指针。
- [x] API 首适配器修改 parts、ImageURL、Parameters、Schema、
  Stop 和 Temperature 后失败；备用 mock 的 LastRequest 保持原值。
- [x] 运行 `go test ./internal/ir ./internal/api -run 'TestClone|TestFallbackRequestIsolation' -count=1`。
  预期共享嵌套字段与空切片保留断言失败。

## 任务 2：完整深拷贝

文件：`internal/ir/request.go`。
- [x] 导入 slices；添加指针复制：

  ```go
  func clonePtr[T any](p *T) *T {
      if p == nil { return nil }
      v := *p
      return &v
  }
  ```

- [x] Clone 先 `cp := *r`，再复制顶层 Messages、Tools、Stop、
  ToolChoice、Quirks。逐 Message 复制 Content.Parts、各 ImageURL、
  ToolCalls 和各 Index。逐 Tool 复制 Function.Parameters。
- [x] 对 StreamOptions、Temperature、TopP、TopK、MaxTokens、
  MaxCompletionTok、PresencePenalty、FrequencyPenalty、Seed 调用 clonePtr。
- [x] 对 ResponseFormat、JSONSchema 分层 clonePtr，再复制 Schema：

  ```go
  cp.ResponseFormat = clonePtr(r.ResponseFormat)
  if cp.ResponseFormat != nil {
      cp.ResponseFormat.JSONSchema = clonePtr(r.ResponseFormat.JSONSchema)
      if cp.ResponseFormat.JSONSchema != nil {
          cp.ResponseFormat.JSONSchema.Schema = slices.Clone(r.ResponseFormat.JSONSchema.Schema)
      }
  }
  ```

- [x] 运行任务 1，预期 PASS；不改变字符串、Content.Present 或 bookkeeping。

## 任务 3：全量验证

- [x] gofmt，`go test ./... -race -count=1`，`go vet ./...`，
  文档链接检查及 git diff --check。
- [x] 更新执行清单为实际完成状态，记录序列化基准对比，
  核查修改范围；不提交、不替换用户运行中的 fmux 服务。
