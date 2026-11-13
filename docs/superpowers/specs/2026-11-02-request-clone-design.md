# 请求 Clone 隔离设计

## 实际问题

Clone 当前只复制 Messages、Tools、Quirks 的顶层切片和 StreamOptions。
Content.Parts、ImageURL、ToolCalls.Index、工具 Parameters、Stop、
ToolChoice、ResponseFormat.JSONSchema.Schema 以及采样指针仍可能共享。

## 方案

Clone 返回完整深拷贝，独立复制所有可变切片和指针；
字符串等不可变值共享，保留 nil 与非 nil 空切片的形状，
保留未序列化 bookkeeping 和 Content.Present。
不采用 JSON 往返，避免额外编码成本以及丢失非 wire 字段。
不采用不可变 IR 改造，因为会影响全部调用方。

## 测试

表驱动失败测试逐字段修改 clone，确认原请求与第二份 clone 均不变；
检查空切片、nil 指针、bookkeeping 的保留；
使用并发修改独立副本的 race 测试验证隔离；
加入真实 fallback 流程测试，首适配器修改嵌套字段后失败，
备用适配器收到的请求仍保持原始字段。

修复只涉及 Clone 和回归测试，不调整路由配置或采样语义。
