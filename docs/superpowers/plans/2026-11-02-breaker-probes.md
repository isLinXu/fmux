# 熔断探测预约优化实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 未实际尝试的备用后端不消耗恢复探测机会。
**Architecture:** Available 只查询；Allow 负责互斥预约，
API 在能力检查后才预约。保留单目标和全部熔断的强制尝试语义。
**Tech Stack:** Go、sync.Mutex、testing、httptest。

## 任务 1：失败回归

文件：`internal/api/breaker_test.go`。
- [x] 增加 `TestUnusedFallbackDoesNotReserveProbe`：

  ```go
  dead, _ := deadBackend(t)
  s := newFallbackServer(t, dead.URL)
  s.cfg.Aliases["auto"] = config.Alias{
      Backend: "mock", Model: "mock-model", Fallback: []string{"lonely"},
  }
  s.brk = breaker.New(breaker.Options{Threshold: 1, Cooldown: 200*time.Millisecond})
  s.brk.Fail("dead", "down")
  time.Sleep(220*time.Millisecond)
  rec := post(t, s, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
  if rec.Code != 200 { t.Fatal(rec.Body.String()) }
  if !s.brk.Allow("dead") { t.Fatal("unused fallback consumed probe") }
  ```

- [x] 运行 `go test ./internal/api -run TestUnusedFallbackDoesNotReserveProbe -count=1`，
  预期断言失败。

## 任务 2：延迟预约

文件：`internal/breaker/breaker.go`、`internal/api/chat.go`。
- [x] 增加不创建 entry 的查询：

  ```go
  func (b *Breaker) Available(backend string) bool {
      if b == nil { return true }
      b.mu.Lock()
      defer b.mu.Unlock()
      e := b.entries[backend]
      return e == nil || b.availableLocked(e)
  }
  func (b *Breaker) availableLocked(e *entry) bool {
      now := b.now()
      return e.openedAt.IsZero() ||
          (now.Sub(e.openedAt) >= b.o.Cooldown &&
          (!e.probing || now.Sub(e.probeAt) >= b.o.Cooldown))
  }
  ```

- [x] Allow 用相同判断，仅开放状态更新 probeAt/probing；
  已关闭状态直接通过。
- [x] API 初筛用 Available，记录 enforceBreaker（原计划多目标且存在可用项）。
  router.Prepare 后，若 enforceBreaker 且 Allow 拒绝则跳过。
  全部不可用时 enforceBreaker=false，保留强制尝试。
- [x] 给 Available 增加闭合、开路、冷却、探测、过期、nil 状态测试，
  并发 32 个 Allow 在冷却后恰有一个通过；重复查询不改变 Snapshot。
- [x] 运行 breaker/API 的 race 回归，预期 PASS。

## 任务 3：收尾

- [x] 文档说明筛选不预约、实际尝试才预约。
- [x] gofmt、完整测试、git diff --check；不提交。
- [x] 接着进入 Clone 深拷贝修复。
