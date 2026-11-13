# OpenAI 请求序列化优化实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 消除 OpenAI 适配器 JSON map 往返，保持现有协议语义。

**Architecture:** 在适配器使用嵌入 ChatRequest 的临时 wire 结构，
显式覆盖 stream 和 stream_options。调用方决定 usage 能力策略，
编码过程不修改 IR 请求。现有行为测试先通过，再用基准确认收益。

**Tech Stack:** Go、encoding/json、net/http/httptest、testing。

---

## 文件职责

- 新建 `internal/adapter/openai/openai_test.go`：实际转发 body 的兼容性、
  请求不变性、非法 JSON 错误，以及短文本/长上下文/图片编码基准。
- 修改 `internal/adapter/openai/openai.go`：单次编码及调用方 usage 策略。
- 不修改 IR、配置或依赖。

## 任务 1：协议测试与基准

- [x] 新增表驱动 httptest，组合请求 Stream、IncludeUsage、后端 UsageInStream。
  请求含 tools、schema、图片和内部 bookkeeping。解析 body，校验唯一键、
  显式 stream 值与其余字段保持一致；比较调用前后请求 JSON。

  ```go
  before, err := json.Marshal(req)
  if err != nil { t.Fatal(err) }
  // 接收实际 HTTP body，检查 stream 和 stream_options。
  after, err := json.Marshal(req)
  if err != nil { t.Fatal(err) }
  if !bytes.Equal(before, after) { t.Fatal("request mutated") }
  ```

- [x] 加入非法 ToolChoice 的错误测试，断言编码失败且没有 HTTP 请求。

  ```go
  req.ToolChoice = json.RawMessage(`{`)
  if _, err := a.Chat(context.Background(), req); err == nil {
      t.Fatal("invalid raw JSON must fail")
  }
  ```

- [x] 添加 `BenchmarkMarshalWithStream`，请求大小分别为短文本、
  256 KiB 文本、1 MiB 图片 data URL，开启 `ReportAllocs`。
- [x] 运行协议测试与旧实现基准：

  ```sh
  /Users/linxu/sdk/go/bin/go test ./internal/adapter/openai -count=1
  /Users/linxu/sdk/go/bin/go test ./internal/adapter/openai -run '^$' -bench BenchmarkMarshalWithStream -benchmem -count=3
  ```

  预期测试通过；基准输出 ns/op、B/op、allocs/op。日志保存于 /tmp，不放入仓库。

## 任务 2：单次编码

- [x] 用临时结构替换 map 往返：

  ```go
  func marshalWithStream(req *ir.ChatRequest, stream bool) ([]byte, error) {
      var options *ir.StreamOptions
      if stream {
          options = req.StreamOptions
      }
      return json.Marshal(struct {
          *ir.ChatRequest
          Stream bool `json:"stream"`
          StreamOptions *ir.StreamOptions `json:"stream_options,omitempty"`
      }{req, stream, options})
  }
  ```

- [x] ChatStream 在局部值拷贝上处理 usage；不修改原请求。

  ```go
  cp := *req
  if !wantUsage {
      cp.StreamOptions = nil
  }
  body, err := marshalWithStream(&cp, true)
  ```

  删除 `dropJSONField`、旧分支及不再使用的 strconv import。

- [x] 格式化，重新运行任务 1 测试和相同基准，比较三组编码成本。

## 任务 3：回归与收尾

- [x] 执行完整验证：

  ```sh
  /Users/linxu/sdk/go/bin/gofmt -w internal/adapter/openai/openai.go internal/adapter/openai/openai_test.go
  /Users/linxu/sdk/go/bin/go test ./... -race -count=1
  /Users/linxu/sdk/go/bin/go vet ./...
  python3 scripts/check_docs.py
  git diff --check
  ```

  预期可运行测试通过；报告依赖真实 fm 或其他环境导致的跳过项。
- [x] 核对 git diff，仅保留本项相关改动，汇报测量结果。
- [x] 不创建 git commit。提交与后续较大架构变更需要单独授权。
