# 新增协议与接口类型

[English](extending.md) | 简体中文 · [返回 README](../README.zh-CN.md)

apix 的核心与协议无关。新增一种协议或一种接口类型（Kind）时，只需要实现 `apix.Codec`，`route` 和 `gateway` 都不需要修改。

## 放在哪里

- 协议按供应商组织，放在 `protocol/<vendor>/`。例如 OpenAI 图像生成放在 `protocol/openai/images.go`，Gemini 的各类接口放在 `protocol/gemini/`。
- 协议 ID 带命名空间，格式为 `<vendor>.<api>`，例如 `openai.images.generations`、`gemini.generate-content`。
- 多家供应商共用的同类转换代码放在 `internal/<kind>/`，例如 `internal/image/`。LLM 的现有实现就在 `internal/llm/`。
- 每个供应商包都提供 `Codecs() []apix.Codec`，调用方用 `apix.NewRegistry(slices.Concat(...))` 显式注册。

## 需要实现什么

```go
type Codec interface {
    Spec() apix.ProtocolSpec
    Inspect(req apix.Payload) (apix.RequestInfo, error)
    EncodeError(err error) apix.Payload
    Accepts(p apix.Protocol) bool
    NewExchange(client apix.Protocol, opts apix.ExchangeOptions) (apix.Exchange, error)
}
```

| 方法 | 要点 |
| --- | --- |
| `Spec` | 填写 `ID`、`Kind`、`Method`、`Path`、默认鉴权方式 `Auth`、固定头 `Headers`、流式帧格式 `Stream`。`Path` 可以使用上游模型名，适合把模型放在 URL 里的协议。 |
| `Inspect` | 从客户端请求中读出 `Model`、`Stream`、需要的 `Features`，以及请求引用的上游状态（`Affinity` 键和 `Stateful` 标记）。请求体不一定是 JSON：multipart 表单要用 `Payload.ContentType` 解析。`Registry.Inspect` 随后会叠加 `Payload.Metadata`。 |
| `EncodeError` | 按该协议的错误格式输出错误体，`gateway.Handler` 用它回复客户端。 |
| `Accepts` | 列出与本协议之间存在**直接**路径的协议：请求可以从这些协议转入，响应可以转出为这些协议。对自身恒为 true。 |
| `NewExchange` | 为一次执行创建状态。响应转换为 `opts.Output`（为空时即客户端协议）。从 `opts.Ext` 读取本协议包定义的选项；遇到不认识的选项类型时返回 `ClassUnsupported`。 |

错误以 `*apix.Error` 值作为 `error` 返回；成功时返回字面量 `nil`：存进 `error` 的 nil `*apix.Error` 并不等于 nil。

两个可选扩展会接入路由：

- Codec 实现 `apix.OptionsValidator`：`ValidateOptions(opts)` 让路由器在配置端点时拒绝错误的 `Ext`、损耗策略或推理覆盖。`NewExchange` 仍需自行校验。
- Exchange 实现 `apix.StateReporter`：`CreatedState()` 返回上游存储的状态的亲和键，例如 `response:<id>`，使后续请求回到同一端点。

`Exchange` 的要求：

- `ConvertRequest(req, upstreamModel)`：`upstreamModel` 为空时沿用客户端的模型名。
- `ConvertResponse(status, resp)`：遇到错误状态码（≥ 400）时，返回已经分类的上游错误，网关据此判断是否切换候选。
- `NewStreamConverter()`：不支持流式时返回 `ClassUnsupported`；Spec 应同时声明 `Stream: apix.FramingNone`。
- 流式转换器的输出是字节事件：SSE 协议输出完整的 SSE 帧，二进制流（如音频）直接输出数据块。

## 示例：图像 Kind

`apix.KindImage` 就是这样实现的，可以作为 `internal/llm` 之外新增 Kind 的完整示例。具体行为见[图像协议转换](protocols/image.zh-CN.md)。

- `internal/image/codec.go`：四个图像协议共用一个 `Codec` 类型，路径、鉴权、错误信封、流式与可接受协议等差异是 `protocols` 表中的行。错误信封来自 `internal/vendorerr`，与 LLM codec 共用。
- `openai.go`、`gemini.go`、`imagen.go`：各线格式的请求解码（JSON 与 multipart 表单）、响应类型和原生请求改写。
- `exchange.go`、`response.go`：直连转换，每个源与目标一个函数，不经过中间表示。`stream.go` 校验原生流；跨协议的流在 `ConvertResponse` 中模拟。
- `protocol/openai/images.go`、`protocol/gemini/images.go` 导出 ID 和构造函数，各供应商的 `Codecs()` 已包含它们。`gemini.GenerateContentImageOptions` 由传给 `image.New` 的 `ExtFunc` 转换为 `image.Options`，与 LLM codec 的做法一致。

```go
func (c *Codec) Spec() apix.ProtocolSpec {
    spec := apix.ProtocolSpec{ID: c.id, Kind: apix.KindImage, Method: "POST", Auth: c.proto.auth, Stream: c.proto.stream}
    // Path 与 ParsePath 取决于协议族。
    return spec
}

// 只声明已经实现的直连路径。
func (c *Codec) Accepts(p apix.Protocol) bool { return p == c.id || slices.Contains(c.proto.accepts, p) }
```

注册以后，图像请求按与 LLM 相同的规则路由：先选同协议的图像端点，再选可转换的图像端点。LLM 端点属于不同的 Kind，永远不会被选中。

```go
reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), gemini.Codecs())...)
```

## 测试要求

- **Exchange 表驱动测试**：覆盖每个 `Accepts` 方向的请求、响应和流（如果支持流式）。
- **快照**：先确认输出符合预期，再加入快照。
- **异常上游输出**：覆盖字段类型错误、截断的流、EOF 前没有终止事件等情况。
- **gateway 集成测试**：至少一个用 `httptest` 模拟上游的用例，验证 `Spec` 的路径、鉴权和 `EncodeError`。
- **依赖**：保持仅依赖标准库。
