# 网关

[English](gateway.md) | 简体中文 · [返回 README](../README.zh-CN.md)

`gateway` 是可选的执行器，通过 HTTP 跑完整个流程：解析、路由、转换、发送、转换回来，并在失败时切换。它是本模块中唯一发起网络请求的包。它遵循的路由决策见[路由](routing.zh-CN.md)。

## 配置上游

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

`Upstream` 由一个 `route.Endpoint` 加上访问它所需的信息组成：

| 字段 | 作用 |
| --- | --- |
| `BaseURL` | 协议、主机及可选的路径前缀，协议的请求路径拼接在其后 |
| `Path` | 替换协议的默认路径。路径随模型或流式模式变化的协议（如 GenerateContent）会拒绝它，前缀应放进 `BaseURL` |
| `APIKey`、`Auth` | 密钥，按 `Auth` 或协议默认鉴权方案发送 |
| `Header` | 发往该上游的每个请求都附加的请求头 |

`Handler(p)` 服务使用协议 `p` 的客户端：读取请求体、调用 `Do`、写出已提交的响应，每次读取后都 flush，让流式事件及时到达客户端。提交点之前的错误按 `p` 的错误格式返回；提交点之后的失败会提前结束响应。需要自己处理 HTTP 层的调用方可以直接用 `gateway.Request` 调用 `Do`。

## 配置项

