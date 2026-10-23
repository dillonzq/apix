# Gemini protocols

[简体中文](gemini.zh-CN.md) · [README](../../README.md)

`protocol/gemini` exports two independent `KindLLM` codecs:

| Codec | Protocol | Conversion paths |
| --- | --- | --- |
| `GenerateContentCodec()` | `gemini.generate-content` | Native and direct bidirectional request, response and SSE conversion with Chat Completions, Responses and Messages |
| `InteractionsCodec()` | `gemini.interactions` | Native only; other protocols, including GenerateContent, are rejected |

`gemini.Codecs()` returns both. Register the other vendors' codecs explicitly when using cross-protocol exchanges. `ExchangeOptions.Output` also works with GenerateContent. Routing still filters eligible endpoints and prefers the client's own protocol.

## Request metadata and HTTP

GenerateContent carries the model and streaming operation in its URL. Use the optional `Payload.Metadata` when the body does not contain them:

```go
stream := true
payload := apix.Payload{
    Body: []byte(`{"contents":[{"role":"user","parts":[{"text":"Hello"}]}]}`),
    Metadata: apix.RequestMetadata{Model: "gemini-model", Stream: &stream},
}
info, err := reg.Inspect(gemini.GenerateContent, payload)
```

A nonempty metadata model and a non-nil stream value override body routing values. Metadata is request-only and is not serialized into the body. Existing Codec and Exchange method signatures are unchanged. A custom codec should honor metadata during conversion; Registry also applies it to routing information.

The Gemini codec's `ProtocolSpec.ParsePath` extracts metadata from `/v1beta/models/{model}:generateContent` or `:streamGenerateContent` (`/v1/models/` is also accepted). `gateway.Handler(gemini.GenerateContent)` calls it automatically. Register that handler at `/v1beta/models/` in an HTTP mux. Other operations such as `countTokens` are rejected.

The outbound `ProtocolSpec.Path` chooses `/v1beta/models/{upstreamModel}:generateContent` or `:streamGenerateContent?alt=sse`. Authentication uses `x-goog-api-key`. The converted Gemini payload carries its effective model and stream mode in `Metadata`; native bodies keep their bytes unless an explicit thinking option changes them. For a Gemini client sent to another protocol, the stream flag is written into the converted JSON request.

## GenerateContent mappings

- `systemInstruction` and `contents` map to instructions/system messages and user/assistant turns. Gemini `model` roles map to assistant roles. Unsupported actual content is rejected rather than silently producing an empty response.
- `functionDeclarations` and `parametersJsonSchema` map to function tools; legacy Gemini Schema types and nullable fields are adapted. Function calls/results retain IDs, including repeated names and out-of-order results. Arguments retain JSON number precision.
- OpenAI Responses namespaces, custom tools and local shell use the existing normalization and restoration machinery. Unsupported hosted tools and options produce diagnostics; Safe/Strict request policies reject the corresponding losses.
- `generationConfig` maps sampling, token limits, stop sequences, candidate count and structured output. Multiple candidates map to Chat choices. When another target can represent only one candidate, additional output candidates produce loss diagnostics.
- Inline images, PDF/text documents, audio and video map where the target supports them. Provider-scoped file IDs cannot be sent to Gemini. The library never downloads or uploads media. Messages cannot represent audio/video inputs; Chat cannot represent a document supplied only by URI.
- Responses include text, reasoning, tools, refusal/stop reasons and usage. Prompt, cached and reasoning token counts remain separate. Supported image output can map to Messages blocks or the Chat `images` extension; Responses image output and audio/video output are rejected.
- Representable grounding references map to URL citations, converting Gemini byte offsets to text rune offsets. Chat/Responses receive late stream annotations; Messages receives them while the referenced text block remains open and reports a loss for already-closed blocks. Incoming citation stream fragments from the other protocols currently produce loss diagnostics.

