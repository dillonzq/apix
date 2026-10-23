# Gemini 协议

[English](gemini.md) · [README](../../README.zh-CN.md)

`protocol/gemini` 提供两个独立的 `KindLLM` Codec：

| 构造器 | 协议 ID | 支持范围 |
| --- | --- | --- |
| `GenerateContentCodec()` | `gemini.generate-content` | 原生，以及与 Chat Completions、Responses、Messages 的请求、普通响应和 SSE 双向直接转换 |
| `InteractionsCodec()` | `gemini.interactions` | 仅原生；与 GenerateContent 和其他协议之间的转换会明确拒绝 |

`gemini.Codecs()` 返回两者。跨协议时还需注册对应供应商的 Codec。GenerateContent 支持独立的 `ExchangeOptions.Output`；路由继续优先选择客户端的原生协议。

## 请求元信息与 HTTP

GenerateContent 的模型和流式模式位于 URL，可以通过可选的 `Payload.Metadata` 传入：

```go
stream := true
payload := apix.Payload{
    Body: []byte(`{"contents":[{"role":"user","parts":[{"text":"你好"}]}]}`),
    Metadata: apix.RequestMetadata{Model: "gemini-model", Stream: &stream},
}
info, err := reg.Inspect(gemini.GenerateContent, payload)
```

非空 Model 和非 nil Stream 优先于请求体中的路由值。元信息只用于请求，不会序列化到请求体中。现有 Codec、Exchange 方法签名保持不变；自定义 Codec 在转换时也应读取元信息，Registry 会将其用于路由。

Gemini 的 `ProtocolSpec.ParsePath` 解析 `/v1beta/models/{model}:generateContent` 或 `:streamGenerateContent`，也接受 `/v1/models/` 前缀。将 `gateway.Handler(gemini.GenerateContent)` 注册到 `/v1beta/models/` 即可自动提取元信息；`countTokens` 等其他操作会明确拒绝。

上游路径由 `ProtocolSpec.Path` 按模型和模式生成，流式使用 `:streamGenerateContent?alt=sse`，认证头为 `x-goog-api-key`。转换后的 Gemini Payload 在 Metadata 中返回有效模型和模式；未显式修改推理选项时，原生请求体逐字节保留。Gemini 客户端转向其他协议时，会将流式标记写入目标 JSON。

## 字段映射

- `systemInstruction`、`contents` 对应系统指令与消息；`model` 角色对应 assistant。不能表示的实际内容会明确报错。
- `functionDeclarations`、`parametersJsonSchema` 对应函数工具；兼容旧 Gemini Schema 的大写类型和 nullable。调用/结果按 ID 配对，支持同名并行调用与乱序返回；参数保留 JSON 数值精度。
- Responses namespace、custom 和本地 shell 复用现有规范化及身份还原。未实现的托管工具和选项会产生诊断，Safe/Strict 策略按语义丢失拒绝请求。
- `generationConfig` 映射采样、token 上限、停止序列、候选数量和结构化输出。多候选可映射为 Chat choices；单候选目标只返回首个候选，并报告额外候选丢失。
- 支持可表示的图片、PDF/文本文件、音频和视频输入。不会下载、上传媒体；跨供应商文件 ID、Messages 音视频输入、Chat 仅 URL 的文档输入会明确拒绝。
- 响应映射文本、推理、函数调用、拒绝/终止原因和 usage；提示词、缓存、推理计数分别处理。可表示的图片输出支持 Messages 块和 Chat images 扩展；Responses 图片输出及音视频输出仍会明确拒绝。
- 可表示的 grounding 引用映射为 URL citation，并将 UTF-8 字节偏移转换为字符偏移。Chat/Responses 可接收晚到的流式引用；Messages 仅在对应文本块仍开放时写入，否则报告丢失。其他协议输入的流式引用片段目前报告丢失。

协议边界接受已知 camelCase/snake_case 别名，不会改写工具参数、结果和 JSON Schema 属性中的业务字段。未知请求选项会报告诊断；原生路径保留扩展字段。

## 推理选项

