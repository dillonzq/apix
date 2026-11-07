# 路由

[English](routing.md) | 简体中文 · [返回 README](../README.zh-CN.md)

`route` 只负责决定请求依次尝试哪些端点，不做任何 I/O。`gateway` 按这个顺序执行请求，并把每次尝试的结果回报给路由器；它的故障切换规则见[网关](gateway.zh-CN.md)。

## 端点

```go
route.Endpoint{
    ID:          "anthropic-main",     // 唯一；健康状态按 ID 记录
    Provider:    "anthropic",          // 日志与指标用的标签
    Protocol:    anthropic.Messages,   // 端点使用的协议
    Models:      map[string]string{"smart": "claude-sonnet-4-5", "*": ""},
    Priority:    0,                    // 越小越先尝试
    Weight:      3,                    // 同优先级内的加权随机
    Unsupported: []string{apix.FeatureAudioInput},
    Options:     route.EndpointOptions{LossPolicy: apix.LossPolicySafe},
}
```

各字段的规则：

- **Models**：把客户端模型名或别名映射到上游模型名。
  - 精确匹配优先；没有精确匹配时，用 `"*"` 匹配任意模型。
  - 映射值为空表示沿用客户端模型名。
  - Models 为空表示服务所有模型，且模型名不变。
- **Unsupported**：列出端点不支持的能力，需要其中任一能力的请求会跳过该端点。请求需要的能力由 Codec 的 `Inspect` 从已知协议字段提取：`stream`、`tools`、`image-input`、`file-input`、`audio-input`、`video-input`、`image-output`、`audio-output`、`video-output`、`structured-output` 和 `web-search`（`apix.Feature*` 常量）。不会扫描工具 schema、参数或结果。未列出的能力视为支持，所以以后版本新识别的能力不会把现有端点排除在外。
- **Options**：端点的 Exchange 选项，包括损耗策略和协议包定义的 `Ext`，例如 `anthropic.MessagesOptions`。`New` 和 `Update` 会通过 `Registry.ValidateOptions` 按端点的 Codec 校验它们，配置错误在配置阶段就会报错，而不是每次请求都被静默跳过。响应协议和推理覆盖属于每个请求，不属于端点。
- **Disabled**：端点不参与路由。

## 排序规则

`Router.Route(info)` 先过滤，再排序。

保留同时满足以下条件的端点：

1. 未禁用；
2. 协议与请求属于同一 Kind；
3. 能服务请求的模型；
4. 支持请求需要的全部能力；
5. `Registry.CanConvert(请求协议, 端点协议)` 为真；
6. 请求为 `Stateful` 时，端点使用客户端协议。

默认策略 `NativeFirst` 按以下顺序排列：

1. **同协议在前**：端点协议与客户端相同（`Candidate.Native`），保留请求协议，按需改写模型，并应用显式配置的覆盖或规范化。
2. **可转换的在后**：同 Kind 的其他协议，走直接转换路径。
3. **层内按优先级**：`Priority` 小者在前。
4. **同优先级按权重**：按 `Weight` 加权随机、不放回地打散。权重小于 1 视为 1。

同协议端点总是排在可转换端点之前，与 `Priority` 无关。如果需要别的顺序，可以用 `route.WithStrategy` 换成自定义的 `Strategy`；亲和性在策略之后生效。

找不到候选时：

- 没有任何端点能服务该模型，返回 `ClassInvalidModel`；
- 有端点能服务该模型，但都不能接收这个请求，返回 `ClassUnsupported`。

## 上游持有的状态

`Inspect` 在 `RequestInfo.Affinity` 中列出请求引用的上游状态，键的形式如 `response:resp_1`、`conversation:conv_1`、`interaction:i1`、`file:file-1`、`cached-content:cachedContents/c1`。引用了转换无法携带的上下文时（Responses 的 `previous_response_id`、`conversation`、`prompt`、`item_reference`，Interactions 的 `previous_interaction_id`，Messages 的 `container`，Google 的 `cachedContent`），还会设置 `RequestInfo.Stateful`。

- **只走同协议**：`Stateful` 请求只发往客户端协议的端点，转换会悄悄丢掉上下文。
- **持有者优先**：`route.Affinity` 记录每个键属于哪个端点。第一个已知键的持有端点排到最前；对 `Stateful` 请求，即使该端点不健康也会返回，因为其他端点无法服务。
- **记录持有者**：实现了 `apix.StateReporter` 的 Exchange 会报告上游新建的状态，网关把它们交给 `Router.Remember`。原生 Responses 报告已存储的 response ID，Interactions 报告 interaction ID。在别处创建的状态（如上传的文件）可以自行调用 `Router.Remember`。

默认的 `AffinityCache` 在进程内保留最近使用的 65536 个键。多实例共享时用 `route.WithAffinity` 替换；传入 `nil` 可关闭亲和性。

## 健康与熔断

`Router.Report(id, Outcome)` 记录一次尝试的结果。默认的 `Breaker` 规则如下：

- **熔断**：连续失败达到 `Threshold`（默认 3）次后，端点在 `Cooldown`（默认 30 秒）内被跳过。失败带有 `apix.Error.RetryAfter`（来自上游 `Retry-After` 头）时，即使未达阈值，也至少暂停这么久。
- **半开**：暂停结束后，端点每次只接收一个试探请求。执行器在发送前调用 `Router.Begin(candidate)`，它放行试探请求并拒绝并发的其他请求。成功一次即完全恢复，再失败一次则重新熔断。如果试探结果一直没有回报，名额在 `Cooldown` 后释放。
- **计入失败的错误**：鉴权、限流、配额、计费、上游服务端错误、网络错误。模型不存在、转换失败和不支持的请求属于请求本身的问题（由客户端控制），不计入；客户端无法通过向 `"*"` 映射发送错误模型名，把健康端点挤出轮换。
- **全部不健康时**：路由器仍按正常顺序返回这些端点，不会因为健康统计而直接拒绝服务，`Begin` 也会放行。

健康状态只保存在进程内。多实例部署时，可以实现 `route.Health` 接口接入共享存储，需要试探限流时再实现可选的 `route.Prober`。

## 热更新

`Router.Update(endpoints)` 原子地替换端点集合：

- 正在路由的请求继续使用旧集合；
- 更新校验失败时（包括 Codec 拒绝的选项），当前配置保持不变；
- 健康状态按端点 ID 保留；
- 路由器会复制传入的 map 和切片，调用方之后修改它们不影响路由器。

`Gateway.Update` 对网关上游适用同样的规则，见[网关](gateway.zh-CN.md#热更新)。

## 执行候选

执行器按顺序尝试候选：

1. 发送前调用 `Router.Begin(candidate)`；返回 false 时（已有半开试探请求在途）跳过该候选；
2. 按 `Candidate.UpstreamModel` 和 `Endpoint.Options` 把请求转换为 `Endpoint.Protocol`，然后发送；
3. 对每次已发出的尝试调用 `Router.Report(endpointID, Outcome)`，供健康统计使用；
4. 对上游新建的状态调用 `Router.Remember(key, endpointID)`。

`gateway` 包完成上述全部步骤，并决定哪些失败切换到下一个候选，见[网关](gateway.zh-CN.md#提交点与故障切换)。自行实现的执行器遵循同样的步骤。
