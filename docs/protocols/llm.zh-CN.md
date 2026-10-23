# LLM 协议转换说明

[English](llm.md) | 简体中文 · [返回 README](../../README.zh-CN.md)

本文件说明当前实现的映射和限制。覆盖 `apix.KindLLM` 下的 `openai.chat-completions`、`openai.responses` 和 `anthropic.messages`，三者支持请求、普通响应及流式响应的全部六个跨协议方向；协议 ID 和接入示例见 README。

GenerateContent 与原生 Interactions 另见 [Gemini 协议](gemini.zh-CN.md)。

## 配置

LLM 协议实现支持媒体输入、本地 shell 适配、缓存提示保留和推理历史回传：

- 原生 Responses 工具规范化使用 Exchange 选项 `openai.ResponsesOptions{NormalizeTools: true}`；
- 无签名 thinking 回传使用 `anthropic.MessagesOptions{AllowUnsignedThinking: true}`；
- 推理覆盖使用 `apix.ExchangeOptions.Reasoning`；
- SSE 解码使用 `sse.Decoder`。

`Inspect` 还会报告请求引用的上游状态（`RequestInfo.Affinity`，如 `response:resp_1`、`file:file-1`），以及请求是否延续上游持有的上下文（`RequestInfo.Stateful`）。原生 Responses Exchange 通过 `apix.StateReporter` 报告已存储的 response ID。详见[路由](../routing.zh-CN.md#上游持有的状态)。

## 原生路径与职责

同协议请求仅改写 `model`，保留历史 reasoning、compaction、角色、工具及未知字段。模型不变时返回原始字节。库不按模型名称自动清理字段；模型目录、路由和上游兼容规则由调用方负责。

同协议响应通常原样透传。Responses 工具身份恢复，以及流式错误、终止校验和 SSE 包装是例外。原生工具恢复保留 namespace 与未建模的数字字段。

## 请求映射

| 内容 | 当前行为 |
| --- | --- |
| system/developer/instructions | 转成目标协议 system/instructions；Messages 还会合并 user/tool_result 与连续 assistant 内容；历史消息边界不保证原样保留 |
| token 限制 | `max_tokens/max_completion_tokens/max_output_tokens` 映射；Messages 必须有正数限制，缺省或非正数默认 4096；Responses → Chat Completions 的非正数 max_output_tokens 被省略 |
| stop/stop_sequences | Chat Completions ↔ Messages 保留；转 Responses 丢弃 |
| 空内容、内容类型 | 空文本块/无有效内容的消息通常被移除；跨协议未知输入类型、system 图片、无法表示的工具结果会明确报错，不会统一静默删除 |
| tool choice / parallel | 转换 auto/none/required/指定工具；Messages 的并行开关映射为 disable_parallel_tool_use；无法表达的组合报错 |
| 采样参数 | 跨协议映射 `temperature`、`top_p`；未映射的顶层字段（如 `n`、`seed`、penalties、`metadata`、`store`、`service_tier`、`previous_response_id`）会报告已知损耗 |

## 结构化输出

JSON Schema 控制在以下字段之间转换：

| 协议 | 字段 |
| --- | --- |
| Chat Completions | `response_format.json_schema` |
| Responses | `text.format` |
| Messages | `output_config.format` |

Chat 与 Responses 可转换 `json_object`；Messages 需要 `json_schema`，无法表示无 schema 的 `json_object`，会报错。模型是否支持目标 schema 由调用方判断。

Messages 的输出格式不携带 OpenAI 的 schema 名称、描述及可选 strictness：名称和描述的损耗会记录为展示信息，显式 `strict: false` 的映射会记录语义变化。函数工具的 `strict` 在转换中保留，包括显式 `false`。

## 工具身份与搜索

Responses 的 `additional_tools` 合并到工具声明；namespace 展平为函数名称，长名称通过截断和摘要控制长度。所有普通与展平名称统一检查碰撞，响应通过请求状态恢复原名称、namespace 和 custom 身份。

Custom 工具（包括 `apply_patch`）可转换为使用字符串 `input` 的函数工具；缺省 schema 包含必需的 `input`。Responses 输出恢复 custom 调用，涵盖 item、input delta/done 和终止快照。custom grammar 等无法映射的约束会报告损耗。

### 本地 shell 适配

此适配将 Responses 客户端的本地 shell 转成 Chat Completions 或 Messages 上游的函数工具；Responses 上游需使用 `openai.ResponsesOptions{NormalizeTools: true}`。向 Responses 客户端返回响应时，同一个 `Exchange` 会恢复原始工具。

本地 `shell` 声明（省略 environment 或指定 `type: "local"`，包括 `{"type":"local","skills":[]}`）转成名为 `shell` 的函数。参数包含必需且非空的 `commands` 数组，以及可选、可为 null 的 `timeout_ms` 和 `max_output_length` 限制。支持省略或空数组形式的本地 `skills`；null 及其他非数组值会被拒绝，非空 skills 和其他无法表示的本地配置会明确报错。shell action 只接受 `commands`、`timeout_ms` 和 `max_output_length`，其他属性会被拒绝。本地 shell 工具不能放在 namespace 中，也不能与名为 `shell` 的普通函数/custom 工具同时声明。历史中的无 namespace、名为 `shell` 的 `custom_tool_call` 同样构成冲突，会在登记 shell 身份前被拒绝；带 namespace 的同名 custom 调用仍保持独立。

历史 `shell_call` 转成函数调用。`shell_call_output` 转成 JSON 文本，保留输出顺序、stdout、stderr、退出码和超时结果。若存在 `max_output_length`，文本包含带 `output` 和 `max_output_length` 的对象；否则包含输出数组。Responses 跨协议请求仅在整个转换成功后提交工具身份，失败时仍保留诊断信息。

恢复的调用包含稳定的 item `id`、独立的 `call_id`、`environment: {"type":"local"}` 和 status。原生流按 `call_id` 保留首次 item ID，后续快照省略 `id` 时仍复用该 ID；原生 status 会保留。恢复前会拒绝缺失或空白的 call ID；缺省 status 在 added 事件中补为 `in_progress`，在完成的 item 中补为 `completed`。action 中省略的执行限制均补为 `null`。

流式输出先宣布 shell item，再在 `response.output_item.done` 和终止快照中返回完整 action，保持相同的 item ID。shell 函数参数的 delta/done 事件被抑制。先处理协议级上游失败，再校验参数，保留上游错误分类及可重试性；成功响应中的无效或不完整参数会报错。

本库不执行命令。托管 shell 环境由原生 Responses 路径保留，跨协议转换不支持；旧版 `local_shell` 不在此映射范围内。实际的原生 `shell_call` 输出不会映射给 Chat Completions 或 Messages 客户端。显式规范化及原生 Responses 恢复保留精确 JSON 数值；既有跨协议路径若将参数解码为浮点数（包括 Messages 请求历史），可能舍入大整数。

### 托管搜索

| 搜索路径 | 支持范围 |
| --- | --- |
| Responses ↔ Messages | 可表示的 web search 声明、查询、位置、允许域名、URL 来源和完成信息 |
| 搜索 → Chat Completions | 无托管搜索执行表示；执行信息的损耗会记录，强制搜索选择无法表示时会报错 |
| 原生请求 | 保留搜索声明和选项，不执行跨协议规范化 |

`tool_search`、`image_generation` 等无法表示的请求工具会报告损耗。namespace 内的托管搜索不作为普通函数展开。Messages 搜索结果的加密内容及不可映射的搜索选项也会报告损耗。

Messages 查询缺少对应搜索结果时，普通与流式转换均将搜索 item 标为 `failed` 并报告缺失结果。失败搜索不发送成功的 `response.web_search_call.completed`；空数组结果可以正常完成。搜索 item 的失败不等于整条响应失败，外层完成状态仍按原协议终止原因转换。

## 推理响应

普通响应与流式响应均覆盖以下六个方向：

| 上游 → 客户端 | 保留方式 |
| --- | --- |
| Chat Completions → Messages | 可读推理映射为 `thinking` 块 / `thinking_delta` |
| Chat Completions → Responses | 可读推理保存在 `reasoning.content[]`（`type: reasoning_text`），同时提供 `summary[]` 展示副本和对应 summary 流式事件 |
| Messages → Chat Completions | `thinking` 文本合并到 `message.reasoning_content` / `delta.reasoning_content` |
| Messages → Responses | 每个 `thinking` 块映射为独立 reasoning item，包含回传 content 和展示 summary |
| Responses → Chat Completions | reasoning item 的 `summary_text` 与可读 `content`（`reasoning_text` / `text`）合并到 `reasoning_content` |
| Responses → Messages | 同样的可读推理映射为 `thinking` 块 / `thinking_delta` |

Chat Completions 按 `reasoning_content` → `reasoning` → `reasoning_details` 的顺序取第一个包含可读文本的字段，支持字符串、数组和带 `text` 的对象。多个别名不会重复拼接。文本保留空白和 Unicode；不会混进最终回答，也不会生成不存在的推理。Chat Completions 的多个推理片段在 Responses 终止结果中聚合为一个 reasoning item；Messages 的多个 thinking 块分别保留为 reasoning items。Chat Completions 输出本身无法保留正文、推理和多个块之间的原始排列关系。

Responses 的流式转换接收 `response.reasoning_summary_text.delta/done` 与 `response.reasoning_text.delta/done`。`output_item` 和终止响应中的 reasoning 快照补充未通过 delta 发送的文本；相同 part 已发送的前缀不会重复输出。合成 Responses 流包含 item/summary 的开始、增量和结束事件，终止 output 的顺序、索引与 item 公告一致。

跨协议仍不传递 Anthropic `signature` / `signature_delta` / `redacted_thinking.data`，以及 Responses `encrypted_content`。这些是供应商专属的签名或不透明数据，没有可靠的跨协议映射；不会伪造签名，也不会当成可读推理。转换出的无签名 thinking 适合展示，不保证可直接回放到要求有效签名的 Anthropic 请求。

相同协议的响应通常原样透传，保留这些字段；Responses 的工具身份恢复和流式错误/终止处理是例外。

## 推理控制与后缀

`reasoning_effort` 与 `reasoning.effort` 相互映射。档位转向 Messages 时默认使用 adaptive：`thinking: {"type": "adaptive"}`，档位写入 `output_config.effort`，并与结构化输出格式合并。`none` 关闭 thinking，`auto` 不设置。不支持 adaptive 的旧 Claude 模型可传入 `anthropic.MessagesOptions{ThinkingMode: anthropic.ThinkingManual}`，改为发送 `{"type": "enabled", "budget_tokens": N}`。原生 Messages 推理控制原样透传。手动模式与反向转换使用固定 effort/budget 表：

| effort | budget |
| --- | --- |
| `none` | 0，关闭 |
| `auto` | -1，上游默认 |
| `minimal` | 512 |
| `low` | 1024 |
| `medium` | 8192 |
| `high` | 24576 |
| `xhigh` | 32768 |
| `max` | 128000 |

反向转换按阈值选择档位，不保证精确往返；正数 budget 超过 24576 时映射为 `xhigh`，不会自动得到 `max`。库不按模型能力过滤或限幅。

默认入口不解析尾部 `model(value)`。需要后缀优先时，把它作为推理覆盖交给 Exchange：

```go
import "github.com/dillonzq/apix/reasoning"

suffix := reasoning.ParseSuffix(requestedModel)
ex, err := reg.NewExchange(client, upstream, apix.ExchangeOptions{
    Reasoning: suffix.Reasoning(), // 没有后缀或后缀无法识别时为 nil
    Ext:       upstreamOptions,    // 例如 anthropic.MessagesOptions{ThinkingMode: anthropic.ThinkingManual}
})
out, err := ex.ConvertRequest(apix.Payload{Body: payload}, suffix.Model)
```

Exchange 会在转换据此推导目标推理控制的路径上剥离客户端自己的控制，完成转换后，再用与本次 Exchange 相同的选项渲染覆盖值，因此后缀与上游推理模式不会互相矛盾。`gateway.Config{ReasoningSuffix: true}` 会对每个请求完成上述步骤，并按去掉后缀的模型名路由。自行构造上游请求体的宿主仍可在转换前调用 `reasoning.StripSupersededReasoning`、转换后调用 `reasoning.ApplySuffixWith(upstream, body, suffix, reasoning.Options{...})`，其中 `MessagesManual`、`GeminiMode` 应与 Exchange 的设置一致。

识别到的后缀取代请求体中的推理控制；`none` / `0` 关闭，`auto` / `-1` 采用上游默认。Chat 与 Responses 得到 effort，数值后缀按上表阈值转换。Messages 遵循 Exchange 的推理模式：默认 adaptive 并写入 `output_config.effort`；使用 `anthropic.ThinkingManual` 时发送 enabled thinking，档位按表取预算，数值后缀保留精确预算。GenerateContent 遵循 `gemini.GenerateContentOptions.ThinkingMode`：budget 模式发送 `thinkingBudget`，level 模式发送 `thinkingLevel`，默认时数值后缀发送预算、档位发送档位。Gemini 没有高于 `high` 的档位，`xhigh` 与 `max` 会变为 `high`，并作为非语义请求损耗报告。

Messages 禁用时移除 `thinking.display`，并始终覆盖客户端的 `output_config.effort` 与 `budget_tokens`；启用时保留可用的 display。Responses 只覆盖 `reasoning.effort`，保留同级字段。

未知后缀仍可被解析并剥离，但不改变请求体推理参数，`Suffix.Reasoning` 对其返回 nil。`StripSupersededReasoning` 只应在后缀已识别时调用。真实包含括号的模型 ID 是否按字面名称路由，由调用方决定。

历史推理与推理控制分别映射。Chat assistant 的 `reasoning_content`（或可读的 `reasoning` / `reasoning_details` 别名）与 Messages assistant 的 `thinking` 保留为 Responses `content` 中的 `reasoning_text`。Responses 原始 content 映射回 Chat `reasoning_content`，思考、assistant 正文和随后并行工具调用归入同一消息；工具结果及 user/system 消息划分轮次。Messages thinking 转为原 assistant 消息的 Chat reasoning。Chat → Messages 和 Responses → Messages 默认省略无签名 thinking，nil 或零值状态也采用这一行为。仅在上游明确支持无签名 thinking 时，配合默认或 `LossPolicyAllow` 策略，显式传入 Exchange 选项 `anthropic.MessagesOptions{AllowUnsignedThinking: true}`。该选项只影响请求转换，不改变原生 Messages 历史或响应展示。

请求回传不会用 Responses summary 替代原始 content。只有 summary 的历史、与原文不同的 summary、加密/不透明内容、未知推理格式及签名丢失均报告为语义损失，safe/strict 策略拒绝转换。与已保留原文完全相同的展示 summary 不报告损失。完整但为空的 Chat reasoning 别名不会与可读别名冲突；不完整别名及非空冲突值仍报告损失。转入 Messages 时无法生成真实 Anthropic 签名，也会报告语义损失；即使启用 `AllowUnsignedThinking`，safe/strict 仍拒绝转换。无论默认省略还是显式允许发送，都记录 `unsigned_thinking` 诊断。同协议仍保留原字段。

合成的 Responses 普通响应、`response.output_item.done` 和终止 `response.output` 都保留原始 reasoning content，供下一次请求回传；summary 事件继续用于展示。再次转换时，相同的 summary/content 快照不会重复输出。这无法恢复客户端历史中已经丢失的原文，调用方也不能在转换后再次删除 reasoning。参见 [DeepSeek 思考回传要求](https://api-docs.deepseek.com/zh-cn/guides/thinking_mode/)及 [Anthropic thinking 签名](https://platform.claude.com/docs/en/docs/build-with-claude/extended-thinking)。

## 响应字段与 usage

| 内容 | 当前行为 |
| --- | --- |
| 文本、函数工具调用 | 转换并保留文本和参数；缺省参数补 `{}`；转 Messages 时解析参数，非法 JSON 报错 |
| 多候选 `choices` | 跨协议只处理第一个候选；同协议透传保留全部 |
| 多个消息 / 文本块 | Chat Completions 聚合正文；合成 Responses 聚合为一个 assistant message，定位在首次文本位置 |
| Chat Completions `message.refusal`、Responses message 内的 `refusal` part | 普通/流式响应保留。Chat Completions ↔ Responses 保留独立 refusal 字段/part；转 Messages 映射为可显示的 text。正常完成的拒绝映射为 content_filter/refusal，工具和截断信号仍优先 |
| 结束原因 | `stop` ↔ `end_turn`、`length` ↔ `max_tokens`、`tool_calls` ↔ `tool_use`、`content_filter` ↔ `refusal`。未知原因回退到普通结束；`stop_sequence` 的具体值不复制 |
| 工具调用与截断并存 | 转 Chat Completions / Messages 时实际工具调用优先于截断原因；转 Responses 时仍以 `length/max_tokens` 映射 `incomplete`，工具信号放在 output items 中 |
| 基础 usage | 输入/输出 token 映射；Chat Completions/Responses 的 total 按输入+输出重算；缺省 usage 合成零值 |
| 缓存提示 | Chat Completions / Responses → Messages 保留内容块和工具上的 `cache_control`（包括 `ttl` 等扩展字段）；系统与 assistant 文本有提示时保留块边界。消息/输入项级提示映射到该项最后生成的块，已有块级提示优先；Chat 工具外层提示优先于 function 内层。同协议原样保留，转向其他协议报告提示丢失，不自动添加缓存断点。工具结果只将原末尾子块的提示迁移到父级 `tool_result`，已有末尾提示优先于消息/项级提示；非末尾子块、reasoning 和 additional_tools 项的提示不可表示，按丢失策略省略或在 strict 模式拒绝。namespace 工具去重保留首次声明，被删除声明上的缓存提示按原始字段路径报告丢失。 |
| 缓存 usage | Messages 的 input 不含缓存，转 Chat Completions/Responses 时加上 cache read+creation；反向转 Messages 时扣掉可表示的缓存量并限制为非负。cache read 保留；cache creation 在三个协议间映射为 cache_creation_input_tokens / cache_write_tokens，缺省细项保持缺省，显式零值保留，不额外重复计入输入总量 |
| 推理 token usage | Chat Completions ↔ Responses 保留 `reasoning_tokens`；转 Messages 没有独立细项，仍计入输出总量 |
| 身份与时间 | 响应 id/model 保留；合成 message/reasoning item 的 id 由响应 id 派生。普通跨协议 Chat Completions 的 created 使用当前时间；Responses 流转 Chat Completions 可使用上游 created_at；其他未建模身份/时间字段不复制 |

Responses 的 `failed`、`cancelled` 或协议内 error 返回分类错误；普通跨协议响应的其他非终止状态也会报错。`incomplete` 映射为截断。合成 Responses 仅表达 completed/incomplete，不保留全部响应配置。

## 流式生命周期

公开入口接收任意网络分块，解析完整 SSE 帧，支持多 `data:` 行和 LF/CRLF/CR。输出为完整目标事件；Chat 包含一次 `[DONE]`，合成 Responses 事件带递增 `sequence_number`，原生序号保留。

工具身份完整后才公告；参数先到时缓存。转 Messages 时，未完成的工具会阻塞后续内容，按目标块的开始、增量、结束顺序输出，关闭后不再发送增量。工具 arguments 的 done、output item 和终止快照补齐未收到的尾部。

正文、refusal 和推理的 done/item/终止快照也可补齐遗漏文本；已发送前缀不重复输出。正文按 item、content index 和 part 类型跟踪。

`Finish` 校验有效终止状态。Chat 的 finish_reason / `[DONE]`、Messages 的 stop_reason / message_stop 可以建立结束状态；Responses 必须有有效终止事件。缺失终止信号或残留的无效载荷会报错。错误状态不可恢复，后续调用不生成成功终止。

转换会重建目标事件，SSE 的 comment、retry、id、ping 等信息不保证跨协议保留。

## URL 引用

支持 Chat `annotations`、Responses annotation 事件和 Messages `citations` 的 URL 引用转换，并保留可表示的来源、标题和文本范围。

Chat 的引用偏移只计入正文，不包含独立 refusal；Messages 将 refusal 渲染为可见文本。可表示的 Unicode 文本范围随内容聚合调整，交错内容无法保留的偏移会报告损耗。

转 Messages 时，提前收到的引用等待其引用范围实际输出完整后发送；分片不足时继续等待，不提前丢弃或计入去重。引用不会被推迟到后续排队块之后。真正晚到且对应块已关闭的引用会记录 `closed_block` 损耗，不重开旧块或挂到后文。

页码、文档位置等引用类型以及加密引用字段尚无完整映射，已知损耗会记录。

## 损耗与能力边界

`Exchange.Diagnostics()` 报告已知字段损耗。allow/safe/strict 的配置与行为见 README；safe/strict 只拒绝请求阶段的损耗。规则检查明确的协议字段，不递归分析工具参数、结果或 JSON Schema 中的业务数据。

Responses 的函数和自定义工具结果支持混合文本与图片。Responses → Messages 在 `tool_result.content` 中保留图文顺序，纯文本结果仍使用字符串。Responses → Chat 先输出连续的工具回复，再通过标注各 call ID 的额外用户消息承载混合结果；文本同时保留在工具回复及对应图片旁。空消息和 reasoning item 不会打断这组工具回复。

用户消息新增文件、音频和视频输入转换，参考 [CLIProxyAPI 官方实现](https://github.com/router-for-me/CLIProxyAPI/tree/091ff27/internal/translator)。

| 输入 | Chat Completions ↔ Responses | 与 Messages 的转换 |
| --- | --- | --- |
| 文件 | `file.file_id/file_data/filename` ↔ `input_file.file_id/file_data/filename` | PDF ↔ base64 文档源，纯文本 ↔ text 文档源；文件名映射到文档 `title` |
| 文件 URL | Responses 保留 `file_url`；转 Chat 时报错 | Responses `file_url` ↔ Messages URL 文档 |
| 音频 | `input_audio` 保留 `data`、`format`，也接受 Responses 顶层的 `data/format` | Messages 无音频输入块，报错 |
| 视频 | `video_url` ↔ `input_video`，支持 URL 字符串或对象，保留 `processing` | Messages 无视频输入块，报错 |

混合文本、图片和媒体按原顺序保留。视频同时提供顶层和嵌套 `processing` 时，顶层值优先。Messages 文本来源文档会编码为 `text/plain` 的 base64 文件；转换回 Messages 时解码并验证 UTF-8，生成 `source.type="text"`、`media_type="text/plain"`。只有 `application/pdf` 使用 base64 文档源；其他或未知 MIME（包括无 MIME 的原始 base64）转 Messages 时明确拒绝，不根据文件名推断类型。文档 `context` 和 `citations` 配置无法跨协议保留，会记录语义损耗，Safe/Strict 拒绝。文件 ID 只在两种 OpenAI 协议之间保留，转 Messages 时报错；调用方须确保 ID 对目标上游有效。

此支持针对用户输入；system、assistant 和无法表示的工具结果媒体会报错。转换器不读取本地文件、不下载 URL，也不上传文件；调用方负责提供文件内容、可访问的 URL 或有效 ID。空媒体、错误字段类型和无法解码的文档 base64 会报错。同协议请求仍保留所有原生字段，只改写模型。输入格式转换不代表目标模型支持该媒体或 MIME；音频、视频输出仍未支持。

请求可转换支持的图片 URL 和 base64 来源；system 图片及无法表示的工具结果会报错。响应中 Chat → Messages 可转换支持的图片块，Chat → Responses 的非文本输出及未支持的 Messages 内容块会报错。图像生成结果、音频和更多多模态输出尚不支持。未知 Responses item/part 用原始 JSON 检查实际载荷，包含 `result` / `action` 等未建模内容且无法表示时会报错，不静默生成成功的空结果。

logprobs、供应商扩展、历史 compaction 和签名不保证跨协议保留。原生保留字段也不意味着任何上游模型都接受这些内容；调用方负责能力验证。

## 实现与验证

供应商 Codec 委托给 `internal/llm/translate` 中的引擎，由引擎负责分派、请求策略和流式分帧。`internal/llm/adapter` 下的各协议 adapter 负责直接转换、上游事件分派和目标流式输出。共享协议类型、事件构造、工具身份和字段规则按职责分文件，位于 `internal/llm/adapter/shared`；推理档位表与覆盖渲染位于 `internal/llm/reasoning`，公开的 `reasoning` 包负责解析模型后缀。模块与状态约定见[代码结构说明](../architecture.zh-CN.md)。

测试覆盖 48 份请求/响应/流式快照、交错工具、晚到身份、引用分片、搜索失败、终止一致性及本地 shell 恢复/错误路径。fuzz 验证网络分块一致性和失败状态不可恢复；这些测试不替代真实上游的兼容验证。命令见 README。
