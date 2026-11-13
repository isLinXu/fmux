# SSE Correctness Implementation Plan

> **For agentic workers:** Use executing-plans to implement this plan inline, task-by-task. Do not create commits or replace the running service.

**Goal:** 修复 SSE 错误丢失、多行事件解析和 completion 元数据不一致。

**Architecture:** 在 OpenAI 适配器组装完整事件并返回 typed StreamError。
API 沿用已写出响应的 fallback 边界，保留错误字段，并对所有 chunk 使用一次确定的元数据。

**Tech Stack:** Go 标准库、httptest、现有 adapter/API/gate/breaker。

---

## Task 1: 事件解析回归

文件：创建 `internal/adapter/openai/sse_test.go`。

- [x] 写入 table tests，通过 `newSSEStream(io.NopCloser(strings.NewReader(wire)))`
  输入错误信封、多行 JSON、CRLF、注释、EOF 和 DONE。
  核心失败用例：

```go
st := newSSEStream(io.NopCloser(strings.NewReader(
    "data: {\"error\":{\"message\":\"injected failure\",\"code\":42}}\n\n")))
chunk, err := st.Recv()
if err == nil || chunk != nil {
    t.Fatalf("chunk=%+v err=%v", chunk, err)
}
```

- [x] 运行 `/Users/linxu/sdk/go/bin/go test ./internal/adapter/openai -run TestSSE -count=1`，
  确认错误信封得到成功 chunk、多行第一行解码失败。
- [x] 在 `internal/adapter/openai/openai.go` 将 SSE 解析迁移到独立
  `internal/adapter/openai/sse.go`，保留 `newSSEStream` 接口。
  Recv 在事件空行或 EOF 时调用 decode；data 间插入 `\n`。
  用 `json.RawMessage` 保留 error 字段，返回 `*StreamError`。
- [x] 同一命令确认通过；测试 UTF-8 分片、70k 帧和错误后的重复 Recv。

## Task 2: API 错误语义

文件：创建 `internal/api/sse_correctness_test.go`；
修改 `internal/api/chat.go`、`internal/api/server.go`。

- [x] 用 httptest 上游与 `newFallbackServer` 覆盖首帧前 fallback、
  无备用 502、内容或角色帧后的错误与备用后端未调用。
  对输出错误信封检查 message/type/code/param，禁止 `[DONE]` 和补发 finish。
- [x] 运行 `/Users/linxu/sdk/go/bin/go test ./internal/api -run TestUpstreamStream -count=1`，
  确认旧 API 丢失原始字段和错误导致 breaker 失败。
- [x] 增加错误信封转换函数，使用 `errors.As` 获取 `*openai.StreamError`；
  JSON 与 SSE 错误路径调用该转换，不改变 timeout 和 HTTPError。
  `backendDown` 对 StreamError 返回 false。
- [x] 同一命令确认通过；运行已有 breaker、stream idle 和 stream max 测试。

## Task 3: 元数据

文件：`internal/api/sse_correctness_test.go`、`internal/api/chat.go`。

- [x] 检查 EOF、缺 usage/finish、缺字段、冲突字段和空流的输出。
  对所有 JSON chunk 的 ID/created/object/model 做相等检查；
  检查非空 ID/非零时间和上游第一帧元数据保留。
- [x] 运行 `/Users/linxu/sdk/go/bin/go test ./internal/api -run TestStreamMetadata -count=1`，
  确认补发尾帧不一致。
- [x] 在 streamChat 的首个成功 chunk 确定一份 metadata，
  后续 chunk 统一赋值；补发 chunk 复制 metadata，不单独调用 NewChunk。
- [x] 同一命令及 `/Users/linxu/sdk/go/bin/go test ./internal/api ./internal/adapter/openai -count=1`
  均通过。

## Task 4: 全量与黑盒复测

- [x] 运行 gofmt、全量 race、vet、临时目录构建。
- [x] 用 `/tmp/fmux-fm-followup.x2VlJX/fault_tests.py` 的模式运行独立复测，
  为修复后的结果使用新的临时目录与构建；保留旧证据。
- [x] 无 fallback 的 wire 错误保留字段；fallback.sse 成功；
  metadata EOF/usage 全部一致；gateway.multiline 成功。
- [x] fm direct.multiline 与模型菜单为外部 CLI 限制，不声称已修复。
- [x] 保存新的修复结果报告、原始日志和构建信息，运行
  `python3 scripts/check_docs.py` 与 `git diff --check`。