| `gateway.Config` 字段 | 作用 |
| --- | --- |
| `Client` | 发送上游请求的 `http.Client`。默认客户端没有超时，以免截断流，且只在原始源内跟随重定向；见[重定向](#重定向) |
| `MaxAttempts` | 单个请求最多尝试的候选数；0 表示尝试全部 |
| `CommitTimeout` | 单次尝试在限定时间内未到达提交点时取消并切换；0 表示不限 |
| `MaxRequestBytes` | 限制 `Handler` 读取的请求体大小（默认 32 MiB） |
| `MaxResponseBytes` | 限制普通响应体大小（默认 64 MiB），超出时切换 |
| `MaxStreamFrameBytes` | 限制单个上游 SSE 帧（默认 64 MiB），不限制流的总长度；见[流式帧上限](#流式帧上限) |
| `ForwardHeaders` | 透传给客户端的上游响应头，如 `"X-Request-Id"`、`"X-Ratelimit-*"` |
| `ReasoningSuffix` | 按去掉后缀的模型名路由 `model(high)`，并把后缀作为 `Request.Reasoning`。模型 ID 可能本身带括号，因此默认关闭 |
| `RouterOptions` | 底层 `route.Router` 的选项，如 `route.WithStrategy`、`route.WithAffinity` |
| `OnAttempt` | 观察每次尝试，用于日志和指标 |
| `OnUsage` | 接收已提交尝试的 [Token 用量](../README.zh-CN.md#token-用量)；见[状态与用量](#状态与用量) |

## 提交点与故障切换

`Do` 按路由器返回的顺序依次尝试候选，并在提交点返回：

- 普通响应：转换成功之后；
- 流式响应：拿到第一批转换后的事件之后。

提交点之前：

| 情况 | 处理 |
| --- | --- |
| 请求转换失败（如被损耗策略拒绝） | 换下一个候选；请求未发出，不计入健康统计 |
| 端点处于半开状态且已有试探请求，`Router.Begin` 拒绝 | 换下一个候选；请求未发出 |
| 网络错误、提交超时、401、402、403、429、5xx 等 `Retryable` 错误 | 回报健康统计，换下一个候选 |
| 404，归类为 `ClassInvalidModel` | 换下一个候选；会回报，但不计入端点失败 |
| 400 等不可重试错误 | 停止，按客户端协议返回该错误 |
| 2xx 但响应或流无法解码、转换，或超过 `MaxResponseBytes` | 重新归类为可重试的 `ClassUpstream` 错误，换下一个候选 |
| 请求被取消 | 立即停止，不计入健康统计；错误满足 `errors.Is(err, context.Canceled)` |

404 可重试，因为各上游的模型目录和已存储状态并不相同。它反映的是请求而不是端点，所以熔断器忽略它，见[健康与熔断](routing.zh-CN.md#健康与熔断)。`CommitTimeout` 到期时取消该尝试，按可重试的网络错误处理；已经提交的流不会被它截断。

提交点之后的失败不再重试，因为客户端可能已经收到部分输出。这类失败通过 `Response.Body.Read` 返回 `*apix.Error`。流结束、失败或被关闭时只上报一次健康结果，只有完整完成才算成功。调用方取消和提前 `Body.Close` 属于未确定结果：会释放半开试探名额，但不清除既有失败计数，也不惩罚端点。取消错误通过 `errors.Is` 保留 `context.Canceled` 或 `context.DeadlineExceeded`，以及自定义取消原因。

## 请求与请求头

鉴权头按协议默认方案注入：OpenAI 用 `Authorization: Bearer <key>`，Anthropic 用 `x-api-key: <key>` 加 `anthropic-version`，Gemini 用 `x-goog-api-key`。`Upstream.Auth` 可以覆盖默认方案。上游路径由转换后 Payload 的元信息生成，URL 中的模型和流式模式总与 Exchange 的输出一致。请求头按以下顺序叠加，后者覆盖前者：

1. 协议固定头；
2. `Request.Header`；
3. `Upstream.Header`；
4. 鉴权头。

客户端的凭据不会被转发。上游响应头只有列在 `Config.ForwardHeaders` 中时才会传给客户端（末尾的 `*` 表示前缀匹配），`Content-Type`、`Content-Length` 等描述报文的头永远不会复制；`Do` 的调用方可以从 `Response.UpstreamHeader` 读取全部响应头。上游的 `Retry-After` 会写入错误的 `RetryAfter`，让该端点暂停接收流量，请求失败时由 `Handler` 转告客户端。

`Request.Output` 把响应转换为同 Kind 的另一种协议，为空时即请求协议。路由仍然优先选择与请求协议相同的端点，因为请求是否需要转换取决于请求协议。`Request.Reasoning` 会为每个上游覆盖推理控制；开启 `Config.ReasoningSuffix` 后，`Do` 从 `model(high)` 后缀生成它，并按去掉后缀的模型名路由。

## 随模型变化的路径

Gemini 的 GenerateContent、GenerateContentImage 和 countTokens 协议把模型（生成协议还有流式模式）放在 URL 而不是请求体中。它们的 `ProtocolSpec.ParsePath` 把 `/v1beta/models/{model}:{action}`（也接受 `/v1/models/` 前缀）解析到 `Payload.Metadata`：生成协议的 action 为 `generateContent` 或 `streamGenerateContent`，计数协议为 `countTokens`，其他 action 会被拒绝。`Handler` 会自动调用它。这些 Handler 应挂载在 `/v1beta/models/` 下。生成与计数共用这个前缀，需要先按操作分发再选择 Handler，示例见 [Token 计数](protocols/tokens.zh-CN.md#网关配置)。

发往上游时，路径由转换后 Payload 的元信息生成：`/v1beta/models/{upstreamModel}:generateContent`，流式为 `:streamGenerateContent?alt=sse`。`Upstream.Path` 不能替换随模型或流式模式变化的路径，前缀应放进 `BaseURL`。不经网关直接转换 GenerateContent 请求体的调用方需要自行传入元信息，见 [GenerateContent URL](protocols/llm.zh-CN.md#generatecontent-url)。

## 流式帧上限

`Config.MaxStreamFrameBytes` 默认把单个上游 SSE 帧限制为 64 MiB，计数包含注释和线路上的换行分隔符，不含 EOF 时补入的分隔符，也不限制整条流的长度。`route.EndpointOptions.MaxStreamFrameBytes` 可按端点覆盖；直接使用 Codec 时设置 `apix.ExchangeOptions.MaxStreamFrameBytes`。负值显式为可信上游关闭限制；较大的图像帧应显式提高上限。

帧超限在提交点之前触发切换，之后变为终态读取错误，并保留之前的有效事件；错误满足 `errors.Is(err, sse.ErrFrameTooLarge)`。

## 重定向

默认 HTTP 客户端的重定向链最多包含十次请求，且只允许原始协议、主机名和有效端口。跨源重定向（包括 HTTPS 降级）会在其他主机收到凭据前失败。自带 `Config.Client` 时，调用方负责其重定向策略，尤其要保护自定义鉴权头。

## 状态与用量

会报告新建状态的 Exchange（如存储的 Responses 或 Interactions）在提交点和流结束时把状态交给 `Router.Remember`，后续轮次因此回到同一端点。见[上游持有的状态](routing.zh-CN.md#上游持有的状态)。

`Config.OnUsage` 只在上游报告了用量时调用。普通响应在 `Do` 返回时调用；流式响应在 EOF、失败或 `Body.Close` 中最先发生的时刻调用一次，提前结束时为已观察到的用量。`Handler` 同样适用，`Response.Usage()` 返回同一结果；流式响应应在读完或关闭响应体之后再调用。

## 热更新

`Gateway.Update(upstreams)` 按 [`Router.Update`](routing.zh-CN.md#热更新) 的规则原子地替换上游集合。进行中的请求在故障切换时继续使用开始时的 URL、凭据和协议配置。网关还会复制上游请求头和鉴权方案。修改网关配置应使用 `Gateway.Update`；`Gateway.Router()` 用于查询路由、报告健康状态和调用 `Remember`。
