# fm 全功能验证报告

验证日期：2026-11-04。目标：当前安装的 `/usr/bin/fm`，
运行环境 macOS 27.2（26B123）。

本文件保留首轮 95 项检查的历史结果。
[补充验证](fm-followup-verification.md)已取得 Swift 请求约 100 秒正常结束、
首内容约 39 秒到达的完整样本，并发现 SSE 错误传播、尾帧元数据与多行事件问题。
因此下文 35 秒取消记录不代表已确认截断。

## 结论

已覆盖帮助列出的全部 9 个顶层命令及主要选项，包含交互内部命令、
配置、schema、流式协议、图像、真实 OCR/条码执行、会话保存/恢复、
TCP/Unix socket 服务。**不能宣称所有功能均可用**：
发现 2 项可复现问题，Apple 设备端与 PCC 功能受当前环境限制。

隔离矩阵共 95 个去重测试用例：75 通过、2 失败、15 环境受限、
1 不支持、2 未执行。无效输入正确拒绝也记为通过，不表示对应输入合法。
这些是测试用例数，不是 95 项独立产品功能。
另外执行了 4 项真实本地调用、4 项真实 fm/fmux conformance，
以及独立 Vision 与实际 Terminal 下的 PCC 检查。

## 功能矩阵

| 命令 | 已验证范围 | 结果 |
|---|---|---|
| `respond` | 普通/缓冲输出、stdin、instructions、多段 text、header、verbose、schema、图片/多图片/label、工具、transcript 保存与恢复、错误处理 | 可控提供商通过；真实短生成及 JSON 通过；原始 Swift 问题在测试上限内未完成 |
| `chat` | 两种渲染模式、会话继续/恢复、instructions、tools、provider header、偏好写入/读回 | 通过；运行时 `/model` 的自定义模型选择失败 |
| `config` | path/list/get、provider 添加/替换/删除、默认模型/提供商持久化、无效 key | 通过 |
| `schema` | string/integer/double/boolean、array/optional/description、dot nesting、object/$defs/$ref、anyOf、错误参数 | 合法语法通过；帮助示例中的 `--nested` 被拒绝 |
| `serve` | TCP 与 Unix socket、health/models、流式/非流式 completions、自定义提供商、端口与传输互斥校验 | 通过；health 同时明确报告 Apple 模型不可用 |
| `models` | URL、命名提供商、默认列表 | 自定义模型发现通过；默认列表包含不可用 Apple 模型时可能非零退出 |
| `count-tokens` | prompt/quiet/instructions/stdin/image/text/transcript | 7 种路径均受设备端模型限制，返回 `ModelManagerError 1026` |
| `quota-usage` | all/system/pcc、无效模型；另在真实 Terminal 查询 PCC | system 正确报告配额不适用；PCC 不可用 |
| `license` | status/show | 通过；没有代用户执行接受条款操作 |

交互命令实际输入了 `/help`、`/quit`、`/clear`、`/save`、`/resume`、
`/sessions`、`/model`、`/instructions`、`/tools add/remove`、`/tokens`。
`/tokens` 能显示提供商返回的 usage，但自定义模型的完整 context
window 大小没有提供；它不等价于可用的设备端 `count-tokens`。
终端渲染测试确认输入、请求、输出、退出链路，不等同于人工视觉审查。

## 已确认问题

### 1. schema 帮助示例使用不存在的参数

帮助示例出现 `--nested`，但二进制拒绝它：

```text
Error: SchemaError(errorDescription: Optional("Unknown argument: --nested"))
```

改用实际支持的 `--object address --schema '<JSON>'`。
嵌套属性通过 `$ref` 与 `$defs` 表达，这是合法输出，不是错误。
证据 ID：`schema.nested.--nested`；正确路径 `schema.nested.--object` 通过。

### 2. chat 内无法切换到当前自定义模型

使用命名提供商启动会话且能够正常请求 `cap-model`，
但输入 `/model cap-model` 得到：

```text
Unknown model 'cap-model'. Available: system, pcc.
```