通过 `ExchangeOptions.Ext` 传入 `gemini.GenerateContentOptions`：

```go
visible := true
opts := apix.ExchangeOptions{
    Ext: gemini.GenerateContentOptions{
        ThinkingMode: gemini.ThinkingBudget,
        IncludeThoughts: &visible,
        RequireThoughtSignatures: true,
    },
}
```

`ThinkingBudget` 将档位映射为预算；`ThinkingLevel` 将正数预算映射为档位。未指定时保留客户端控制的形式；0/off 与 -1/auto 保持数字哨兵。`IncludeThoughts` 独立控制推理展示。宿主根据实际模型能力选择模式，本库不按模型名猜能力，也不自动下载模型目录或调整模型上限。`thinking.ApplySuffix` 同样支持 GenerateContent。

Gemini 推理控制转向 Messages 时遵循 Messages 推理模式：默认 adaptive，数字预算转换为 effort 档位；使用 `anthropic.ThinkingManual` 时数字预算原样保留，档位按固定预算表转换。宿主仍需选择兼容的 Anthropic 模型并校验预算与输出上限。

## 签名回放

签名属于具体 Gemini part，不能当作其他供应商的原生签名。原生路径完整保留；转换后的响应通过 apix 扩展 `extra_content.google.parts` 携带有序的原始 Gemini parts：

- Chat：assistant 消息或流式末尾 delta。
- Messages：响应内容块，以及末尾 message delta。
- Responses：输出项、流式完成项和终止快照。

下一轮请求回到 Gemini 时，需要随 assistant 消息/输出项保留该字段，转换器会恢复签名与关联内容。对应的单个内容或工具项也接受 `extra_content.google.thought_signature`。如果客户端 SDK 会丢弃扩展字段，宿主需要另外保存并恢复。Exchange 只服务一次执行，不充当会话缓存。

`RequireThoughtSignatures` 可要求历史函数调用必须带签名。由于不同模型的要求不同，它默认关闭。跨协议函数调用目前要求完整的 `args` 对象，`partialArgs` / `willContinue` 这类增量参数变体会明确拒绝。

## 流式约定

`Feed` 接受任意 SSE 字节分块。Gemini 的 `finishReason` 只记录完成状态，转换器继续接收末尾 usage 和签名；宿主必须在传输 EOF 调用 `Finish`，此时才输出目标终止事件。缺少合法终止状态会失败，错误不可恢复，已经产生的有效事件仍会返回。

其他协议作为上游时，先使用其原生验证器检查完整源事件，再进行直接转换，防止错误输入生成成功的 Gemini 终止事件。函数参数片段在完整后输出。Gemini 原生 SSE 帧逐字节透传。

## Interactions 原生支持

路径为 `POST /v1beta/interactions`，认证头为 `x-goog-api-key`。请求必须且只能指定 `model` 或 `agent`。模型别名可以改写；agent 请求不能被改写成另一个模型。`previous_interaction_id`、`background`、tools、input 和扩展字段交给上游处理；后台响应允许处于 `in_progress`。

原生 SSE 保留事件并验证完成/工具交接与错误。暂不提供跨协议转换、历史资源获取、后台任务轮询、取消或跨供应商会话迁移，这些能力由宿主或上游负责。

## 验证

请求/响应/流式快照共 48 份：原有 27 份不变，新增 21 份 GenerateContent 用例。公开 API 测试覆盖独立输出协议、精确工具参数、媒体、签名回放、晚到签名、Unicode 引用、错误终止与完整/逐字节输入等价；网关测试覆盖 URL 元信息、鉴权、同协议优先、跨协议切换和 Interactions 状态透传。`make fuzz` 已包含 Gemini 分块与终止状态目标。

字段映射参考 [CLIProxyAPI ceb045a](https://github.com/router-for-me/CLIProxyAPI/tree/ceb045a129d43f805e93a5e4aef3a59b108f34f7/internal/translator/gemini)、[GenerateContent API](https://ai.google.dev/api/generate-content) 和 [Interactions API](https://ai.google.dev/gemini-api/docs/interactions-overview)。本地测试不代替真实上游兼容性验证。
