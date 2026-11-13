# 流式总时长上限设计

## 方案

增加 `timeouts.stream_max`，默认 15m，0 使用默认值，
负值显式禁用以兼容超长生成。正值限制一次流式请求的排队、
冷启动、后端响应及 fallback 总时长；计时从请求完成归一化后开始，
不包含读取客户端请求体。非流式行为不变。

使用一次请求级 `context.WithTimeoutCause`，而不是每个 fallback 重新计时。
保留 stream_idle 的逐 chunk 重置机制，以 context cause 区分超时原因。
两个时限以先触发者为准，不在总超时后继续尝试其他后端。

发送 SSE 前超时返回 HTTP 504、code `stream_max_duration`；
已发送 SSE 后只发错误事件，不补 finish 或 `[DONE]`。
取消上游请求、释放 gate、pin、进程 claim，记录 504 指标，
总时长超限不计为后端宕机。

## 备选与权衡

仅从上游 ChatStream 开始计时实现简单，但排队、冷启动和多级 fallback
可绕过总上限，本次不采用。默认禁用更保守，但无法默认防止持续慢流占用；
采用 15m 默认并允许负值禁用。

## 测试与文档

先写失败测试：持续产 token 仍到期、首 token 前到期、
排队到期、冷启动到期、fallback 共用期限。验证正常完成、
idle 优先、客户端取消、取消后资源释放与熔断状态。
配置默认值、显式正值、负值、YAML 加载各自测试。
更新示例配置和架构说明；race 全量回归。