URL 提供商路径也出现相同行为。建议退出后使用
`fm chat --model-provider fmux --model qwen-fast` 指定模型，
暂不依赖 `/model` 切换自定义模型。
证据 ID：`chat.runtime.model_named`、`chat.runtime_commands`。
这两项问题来自安装的 Apple CLI 表现，本次没有修改系统二进制。

## 工具执行结果

使用本地生成的文字图片和二维码，可控提供商显式返回工具调用；
`fm` 在本机执行 Vision 工具，随后将真实工具结果提交给提供商。
最终回复由测试提供商回显，**识别结果本身不是预设答案**。

- OCR 实际结果：`FM VERIFY 12345`。
- QR 实际结果：`FM-VERIFY-QR-12345`，barcodeType 为 `qr`。
- 两张显式标记图片路径通过；不启用图像工具时 `--label` 被正确拒绝。
- 首次识别观察窗口内有超时，独立 Vision 首次初始化也超过 30 秒。
  最终无标签 OCR 暖启动约 0.30 秒通过，此前 30 秒超时保存在
  `respond.tool.ocr.earlier_attempt`。不能将暖启动速度当作冷启动承诺，
  也不能仅凭这些观察断言全部延迟都来自同一个原因。

## 真实模型验证

实际调用链：`fm → 运行中的 fmux → Ollama-compatible MLX runner`，
模型为 `qwen3.8:27b-32k`。没有替换正在运行的服务。

| 调用 | 实际结果 |
|---|---|
| 原始 `fm respond 'What is Swift?'` | 35 秒测试上限内未完成，捕获 stdout/stderr 为空；测试主动取消 |
| `qwen-fast`，简短 instructions 与 `/no_think`，缓冲输出 | 7.206 秒，退出 0，输出 `OK` |
| `qwen-json`，Person schema | 20.803 秒，退出 0，输出 `{"name": "Ada", "age": 36}` |
| `fm models --model-provider fmux` | 0.041 秒，列出 4 个别名 |

35 秒是本次测试上限，不是 fm 或服务的正式超时配置。
管道缓冲也可能隐藏中间输出，**该结果不能证明首 token 未生成、
回答已截断，或用户之前那一行文本是报错**。原始问题仍需要单独定位延迟。
可控提供商的流式测试和真实短回复证明客户端主链路可工作，
但不代表任意模型、任意 prompt 都会在同一时间内完成。

## 环境限制与未执行项

- 公共 FoundationModels 探针返回 `deviceNotEligible`；
  `fm respond -m system` 返回不支持 Apple Intelligence。
  因而 system 的 use-case、guardrails、greedy 未能验证生成行为。
- `count-tokens` 绑定设备端模型，设置自定义提供商不能使它恢复。
- 自定义提供商的 `-g/--greedy` 明确返回
  `Greedy sampling is not supported`，本次记录为不支持，而非网关故障。
- 自动化上下文的 PCC 查询要求 Terminal app；在实际 Terminal 再查，
  仍返回 `fm.UnavailableError error 1`。不能据此推断具体区域或账号原因。
- 未执行 PCC 云端生成，也未代用户接受或重新接受法律条款。
- `chat --set-*` 是写设置后退出的独立模式，不能混用启动聊天选项。
  设置 `system/pcc` 和屏幕阅读偏好的正向操作在隔离 HOME 内通过，
  默认模型读回通过；不合法组合也被正确拒绝。

## 安全与证据

配置/会话测试使用临时 HOME 与 CFFIXED_USER_HOME，
原用户 `~/.fm/config.json` 的前后 SHA-256 一致。
测试进程已退出；本次创建的 PCC Terminal 窗口已关闭。
没有提交、修改生产代码、替换服务、主动下载推理模型或导出用户密钥。

完整用例、命令、stdout/stderr、脱敏后的请求及诊断输出见
[机器可读证据](fm-verification-results.json)。
[复测脚本与原始证据包](fm-verification-evidence.zip)包含测试图片和本次脚本；
脚本依赖当前 workspace 的 `scripts/capture_fm.py`，
会访问本机现有网关的真实推理测试有明确时间上限。
