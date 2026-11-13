# SSE 错误传播与 completion 一致性设计

## 状态与范围

用户已确认按推荐方案实施。本批修复上一轮黑盒验证发现的三个网关协议问题：
SSE 错误丢失、补发尾帧元数据变化、标准多行事件无法解析。
保留当前未提交的首批优化，不增加依赖、不提交、不替换运行中的服务。
指标导出按已经批准的独立设计后续执行，不混入本批。
Apple `/usr/bin/fm` 的模型菜单与帮助问题不属于本仓库可修复源码。

## 证据与现有边界

依据 `docs/fm-followup-verification.md`：
错误信封被解码为空 choices 的普通 chunk，并被补上成功尾帧；
事件解析在首条 data 行后返回；补发帧分别调用 `ir.NewChunk`。

现有 API 已在尚未写出响应时允许尝试下一 target，在写出后禁止 fallback。
本设计保留这一边界，不能把“尚无内容 token”视作可重试条件：
角色帧或 usage 帧也可能已经提交 HTTP 200。

## 方案比较

1. 推荐：适配器完整解析 SSE 事件并返回独立类型的上游流错误；
   API 保留可解析的错误字段，并复用一次确定的 completion 元数据。
   改动局限在 OpenAI 适配器、API 与相应测试，不改变路由架构。
2. 仅检测 data 中的 error 文本：改动少，但不能可靠区分合法 JSON 字段、
   非信封 `event: error` 与多行事件，不采用。
3. 缓存整个 completion 后再决定是否 fallback：能推迟写出，
   但破坏实时输出、增加内存与首输出等待，不采用。

## 事件解析

`internal/adapter/openai` 按空行分隔完整事件，多个 data 字段之间拼接换行。
只剥离冒号后最多一个空格，保留 SSE data 原始内容；
忽略注释与未知字段，支持 LF、CRLF、UTF-8 分片与大事件。
普通单行事件仍在紧随的空行后输出，不引入整段流缓存。
保留现有 EOF 宽容路径：EOF 前最后一条无空行的完整事件仍可解码。
在完整事件层识别 `[DONE]`，后续 Recv 返回 EOF。

JSON error 信封无论是否带 event 标签都返回上游流错误，不返回普通 chunk。
保留 message、type、code、param；code/param 接受 JSON 值，
不因 numeric code 或 null 变成无关的 JSON 解析错误。
`event: error` 的非信封内容也必须作为错误，不能变成成功 chunk；
非标准内容使用有长度上限的诊断消息与网关默认错误字段。
错误终止该流，后续 Recv 返回 EOF，不继续处理错误后的成功标记。

## API 错误语义

新增上游流错误不伪装成 HTTPError，因为上游实际返回 HTTP 200。
无明确 HTTP 状态时最终错误使用 502，不从任意 code 推测状态。
解析到的标准错误信封字段在最终 JSON 或流内错误信封中保留；
缺少 message/type 时补网关默认值。现有 timeout code 与 HTTPError 行为保持。

尚未向客户端写出时沿用现有 target fallback；全部 target 失败才输出错误。
已经写出时发送 data 格式的 OpenAI 错误信封，不切换后端，
不补 stop、usage 或成功 `[DONE]`。取消仍不启动 fallback。
上游流错误不作为“连接失败”默认触发 backendDown：
HTTP 200 的应用错误不足以证明进程不可达，不更改其他 breaker 策略。

## Completion 元数据

每次 streamChat 只确定一组 ID、created、object，输出 model 仍使用 alias。
以第一条成功 chunk 的非空 ID 和非零 created 为准，缺失字段只生成一次。
后续转发 chunk 与补发 head/finish/usage 都复用这一组元数据，
不允许中途改成另一个 completion。没有任何成功 chunk 时一次生成全部字段。
不改变既有 usage 估算、finish 补发、普通 EOF 宽容性和单 choice 处理范围。

## 测试与验收

先添加并确认失败，再实施最小修复：

- 适配器：普通错误信封、event:error、错误前后有内容、numeric/null code、
  非信封事件错误、多行合法 JSON、空 data、未知字段、EOF、DONE 与重复 Recv。
- API：首帧前错误触发可用 fallback；无 fallback 返回 502 并保留错误字段；
  已输出后的错误保留前缀、不调用备用后端、不输出成功尾帧。
- Breaker：应用流错误不累计不可达失败，既有 HTTP 503 测试仍通过。
- 元数据：missing usage、missing finish、EOF、无帧、缺字段和冲突字段；
  每次输出的 ID/created/object/model 恒定且 usage 计算未改变。
- 兼容：单行、CRLF、注释、跨传输分片、中文、70k 大事件和取消路径。

运行适配器/API 定向测试、全量 `go test ./... -race -count=1`、
vet、临时目录构建、文档链接与 diff 格式检查。
使用随机端口隔离网关重跑既有故障套件，不向用户服务注入异常；
fm 直连上游的多行解析和模型菜单失败不因网关修复而修改预期或声称解决。
更新修复结果报告，保留原始失败证据，区分源码测试与已部署行为。
