# apix

<div align="center">

<img src="assets/logo.svg" alt="apix Logo" width="144" height="144">

**跨供应商转换与路由 API 调用，一个 Go 库。**

[English](README.md) · **简体中文**

[安装](#安装) · [快速开始](#快速开始) · [路由](#路由) · [网关](#网关) · [文档](#文档)

[![许可证：MIT](https://img.shields.io/badge/license-MIT-64748b?style=flat-square)](LICENSE)
[![版本：v0.1.0](https://img.shields.io/badge/release-v0.1.0-6366f1?style=flat-square)](https://github.com/dillonzq/apix/releases/tag/v0.1.0)
[![Go：1.26.7+](https://img.shields.io/badge/Go-1.26.7%2B-0891b2?style=flat-square&logo=go&logoColor=white)](go.mod)
[![依赖：标准库](https://img.shields.io/badge/dependencies-standard_library-0d9488?style=flat-square)](go.mod)

</div>

---

apix 在不同供应商的 API 协议之间转换请求、普通响应和流式响应，并把每个请求路由到最合适的供应商：优先选择与客户端同协议的供应商，其次选择可转换的同类接口。仅依赖 Go 标准库，要求 Go **1.26.7+**。

库是分层的：核心只做转换和路由决策，不做任何网络 I/O；可选的 gateway 通过 HTTP 执行请求并自动故障切换。

| 包 | 职责 |
| --- | --- |
| `apix` | 与协议无关的核心：`Kind`、`Protocol`、`Codec`、`Exchange`、`Registry`、错误、诊断 |
| `protocol/openai`、`protocol/anthropic`、`protocol/gemini` | 各供应商协议的 Codec |
| `route` | 同协议优先的端点选择、优先级、权重、健康统计 |
| `gateway` | 可选的 HTTP 执行器与 `http.Handler`，支持故障切换 |
| `sse` | Server-Sent Events 分帧与解码 |
| `thinking` | 可选的 LLM 推理控制与 `model(high)` 后缀 |

## 支持范围

| Kind | 协议 ID | 常量 | 端点 |
| --- | --- | --- | --- |
| `llm` | `openai.chat-completions` | `openai.ChatCompletions` | `POST /v1/chat/completions` |
| `llm` | `openai.responses` | `openai.Responses` | `POST /v1/responses` |
| `llm` | `anthropic.messages` | `anthropic.Messages` | `POST /v1/messages` |
| `llm` | `gemini.generate-content` | `gemini.GenerateContent` | `POST /v1beta/models/{model}:generateContent` / `:streamGenerateContent` |
| `llm` | `gemini.interactions` | `gemini.Interactions` | `POST /v1beta/interactions`（仅原生） |

Chat Completions、Responses、Messages 与 Gemini GenerateContent 在全部十二个方向上直接互转；Interactions 仅支持原生。原生请求保留供应商字段，GenerateContent 的模型和流式模式通过请求元信息传入。URL 处理、推理选项和签名回放见 [Gemini 协议](docs/protocols/gemini.zh-CN.md)。转换覆盖文本、图片/文件/音频/视频输入、函数与 custom 工具、本地 shell 适配、推理及其历史回传、usage、结构化输出、缓存提示和可表示的引用。映射与限制见 [LLM 协议转换](docs/protocols/llm.zh-CN.md)。

图像生成、语音等其他接口类型通过同一套接口接入，见[新增协议与接口类型](docs/extending.zh-CN.md)。

## 安装

```sh
go get github.com/dillonzq/apix@v0.1.0
```

## 快速开始

注册需要的 Codec，然后每个请求创建一个 `Exchange`。下面把 Anthropic Messages 请求转换给 OpenAI Chat Completions 上游，再把响应转换回来：

```go
reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs())...)
if err != nil {
    log.Fatal(err)
}
ex, aerr := reg.NewExchange(anthropic.Messages, openai.ChatCompletions, apix.ExchangeOptions{})
if aerr != nil {
    log.Fatal(aerr)
}
upstream, aerr := ex.ConvertRequest(apix.Payload{Body: clientBody}, "gpt-4.1")
// 用自己的 HTTP 客户端发送 upstream.Body，然后：
reply, aerr := ex.ConvertResponse(status, apix.Payload{Body: upstreamBody})
```

上游模型名为空时沿用客户端的模型。流式响应按块转换：

```go
stream, aerr := ex.NewStreamConverter()
events, done, aerr := stream.Feed(networkChunk) // 按顺序写出 events
tail, aerr := stream.Finish()                   // 上游 EOF 时调用
```

`Feed` 接收任意大小的字节块，返回可直接写出的完整客户端事件。一次调用可能同时返回事件和错误：先写出事件，再停止。没有有效终止事件就结束的流会在 `Finish` 时失败，失败后的转换器会持续返回错误。

`Exchange` 保存一次执行的状态，例如响应中需要恢复的工具身份和记录的诊断。每个请求新建一个，并顺序使用。Codec 和 Registry 可以并发使用。

### Exchange 选项

`apix.ExchangeOptions` 携带损耗策略，以及上游 Codec 专属的选项：

| 选项 | 上游 | 作用 |
| --- | --- | --- |
| `Output: openai.Responses` | 任意 | 把响应和流转换为同 Kind 的另一种协议，而不是客户端协议；适用于响应格式与请求格式分开指定的宿主 |
| `LossPolicy: apix.LossPolicySafe` | 任意 | 拒绝影响语义的请求转换损耗 |
| `LossPolicy: apix.LossPolicyStrict` | 任意 | 拒绝所有已报告的请求损耗 |
| `Ext: anthropic.MessagesOptions{AllowUnsignedThinking: true}` | Messages | 把跨协议推理历史回传为无签名 thinking 块 |
| `Ext: anthropic.MessagesOptions{ThinkingMode: anthropic.ThinkingManual}` | Messages | 为不支持 adaptive 的旧 Claude 模型发送固定 `budget_tokens`（默认 adaptive） |
| `Ext: openai.ResponsesOptions{NormalizeTools: true}` | Responses | 为只接受函数工具的上游展开 namespace、包装 custom 工具并适配本地 shell |

`ex.Diagnostics()` 列出转换省略或近似的字段，包含 `Code`、`Path`、`Message`、`Semantic`、`From`、`To`。响应和流式响应只报告损耗，不会因策略被拒绝。

## 路由

`route.Router` 对能服务某个请求的端点排序：与客户端同协议的端点排在前面（零转换），可转换的同类端点排在后面。每一层内 `Priority` 小者优先，同优先级按 `Weight` 加权打散。持续失败的端点会在冷却期内被跳过。

```go
r, err := route.New(reg, []route.Endpoint{
    {ID: "openai", Protocol: openai.ChatCompletions, Models: map[string]string{"smart": "gpt-4.1"}},
    {ID: "anthropic", Protocol: anthropic.Messages, Priority: 1, Models: map[string]string{"smart": "claude-sonnet-4-5"}},
})
info, aerr := reg.Inspect(anthropic.Messages, apix.Payload{Body: clientBody})
candidates, aerr := r.Route(info) // anthropic 在前：对 Messages 客户端它是同协议
// 按顺序尝试候选，并回报每次尝试的结果，供健康统计使用：
r.Report(candidates[0].Endpoint.ID, route.Outcome{Err: attemptErr, Latency: elapsed})
```

`Models` 把客户端模型名或别名映射到上游模型名，`"*"` 匹配任意模型。`Update` 可在运行时替换端点集合。过滤规则、熔断和自定义策略见[路由](docs/routing.zh-CN.md)。

## 网关

`gateway.Gateway` 通过 HTTP 跑完整个流程：解析、路由、转换、发送、转换回来，并在失败时切换。

```go
g, err := gateway.New(gateway.Config{
    Registry: reg,
    Upstreams: []gateway.Upstream{
        {Endpoint: route.Endpoint{ID: "anthropic", Protocol: anthropic.Messages}, BaseURL: "https://api.anthropic.com", APIKey: anthropicKey},
        {Endpoint: route.Endpoint{ID: "openai", Protocol: openai.ChatCompletions, Models: map[string]string{"*": "gpt-4.1"}}, BaseURL: "https://api.openai.com", APIKey: openaiKey},
    },
})
http.Handle("/v1/messages", g.Handler(anthropic.Messages))
http.Handle("/v1/chat/completions", g.Handler(openai.ChatCompletions))
```

`Do` 在提交点返回：普通响应转换成功之后，或流式响应产出第一个事件之后。提交点之前，可重试的失败（限流、服务端错误、网络错误、上游输出无法使用）以及被拒绝的转换会切换到下一个候选；提交点之后不再重试，因为客户端可能已经收到输出，失败通过 `Response.Body.Read` 返回。鉴权头按各协议的方案注入（OpenAI 用 `Authorization: Bearer`，Anthropic 用 `x-api-key` 加 `anthropic-version`），客户端凭据不会被转发。

## 错误

错误类型为 `*apix.Error`，包含 `Class`、`Message`、`StatusCode` 和 `Retryable`。消息已脱敏，包装自己的消息时可使用 `apix.Redact`。`apix.HTTPStatus` 给出响应状态码，`Registry.EncodeError` 按客户端协议的格式输出错误体。`FromNetwork` 和 `PostFirstByteNetwork` 分别对上游首字节之前和之后的传输失败分类。

## 宿主自行包装 SSE

转换器输出完整的 SSE 帧。宿主若要自己添加 `data:` 帧，可用零值可用的 `sse.Decoder` 拆帧：

```go
var decoder sse.Decoder
for _, frame := range events {
    for _, event := range decoder.Feed(frame) {
        if event.Done {
            continue // Chat Completions 的 [DONE]，由宿主自行发送
        }
        // 使用 event.Event 和 event.Data
    }
}
decoder.Finish() // 处理完所有转换输出后调用
```

## 文档

- [架构](docs/architecture.zh-CN.md)：分层、分派与状态约定
- [路由](docs/routing.zh-CN.md)：端点选择、健康统计与网关的切换规则
- [新增协议与接口类型](docs/extending.zh-CN.md)
- [Gemini 与原生 Interactions](docs/protocols/gemini.zh-CN.md)
- [LLM 协议转换](docs/protocols/llm.zh-CN.md)：字段映射、工具、推理、流式与限制
- [CONTRIBUTING.md](CONTRIBUTING.md)：检查、设计规则与发布流程

## 开发

```sh
make check # gofmt、vet、模块与依赖检查、构建、竞态测试
make fuzz  # 流式 fuzz
```

转换行为由 `internal/llm/translate/testdata/conversion` 下 48 份经过审阅的快照固定。确认输出变化符合预期后才更新：

```sh
go test ./internal/llm/translate -run 'Test(Protocol|GeminiProtocol)Golden' -update
```

本库采用 [MIT 许可证](LICENSE)。
