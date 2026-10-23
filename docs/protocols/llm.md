# LLM protocol conversion

English | [简体中文](llm.zh-CN.md) · [Back to README](../../README.md)

This document describes the current mappings and limitations. It covers the `apix.KindLLM` protocols `openai.chat-completions`, `openai.responses`, and `anthropic.messages`; all six cross-protocol directions support requests, ordinary responses, and streams. See the README for protocol IDs and integration examples.

GenerateContent and native Interactions are documented separately in [Gemini protocols](gemini.md).

## Configuration

The LLM codecs support media input, local shell adaptation, cache hint retention, and reasoning history replay. Configure native Responses tool normalization with `openai.ResponsesOptions{NormalizeTools: true}`, unsigned thinking replay with `anthropic.MessagesOptions{AllowUnsignedThinking: true}`, and reasoning overrides with `apix.ExchangeOptions.Reasoning`. Use `sse.Decoder` to decode SSE frames.

`Inspect` also reports the upstream state a request refers to (`RequestInfo.Affinity`, such as `response:resp_1` or `file:file-1`) and whether it continues upstream-owned context (`RequestInfo.Stateful`). Native Responses exchanges report stored response IDs through `apix.StateReporter`. See [routing](../routing.md#upstream-owned-state).

## Native paths and responsibilities

Same-protocol requests rewrite only `model`, preserving historical reasoning, compaction, roles, tools, and unknown fields. An unchanged model returns the original bytes. Model catalogs, routing, and upstream compatibility cleanup belong to callers.

Same-protocol responses generally pass through. Responses tool identity restoration and stream error handling, termination checks, and SSE framing are exceptions. Native tool restoration preserves namespaces and unmodeled numeric fields.

## Request mappings

| Content | Current behavior |
| --- | --- |
| system/developer/instructions | Mapped to target system/instructions. Messages also merges user/tool_result and consecutive assistant content; historical message boundaries may change |
| Token limits | Maps `max_tokens/max_completion_tokens/max_output_tokens`. Messages requires a positive limit and defaults absent/nonpositive limits to 4096. Responses → Chat Completions omits nonpositive max_output_tokens |
| stop/stop_sequences | Preserved between Chat Completions and Messages; dropped when converting to Responses |
| Empty content and content types | Empty text blocks/messages without effective content are generally removed. Unknown cross-protocol input types, system images, and unrepresentable tool results fail explicitly rather than being uniformly discarded |
| Tool choice / parallel execution | Maps auto/none/required/named tools. Messages parallel control maps to disable_parallel_tool_use; unrepresentable combinations fail |
| Sampling | Maps `temperature` and `top_p`; unmapped top-level fields such as `n`, `seed`, penalties, `metadata`, `store`, `service_tier`, and `previous_response_id` produce known-loss diagnostics |

## Structured output

JSON Schema controls map between these fields:

| Protocol | Field |
| --- | --- |
| Chat Completions | `response_format.json_schema` |
| Responses | `text.format` |
| Messages | `output_config.format` |

Chat and Responses can map `json_object`. Messages requires `json_schema`; schema-free `json_object` fails explicitly. Callers determine whether the upstream model supports the target schema.

Messages output format does not carry OpenAI schema names, descriptions, or optional strictness. Name/description loss is reported as presentation loss; explicit `strict: false` reports a semantic change. Function tool `strict` is preserved, including explicit `false`.

## Tool identities and search

Responses `additional_tools` merges into declarations. Namespaces flatten into function names; long names use truncation plus a digest. All ordinary and flattened names are checked for collisions. Request state restores the original name, namespace, and custom identity in responses.

Custom tools, including `apply_patch`, can become functions using string `input`; the default schema requires `input`. Responses output restores custom calls across items, input delta/done, and terminal snapshots. Unrepresentable constraints such as custom grammars produce diagnostics.

### Local shell adaptation

This adapts a Responses client's local shell for Chat Completions or Messages upstreams, or for a Responses upstream through `openai.ResponsesOptions{NormalizeTools: true}`. The same `Exchange` restores the original tools in responses to the Responses client.

A local `shell` declaration (environment omitted or `type: "local"`, including `{"type":"local","skills":[]}`) becomes a function named `shell`. Its arguments contain a required nonempty `commands` array and optional nullable `timeout_ms` and `max_output_length` limits. Omitted or empty-array local `skills` are supported; null and other non-array values are rejected, and nonempty skills or other unrepresentable local settings fail explicitly. Shell actions reject properties outside `commands`, `timeout_ms`, and `max_output_length`. Local shell tools cannot be namespaced or declared alongside an ordinary function/custom tool named `shell`. Unqualified historical `custom_tool_call` items named `shell` also conflict and are rejected before shell identity registration; namespaced custom calls with the same leaf name remain distinct.

Historical `shell_call` items become function calls. `shell_call_output` becomes JSON text preserving output order, stdout, stderr, exit codes, and timeout outcomes. If `max_output_length` is present, the text contains an object with `output` and `max_output_length`; otherwise it contains the output array. Cross-protocol Responses request identities are committed only after the entire conversion succeeds; failure diagnostics remain available.

Restored calls include a stable item `id`, a separate `call_id`, `environment: {"type":"local"}`, and a status. Native streams retain the first item ID for each `call_id`, including later snapshots that omit `id`; native statuses are preserved. Missing or blank call IDs are rejected before restoration; missing status becomes `in_progress` on an added event and `completed` on a completed item. Missing action limits are emitted as `null`.

Streaming announces the shell item before completion, then emits the complete action in `response.output_item.done` and the terminal snapshot with the same item ID. Shell function argument delta/done events are suppressed. Protocol-level upstream failures are classified before argument validation, preserving their error category and retryability; invalid or incomplete arguments in successful responses fail.

The library does not execute commands. Hosted shell environments remain native on Responses paths and are unsupported across protocols; legacy `local_shell` tools are outside this mapping. Actual native `shell_call` output is not mapped to Chat Completions or Messages clients. Exact JSON numbers are preserved by explicit normalization and native Responses restoration; existing cross-protocol paths that decode arguments into floating-point values, including Messages request history, may round large integers.

### Hosted search

| Search path | Supported scope |
| --- | --- |
| Responses ↔ Messages | Representable web search declarations, queries, location, allowed domains, URL sources, and completion information |
| Search → Chat Completions | No hosted execution representation; execution metadata loss is reported, and an unrepresentable forced search choice fails |
| Native requests | Preserve search declarations/options without cross-protocol normalization |

Unrepresentable request tools such as `tool_search` and `image_generation` produce diagnostics. Hosted search inside a namespace is not flattened as an ordinary function. Encrypted Messages search results and unmapped search options also produce diagnostics.

A Messages query without its search result becomes a failed item in both ordinary and streaming conversion, with a missing-result diagnostic. Failed searches do not emit a successful `response.web_search_call.completed`; an empty result array can complete normally. A failed search item does not itself mean the whole response failed: the outer completion status follows the source termination reason.

## Reasoning responses

Non-streaming and streaming responses support all six cross-protocol directions:

| Upstream → client | Preservation |
| --- | --- |
| Chat Completions → Messages | Readable reasoning becomes `thinking` blocks / `thinking_delta` |
| Chat Completions → Responses | Readable reasoning is preserved in `reasoning.content[]` (`type: reasoning_text`), with a display copy in `summary[]` and corresponding summary stream events |
| Messages → Chat Completions | Thinking text is merged into `message.reasoning_content` / `delta.reasoning_content` |
| Messages → Responses | Each thinking block becomes a separate reasoning item with replay content and a display summary |
| Responses → Chat Completions | Reasoning `summary_text` and readable `content` (`reasoning_text` / `text`) are merged into `reasoning_content` |
| Responses → Messages | The same readable reasoning becomes `thinking` blocks / `thinking_delta` |

Chat Completions selects the first field containing readable text in this order: `reasoning_content` → `reasoning` → `reasoning_details`. Strings, arrays, and objects with `text` are supported. Aliases are not concatenated twice. Whitespace and Unicode are preserved. Reasoning stays separate from the final answer, and absent reasoning is not synthesized. Multiple Chat Completions reasoning fragments are aggregated into one reasoning item in the terminal Responses output; Messages thinking blocks remain separate reasoning items. Chat Completions output cannot preserve the original ordering among text, reasoning, and multiple blocks.

Responses streams accept `response.reasoning_summary_text.delta/done` and `response.reasoning_text.delta/done`. Reasoning snapshots from output items and terminal responses fill in text not delivered through deltas; already emitted prefixes of the same part are not repeated. Synthesized Responses streams include item/summary start, delta, and end events. Terminal output ordering and indexes match the announced items.

Cross-protocol conversion omits Anthropic `signature`, `signature_delta`, `redacted_thinking.data`, and Responses `encrypted_content`. These are provider-specific signatures or opaque data with no reliable cross-protocol mapping. Signatures are not fabricated, and opaque data is not interpreted as readable reasoning. Unsigned thinking is suitable for display but is not guaranteed to be replayable to Anthropic endpoints requiring valid signatures.

Same-protocol responses generally pass through unchanged and preserve these fields. Responses tool identity restoration and streaming error/termination handling are exceptions.

## Reasoning controls and suffixes

`reasoning_effort` and `reasoning.effort` map to each other. A level sent to Messages becomes adaptive thinking by default: `thinking: {"type": "adaptive"}` with the level in `output_config.effort`, merged with any structured output format. `none` disables thinking and `auto` leaves it unset. For Claude models that predate adaptive thinking, pass `anthropic.MessagesOptions{ThinkingMode: anthropic.ThinkingManual}` to send `{"type": "enabled", "budget_tokens": N}` instead. Native Messages thinking controls pass through unchanged. Manual mode and reverse mapping use a fixed effort/budget table:

| Effort | Budget |
| --- | --- |
| `none` | 0, disabled |
| `auto` | -1, upstream default |
| `minimal` | 512 |
| `low` | 1024 |
| `medium` | 8192 |
| `high` | 24576 |
| `xhigh` | 32768 |
| `max` | 128000 |

Reverse mapping uses thresholds, so round trips are not exact. Positive budgets above 24576 map to `xhigh`, never automatically to `max`. The library does not filter against model capabilities or clamp budgets.

Default entry points do not parse trailing `model(value)`. To give a suffix precedence, pass it to the exchange as a reasoning override:

```go
import "github.com/dillonzq/apix/reasoning"

suffix := reasoning.ParseSuffix(requestedModel)
ex, err := reg.NewExchange(client, upstream, apix.ExchangeOptions{
    Reasoning: suffix.Reasoning(), // nil for an absent or unknown suffix
    Ext:       upstreamOptions,    // e.g. anthropic.MessagesOptions{ThinkingMode: anthropic.ThinkingManual}
})
out, err := ex.ConvertRequest(apix.Payload{Body: payload}, suffix.Model)
```

The exchange strips the client's own control where conversion would derive the target control from it, converts, and then renders the override with the same options as the rest of the exchange, so the suffix and the upstream's thinking mode cannot disagree. `gateway.Config{ReasoningSuffix: true}` does all of this for every request, routing by the model without the suffix. Hosts that build upstream bodies themselves can still call `reasoning.StripSupersededReasoning` before conversion and `reasoning.ApplySuffixWith(upstream, body, suffix, reasoning.Options{...})` after it, setting `MessagesManual` and `GeminiMode` to match the exchange.

Recognized suffixes replace body reasoning controls. `none` / `0` disables reasoning; `auto` / `-1` uses upstream defaults. Chat and Responses receive an effort; numeric suffixes map through the thresholds above. Messages follows the exchange's thinking mode: adaptive thinking with `output_config.effort` by default, or enabled thinking with a table budget or a numeric suffix's exact budget under `anthropic.ThinkingManual`. GenerateContent follows `gemini.GenerateContentOptions.ThinkingMode`: budget mode sends `thinkingBudget`, level mode sends `thinkingLevel`, and the default sends numeric suffixes as budgets and levels as levels. Gemini has no level above `high`, so `xhigh` and `max` become `high` and are reported as a non-semantic request loss.

Messages disables `thinking.display` when turning thinking off and always supersedes the client's `output_config.effort` and `budget_tokens`; active overrides retain applicable display metadata. Responses overrides only `reasoning.effort`, preserving sibling fields.

Unknown suffixes can still be parsed and stripped, but leave body reasoning controls unchanged; `Suffix.Reasoning` returns nil for them. Only call `StripSupersededReasoning` for a recognized suffix. Callers decide whether literal parenthesized model IDs should be routed as-is.

Historical reasoning is mapped separately from reasoning controls. Chat assistant `reasoning_content` (or readable `reasoning` / `reasoning_details` aliases) and Messages assistant `thinking` are preserved as Responses `content` parts with `type: reasoning_text`. Responses original content maps back to Chat `reasoning_content`; the reasoning, assistant text, and following parallel calls share an assistant message, with tool results and user/system messages delimiting turns. Messages thinking maps to Chat reasoning on its original assistant message. Chat → Messages and Responses → Messages omit unsigned thinking by default, including with nil or zero-value state. For an upstream known to accept unsigned thinking, explicitly pass `anthropic.MessagesOptions{AllowUnsignedThinking: true}` as the exchange option with the default or `LossPolicyAllow` policy. This option affects request conversion only; native Messages history and response display are unchanged.

Request replay never substitutes a Responses summary for original content. Summary-only history, differing summaries, opaque/encrypted records, unknown reasoning variants, and signature loss are reported as semantic losses; safe/strict policies reject them. Identical display summaries alongside preserved original content do not cause a loss diagnostic. Complete empty Chat reasoning aliases do not conflict with a readable alias; incomplete aliases and non-empty conflicting values still report losses. Converting raw reasoning to Messages also reports a semantic loss because an authentic Anthropic signature is unavailable; safe/strict reject it even when `AllowUnsignedThinking` is enabled. The `unsigned_thinking` diagnostic is recorded both when the block is omitted and when emission is explicitly enabled. Native requests preserve all original fields.

Synthesized Responses non-streaming output, `response.output_item.done`, and terminal `response.output` retain original reasoning content for the next request. Summary events remain available for display. Mirrored summary/content snapshots are not emitted twice when converted back. This does not recover content already missing from a client's history, and callers must not subsequently strip preserved reasoning items. See [DeepSeek thinking replay requirements](https://api-docs.deepseek.com/guides/thinking_mode/) and [Anthropic thinking signatures](https://platform.claude.com/docs/en/docs/build-with-claude/extended-thinking).

## Response fields and usage

| Content | Current behavior |
| --- | --- |
| Text and function tool calls | Text and arguments are converted and preserved; absent arguments default to `{}`. Messages conversion parses arguments and rejects invalid JSON |
| Multiple `choices` | Cross-protocol conversion uses the first choice; same-protocol passthrough preserves all choices |
| Multiple messages / text blocks | Chat Completions aggregates body text. Synthesized Responses aggregates it into one assistant message at the first text position |
| Chat Completions `message.refusal` and Responses `refusal` parts | Preserved in non-streaming and streaming responses. Chat Completions ↔ Responses keeps separate refusal fields/parts; Messages renders them as visible text. Normally completed refusals map to content_filter/refusal; tool and truncation signals take priority |
| Finish reasons | `stop` ↔ `end_turn`, `length` ↔ `max_tokens`, `tool_calls` ↔ `tool_use`, `content_filter` ↔ `refusal`. Unknown reasons fall back to ordinary completion; the specific `stop_sequence` value is not copied |
| Tools and truncation together | Actual tool calls take priority over truncation in Chat Completions / Messages. Responses still maps `length/max_tokens` to `incomplete`, with tool signals represented by output items |
| Basic usage | Maps input/output tokens. Chat Completions/Responses totals are recalculated as input + output. Missing usage becomes zero counts |
| Cache hints | Chat Completions / Responses → Messages preserves content-block and tool `cache_control`, including `ttl` and extension fields. System and assistant text retains block boundaries when hints are present. Message/input-item hints map to the last block generated for that item; existing block hints take precedence. Chat tool envelope hints take precedence over function hints. Native requests retain hints; other cross-protocol targets report hint loss. No cache breakpoints are added automatically  Only a hint on the original last tool-output part migrates to the parent `tool_result`, taking precedence over message/item hints. Earlier child hints and hints on reasoning/additional_tools items are unrepresentable: loss policy omits and reports them, or rejects in strict mode. Namespace deduplication keeps the first declaration and reports discarded cache hints at their original field paths. |
| Cache usage | Messages input excludes cached tokens; conversion to Chat Completions/Responses adds cache read + creation. Conversion to Messages subtracts representable cache counts and clamps input to nonnegative. Cache reads are preserved; cache creation maps between cache_creation_input_tokens / cache_write_tokens. Absent details remain absent, explicit zeros remain explicit, and input totals do not double-count caches |
| Reasoning token usage | Chat Completions ↔ Responses preserves `reasoning_tokens`. Messages has no separate detail but still includes them in output totals |
| Identity and time | Response id/model are preserved. Synthesized message/reasoning item IDs derive from the response ID. Non-streaming cross-protocol Chat Completions uses the current time for created; Responses streams may supply upstream created_at. Other unmodeled identity/time fields are not copied |

Responses failed/cancelled statuses or protocol error objects return classified errors; other nonterminal statuses fail ordinary cross-protocol conversion. Incomplete maps to truncation. Synthesized Responses expresses completed/incomplete without retaining the complete response configuration.

## Stream lifecycle

Public entry points accept arbitrary network chunks and parse complete SSE frames, including multiple `data:` lines and LF/CRLF/CR separators. Output contains complete target events; Chat emits one `[DONE]`, synthesized Responses events contain increasing `sequence_number`, and native sequence numbers remain intact.

Tools are announced only after identity is complete; earlier arguments are buffered. Messages conversion holds later content behind unfinished tools and emits sequential block start/delta/stop events, with no deltas after closure. Argument done events, output items, and terminal snapshots fill missing argument tails.

Text, refusal, and reasoning done/item/terminal snapshots also reconcile omitted tails without repeating emitted prefixes. Visible text is tracked by item, content index, and part type.

`Finish` validates terminal state. Chat finish_reason / `[DONE]` and Messages stop_reason / message_stop can establish completion; Responses requires a valid terminal event. Missing termination signals or invalid residual payloads fail. Errors remain terminal, and subsequent calls do not generate successful completion.

Conversion rebuilds target events; SSE comments, retry, id, and ping metadata are not guaranteed across protocols.

## URL citations

URL citations map between Chat annotations, Responses annotation events, and Messages citations, retaining representable sources, titles, and text ranges.

Chat offsets count content only, excluding separate refusal text; Messages renders refusals as visible text. Representable Unicode ranges adjust with content aggregation; offsets lost through interleaving produce diagnostics.

When converting to Messages, annotations received before their text wait until the quoted range has actually been emitted. Incomplete fragments remain pending without being discarded or deduplicated, and citations are delivered before later queued blocks. Truly late citations whose target block is already closed report a `closed_block` loss; old blocks are not reopened, and citations are not attached to later text.

Page/document-location citations and encrypted citation metadata have no complete mappings; known losses are reported.

## Losses and capability boundaries

`Exchange.Diagnostics()` reports known field losses. See the README for allow/safe/strict configuration; safe/strict rejection applies only to requests. Rules inspect explicit protocol fields, never business data inside tool inputs, results, or JSON Schema.

Responses function and custom tool outputs can contain mixed text and images. Responses → Messages preserves their order inside `tool_result.content`, retaining the string representation for text-only results. Responses → Chat keeps consecutive tool replies together, then emits mixed results in an additional user message labeled with each call ID; text appears both in the tool reply and alongside its images. Empty messages and reasoning items do not interrupt this grouping.

User messages support file, audio, and video input conversion, following the [official CLIProxyAPI implementation](https://github.com/router-for-me/CLIProxyAPI/tree/091ff27/internal/translator).

| Input | Chat Completions ↔ Responses | Messages conversion |
| --- | --- | --- |
| File | `file.file_id/file_data/filename` ↔ `input_file.file_id/file_data/filename` | PDF ↔ base64 document sources, plain text ↔ text document sources; filenames map to document `title` |
| File URL | Responses preserves `file_url`; conversion to Chat fails | Responses `file_url` ↔ Messages URL documents |
| Audio | `input_audio` preserves `data` and `format`; flat Responses `data/format` is also accepted | Unsupported: Messages has no audio input block |
| Video | `video_url` ↔ `input_video`, accepting string or object URLs and preserving `processing` | Unsupported: Messages has no video input block |

Mixed text, images, and media retain their order. Top-level video `processing` overrides the nested value. Messages text-source documents become base64 `text/plain` files; conversion back to Messages decodes and validates UTF-8 before emitting `source.type="text"` with `media_type="text/plain"`. Only `application/pdf` uses a base64 document source. Other or unknown MIME types, including raw base64 without MIME, fail when converting to Messages; filenames are not used to infer types. Document `context` and `citations` settings report semantic loss and fail under Safe/Strict. File IDs are preserved only between OpenAI protocols; conversion to Messages fails. Callers must ensure that IDs are valid for the target upstream.

This support covers user input. Media in system or assistant messages and unrepresentable tool outputs fails. The library does not read local files, fetch URLs, or upload files: callers supply inline content, accessible URLs, or valid IDs. Missing media, invalid field types, and invalid document base64 fail. Same-protocol requests continue to preserve native fields and rewrite only the model. A convertible input does not imply model support for its media or MIME. Audio and video output remain unsupported.

Requests can map supported image URLs and base64 sources; system images and unrepresentable tool results fail. In responses, Chat → Messages maps supported image blocks, while Chat → Responses non-text output and unsupported Messages blocks fail. Image-generation results, audio, and further multimodal output remain unsupported. Unknown Responses items/parts are checked using original JSON, including unmodeled `result` / `action` payloads. Unrepresentable actual content fails rather than silently producing a successful empty response.

Logprobs, provider extensions, historical compaction, and signatures are not guaranteed across protocols. Preserving a field on a native path does not mean every upstream model accepts it; callers validate capabilities.

## Implementation and validation

The vendor codecs delegate to the engine in `internal/llm/translate`, which owns dispatch, request policy, and stream framing. Protocol adapters under `internal/llm/adapter` own direct conversion, upstream event dispatch, and target stream output. Shared protocol types, event construction, tool identities, and field rules are organized by responsibility in `internal/llm/adapter/shared`; reasoning controls and suffixes live in `reasoning`. See [code structure and maintenance](../architecture.md) for module and state contracts.

Tests cover 48 request/response/stream snapshots, interleaved tools, late identity, citation fragments, search failure, termination consistency, and local shell restoration/error paths. Fuzz tests check network chunking invariance and irreversible errors; they do not replace real-upstream compatibility validation. Commands are in the README.
