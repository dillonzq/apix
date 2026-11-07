# Token 计数

[English](tokens.md) | 简体中文 · [返回 README](../../README.zh-CN.md)

Token 计数协议属于 `apix.KindTokens`：只计算一个 LLM 请求的输入 token 数，不做生成，且只在彼此之间转换；计数请求不会被路由到生成端点，反之亦然。实现位于 `internal/tokens`；各供应商包导出 ID 与构造函数，每个供应商的 `Codecs()` 都已包含它们。

| 协议 ID | 常量 | 端点 | 响应 |
| --- | --- | --- | --- |
| `anthropic.messages.count-tokens` | `anthropic.MessagesCountTokens` | `POST /v1/messages/count_tokens` | `{"input_tokens": n}` |
| `openai.responses.input-tokens` | `openai.ResponsesInputTokens` | `POST /v1/responses/input_tokens` | `{"object": "response.input_tokens", "input_tokens": n}` |
| `google.count-tokens` | `google.CountTokens` | `POST /v1beta/models/{model}:countTokens` | `{"totalTokens": n, ...}` |

Chat Completions 没有计数端点，因此只支持 Chat Completions 的上游无法提供计数。计数不支持流式：spec 声明 `apix.FramingNone`，`NewStreamConverter` 返回 `ClassUnsupported`。

## 直接路径

三个协议在六个方向上直接互转。计数请求与同一供应商的生成请求携带相同内容，因此转换复用 LLM 引擎：

1. 把客户端的计数 body 读作对应的生成请求：Messages 与 Responses 原样读取；Gemini 读取 `contents`，或 `generateContentRequest` 中的请求（也接受 `generate_content_request`）。
2. 该请求按生成请求的方式转换，映射、诊断和损耗策略与 [LLM 协议转换](llm.zh-CN.md) 相同。
3. 再把结果整形为上游的计数 body。

| 上游 | 转换后的 body |
| --- | --- |
| Messages | `model`、`messages`、`system`、`tools`、`tool_choice`、`thinking`；结构化输出会增加 schema，因此保留 `output_config.format`。`max_tokens`、`stream`、采样参数和 `output_config.effort` 等生成控制会被丢弃：端点不接受它们，它们也不影响输入。 |
| Responses | `model`、`input`、`instructions`、`tools`、`tool_choice`、`parallel_tool_calls`、`reasoning`、`text`。 |
| Gemini | `{"generateContentRequest": {...}}`，其中 `model` 为 `models/{upstreamModel}`，这样系统指令和工具都会计入，而不只是 `contents`。请求保留 `contents`、`systemInstruction`、`tools` 与 `toolConfig`；`generationConfig` 只保留响应 schema 相关字段（`responseMimeType`、`responseSchema`、`responseJsonSchema`），`maxOutputTokens`、采样参数和 `thinkingConfig` 等生成控制会被丢弃。模型同时通过 `Payload.Metadata` 进入 URL。 |

原生请求保留原始字节，只改模型。原生 Gemini body 若使用 `generateContentRequest` 形式，其内部的 `model` 也会改为上游模型。

计数来自上游，使用上游自己的 tokenizer。请求被路由到其他供应商时，计数对实际提供服务的模型准确，而不是对客户端指定的模型。

## 响应

| 上游响应 | 读取的计数 |
| --- | --- |
| Messages、Responses | `input_tokens`，非负整数 |
| Gemini | `totalTokens`（或 `total_tokens`），proto JSON 可能写成字符串；缺失时为 0，因为 proto JSON 会省略零值 |

原生响应在校验计数后原样透传。转换后的响应只携带计数，因此 Gemini 的 `cachedContentTokenCount` 与 `promptTokensDetails` 不会传到其他协议。没有有效计数的响应返回 `ClassUpstream` 错误，网关将其视为可重试的上游故障。错误状态码的分类与生成请求相同，`Handler` 以客户端协议的错误格式返回。

计数 Exchange 不实现 `apix.UsageReporter`：计数不是计费用量。

## 选项

计数 Codec 不定义任何 `Ext` 选项，传入会被拒绝。`LossPolicy` 与 `Output` 的行为与其他 Kind 相同。`Reasoning` 覆盖会被校验，然后忽略，因为它不改变输入；开启 `gateway.Config.ReasoningSuffix` 时，`model(high)` 的计数请求按 `model` 路由。

## 网关配置

每个端点只讲一种协议，因此计数端点要和同一上游的生成端点并列配置：

```go
g, err := gateway.New(gateway.Config{
    Registry: reg,
    Upstreams: []gateway.Upstream{
        {Endpoint: route.Endpoint{ID: "anthropic", Protocol: anthropic.Messages}, BaseURL: anthropicURL, APIKey: anthropicKey},
        {Endpoint: route.Endpoint{ID: "anthropic-count", Protocol: anthropic.MessagesCountTokens}, BaseURL: anthropicURL, APIKey: anthropicKey},
        {Endpoint: route.Endpoint{ID: "google-count", Protocol: google.CountTokens, Priority: 1}, BaseURL: googleURL, APIKey: googleKey},
    },
})
http.Handle("/v1/messages", g.Handler(anthropic.Messages))
http.Handle("/v1/messages/count_tokens", g.Handler(anthropic.MessagesCountTokens))
http.Handle("/v1/responses/input_tokens", g.Handler(openai.ResponsesInputTokens))
```

Gemini 的生成与计数操作共用 `/v1beta/models/` 前缀，需要先按操作分发再选择 handler：

```go
generate, count := g.Handler(google.GenerateContent), g.Handler(google.CountTokens)
http.HandleFunc("/v1beta/models/", func(w http.ResponseWriter, r *http.Request) {
    if strings.HasSuffix(r.URL.Path, ":countTokens") {
        count.ServeHTTP(w, r)
        return
    }
    generate.ServeHTTP(w, r)
})
```

路由规则不变：原生端点优先，其次是可转换端点。带 `previous_response_id` 或 `conversation` 的请求为 `Stateful`，只会发往 Responses 端点。从内容中识别出的特性（如图片输入）与生成请求一样参与端点过滤。
