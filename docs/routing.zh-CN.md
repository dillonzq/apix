# 路由与故障切换

[English](routing.md) | 简体中文 · [返回 README](../README.zh-CN.md)

`route` 只负责决定请求依次尝试哪些端点，不做任何 I/O。`gateway` 按这个顺序执行请求，并把每次尝试的结果回报给路由器。

## 端点

```go
route.Endpoint{
    ID:       "anthropic-main",     // 唯一；健康状态按 ID 记录
    Provider: "anthropic",          // 日志与指标用的标签
    Protocol: anthropic.Messages,   // 端点使用的协议
    Models:   map[string]string{"smart": "claude-sonnet-4-5", "*": ""},
    Priority: 0,                    // 越小越先尝试
    Weight:   3,                    // 同优先级内的加权随机
    Features: []string{apix.FeatureStream, apix.FeatureTools},
    Options:  apix.ExchangeOptions{LossPolicy: apix.LossPolicySafe},
}
```

各字段的规则：

- **Models**：把客户端模型名或别名映射到上游模型名。
  - 精确匹配优先；没有精确匹配时，用 `"*"` 匹配任意模型。
  - 映射值为空表示沿用客户端模型名。
  - Models 为空表示服务所有模型，且模型名不变。
- **Features**：列出端点支持的能力；为空表示不限制。请求需要的能力由 Codec 的 `Inspect` 从已知协议字段提取：`stream`、`tools`、`image-input`、`file-input`、`audio-input`、`video-input`、`image-output`、`audio-output`、`video-output`、`structured-output` 和 `web-search`（`apix.Feature*` 常量）。不会扫描工具 schema、参数或结果。列表非空的端点必须声明其请求用到的全部能力。
- **Options**：每次创建 Exchange 时传给上游 Codec，例如 `anthropic.MessagesOptions`。
- **Disabled**：端点不参与路由。

## 排序规则

`Router.Route(info)` 先过滤，再排序。

保留同时满足以下条件的端点：

1. 未禁用；
2. 协议与请求属于同一 Kind；
3. 能服务请求的模型；
4. 支持请求需要的全部能力；
5. `Registry.CanConvert(请求协议, 端点协议)` 为真。

默认策略 `NativeFirst` 按以下顺序排列：

1. **同协议在前**：端点协议与客户端相同（`Candidate.Native`），无需转换，只改写模型。
2. **可转换的在后**：同 Kind 的其他协议，走直接转换路径。
3. **层内按优先级**：`Priority` 小者在前。
4. **同优先级按权重**：按 `Weight` 加权随机、不放回地打散。权重小于 1 视为 1。

同协议端点总是排在可转换端点之前，与 `Priority` 无关。如果需要别的顺序，可以用 `route.WithStrategy` 换成自定义的 `Strategy`。

找不到候选时：

- 没有任何端点能服务该模型，返回 `ClassInvalidModel`；
- 有端点能服务该模型，但都不能接收这个请求，返回 `ClassUnsupported`。

## 健康与熔断

`Router.Report(id, Outcome)` 记录一次尝试的结果。默认的 `Breaker` 规则如下：

- **熔断**：连续失败达到 `Threshold`（默认 3）次后，端点在 `Cooldown`（默认 30 秒）内被跳过。
- **半开**：冷却结束后端点恢复接收流量；这时再失败一次就会重新熔断，成功一次则完全恢复。
- **计入失败的错误**：鉴权、模型不存在、限流、配额、计费、上游服务端错误、网络错误。转换失败和不支持的请求属于请求本身的问题，不计入。
- **全部熔断时**：路由器仍按正常顺序返回这些端点，不会因为健康统计而直接拒绝服务。

健康状态只保存在进程内。多实例部署时，可以实现 `route.Health` 接口，接入共享存储。

## 热更新

`Router.Update(endpoints)`（经网关则用 `Gateway.Update(upstreams)`）原子地替换端点集合：

- 正在路由的请求继续使用旧集合；
- 网关请求在故障切换时继续使用同一批 URL、凭据和协议配置；
- 更新校验失败时，当前配置保持不变；
- 健康状态按端点 ID 保留；
- 路由器会复制传入的 map 和切片，调用方之后修改它们不影响路由器。

网关还会复制上游 Header 和鉴权方案。修改网关配置应使用 `Gateway.Update`；`Gateway.Router()` 用于查询路由和报告健康状态。

## 网关的执行规则

`gateway.Do` 依次尝试候选，`MaxAttempts` 为 0 时尝试全部候选。

**提交点**：普通响应转换成功之后，或流式响应产出第一批事件之后，`Do` 才返回。

提交点之前：

| 情况 | 处理 |
| --- | --- |
| 请求转换失败（如被损耗策略拒绝） | 换下一个候选；请求未发出，不计入健康统计 |
| 网络错误、429、5xx 等 `Retryable` 错误 | 记录失败，换下一个候选 |
| 400 等不可重试错误 | 停止，按客户端协议返回该错误 |
| 2xx 但响应或流无法解码、转换 | 视为可重试的上游错误，换下一个候选 |
| 请求被取消 | 立即停止，不计入健康统计 |

提交点之后的失败不再重试，因为客户端可能已经收到部分输出。这类失败通过 `Response.Body.Read` 返回 `*apix.Error`，并计入健康统计。

鉴权头按协议默认方案注入：OpenAI 用 `Authorization: Bearer <key>`，Anthropic 用 `x-api-key: <key>` 加 `anthropic-version`。端点可以用 `Upstream.Auth` 覆盖默认方案，用 `Path` 覆盖默认路径。请求头的叠加顺序是：

1. 协议固定头；
2. `Request.Header`；
3. `Upstream.Header`；
4. 鉴权头。

客户端的凭据不会被转发。

`Request.Output` 把响应转换为同 Kind 的另一种协议，为空时即请求协议。路由仍然优先选择与请求协议相同的端点，因为请求是否需要转换取决于请求协议。