Known camelCase/snake_case aliases are accepted at the protocol boundary. Business data inside arguments, results and JSON Schema properties is never searched for aliases. Unknown request options are diagnosed; native requests retain provider extensions.

## Thinking options

Set `ExchangeOptions.Ext` to `gemini.GenerateContentOptions`:

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

`ThinkingBudget` maps levels to budgets; `ThinkingLevel` maps positive budgets to levels. With no mode override, the client control keeps its form. Zero/off and -1/auto retain numeric sentinels. `IncludeThoughts` is independent of reasoning amount. The host selects the mode supported by its model; apix does not infer capabilities from model names or clamp against an online model catalog. `thinking.ApplySuffix` also understands GenerateContent.

Gemini thinking controls sent to Messages follow the Messages thinking mode: adaptive by default, with numeric budgets converted to an effort level; with `anthropic.ThinkingManual`, numeric budgets stay exact and levels use the fixed budget table. The host remains responsible for selecting a compatible Anthropic model and validating its budget/output limits.

## Signed history

Gemini signatures are opaque and part-specific. Native paths preserve them. Converted responses carry original ordered Gemini parts in an apix extension, `extra_content.google.parts`:

- Chat: on the assistant message, or the final stream delta.
- Messages: on response blocks, and on the final stream message delta.
- Responses: on output items, including completed stream items and the terminal snapshot.

On the next request to Gemini, retain this extension with the assistant message/item. The converter restores the signed parts. It also accepts `extra_content.google.thought_signature` on supported individual messages/tool parts. SDKs that discard unknown fields require the host to preserve and restore the extension separately. An Exchange is per execution, not a session cache. Gemini signatures are never fabricated or presented as another vendor's native signature.

`RequireThoughtSignatures` rejects historical function calls missing a Gemini signature. It is opt-in because requirements differ by model. GenerateContent tools currently require complete `args` objects; incremental `partialArgs`/`willContinue` function-argument variants are explicitly rejected by cross-protocol conversion.

## Streams

`Feed` accepts arbitrary SSE network chunks. A Gemini `finishReason` records a terminal state but does not close the converter: later usage and signatures must still be read. Call `Finish` at transport EOF. The converter then emits the target terminal, or fails if no valid terminal state was observed. Errors retain earlier output, remain terminal on subsequent calls, and never manufacture a successful completion.

For the other source protocols, native validation runs before conversion emits a Gemini terminal. Tool argument fragments are buffered until complete. Gemini native SSE frames are passed through unchanged.

## Native Interactions

The path is `POST /v1beta/interactions`, authenticated with `x-goog-api-key`. Requests must specify exactly one of `model` or `agent`. Native model aliases may be rewritten; agent requests cannot be rewritten into a different model. `previous_interaction_id`, `background`, tools, input and extensions are passed through to the upstream. Responses may remain `in_progress` for background work.

Native SSE preserves events and validates completion/tool handoff and errors. No conversion to GenerateContent or another vendor is advertised. apix does not retrieve stored history, poll background jobs, cancel tasks or switch conversation state between providers. Those remain host/upstream responsibilities.

## Validation

The conversion suite contains 48 request/response/stream snapshots: the original 27 plus 21 GenerateContent cases. Public tests cover independent response protocols, exact tool arguments, media, signed replay, late signatures, Unicode citations, invalid source terminals and whole/bytewise stream equivalence. Gateway tests cover URL metadata, auth, native preference, cross-protocol failover and Interactions state passthrough. `make fuzz` includes the Gemini boundary/terminal target.

Field mappings were checked against [CLIProxyAPI `ceb045a`](https://github.com/router-for-me/CLIProxyAPI/tree/ceb045a129d43f805e93a5e4aef3a59b108f34f7/internal/translator/gemini), the [GenerateContent API](https://ai.google.dev/api/generate-content), and [Interactions](https://ai.google.dev/gemini-api/docs/interactions-overview). Local fixtures do not replace live-provider compatibility tests.
