# OpenAI 请求序列化优化设计

## 范围与顺序

第一项仅优化 `internal/adapter/openai` 的请求编码，不改变 IR、路由、
进程管理或内存驱逐策略。后续依次处理流式总时长上限、熔断探测清理、
Clone 深拷贝检查，每项先核查实际问题，再独立设计、测试和实现。

## 当前问题

`marshalWithStream` 将请求编码成 JSON，再解码为 RawMessage map，
覆盖 stream 后重新编码。部分流式请求还调用 `dropJSONField`，
再次解码与编码以去除 stream_options。
这增加大上下文与图片 data URL 请求的编码成本；具体收益需要基准测量，
不采用此前未经测量的百分比估计。

## 方案选择

采用适配器内临时 wire 结构：嵌入请求字段，通过同名显式字段覆盖
stream 与 stream_options，然后单次调用 json.Marshal。
不修改请求原对象，也不增加 ChatRequest 的协议控制字段。

备选方案是在 IR 添加覆盖字段或自定义 MarshalJSON。其影响所有调用方，
增加协议回归范围，本次不采用。保留 map 往返虽无行为变更，但不能消除冗余工作。

## 兼容性要求

- 非流式请求始终显式发送 `"stream": false`，移除 stream_options。
- 流式请求始终显式发送 `"stream": true`。
- 仅客户端请求 usage 且后端支持 UsageInStream 时保留 stream_options。
- 其他已支持请求字段保持相同 JSON 语义，包括 tools、tool_choice、
  response_format、采样参数、图片内容。
- 不输出 ResolvedAlias、ResolvedBackend、ResolvedModel、ClientIsFm、Quirks。
- 每个协议键最多出现一次；不依赖 JSON 对象字段顺序。
- 编码错误继续返回给调用方，不发起后端请求。
- 编码前后原请求及其 StreamOptions 内容不变。

## 验证

先补适配器测试，覆盖流式/非流式、usage 开关、后端能力开关、
显式 stream 值、键唯一性、字段保留、非法 RawMessage 与原对象不变。
用 httptest 验证实际转发 body，并运行已有 conformance 回归测试。

对短文本、长上下文、图片 data URL 请求建立报告 allocations 的基准，
保存优化前后测量结果。性能优化不强行制造协议行为失败：
现有兼容行为测试应先在旧实现通过，再比较基准；
发现真实缺陷时先添加失败回归测试。

使用 `/Users/linxu/sdk/go/bin/go` 运行测试、race 检查及 vet。
不修改系统 PATH、不增加依赖、不自动提交。

## 风险与验收

主要风险是嵌入结构同名字段的 JSON 选择规则或 omitempty 引起协议差异，
通过解析实际 body 并检查键唯一性验证。
验收要求是请求语义兼容、输入不变、无新增依赖、相关测试通过，
且大请求编码基准有可重复的改进。不设未经测量的性能百分比目标。

## 核查修正

现有熔断器已有丢失探测在下个 cooldown 后恢复的实现与测试，
后续不以“永久挂起”为理由重复实现。预算驱逐回调在锁外执行，
没有证据时不进行异步驱逐或全局锁分片改造。
