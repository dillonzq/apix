# Token counting

English | [简体中文](tokens.zh-CN.md) · [Back to README](../../README.md)

Token counting protocols belong to `apix.KindTokens`. They count the input tokens of an LLM request without generating, and convert only among themselves: a count never reaches a generation endpoint, and the reverse holds too. The implementation lives in `internal/tokens`; each vendor package exports its ID and constructor, and every vendor's `Codecs()` includes it.

| Protocol ID | Constant | Endpoint | Reply |
| --- | --- | --- | --- |
| `anthropic.messages.count-tokens` | `anthropic.MessagesCountTokens` | `POST /v1/messages/count_tokens` | `{"input_tokens": n}` |
| `openai.responses.input-tokens` | `openai.ResponsesInputTokens` | `POST /v1/responses/input_tokens` | `{"object": "response.input_tokens", "input_tokens": n}` |
| `google.count-tokens` | `google.CountTokens` | `POST /v1beta/models/{model}:countTokens` | `{"totalTokens": n, ...}` |

Chat Completions has no count endpoint, so an upstream that only speaks Chat Completions cannot serve counts. Counts never stream: the specs declare `apix.FramingNone`, and `NewStreamConverter` returns `ClassUnsupported`.

## Direct paths

The three protocols convert in all six directions. A count request carries the same content as a generation request of the same vendor, so conversion reuses the LLM engine:

1. The client's count body is read as its generation request: Messages and Responses bodies as they are, and a Gemini body either as `contents` or as the request inside `generateContentRequest` (`generate_content_request` is also accepted).
2. That request converts as it would for generation, with the same mappings, diagnostics and loss policy as [LLM protocol conversion](llm.md).
3. The result is reshaped into the upstream's count body.

| Upstream | Converted body |
| --- | --- |
| Messages | `model`, `messages`, `system`, `tools`, `tool_choice`, `thinking`, and `output_config.format` when structured output adds a schema. Generation controls such as `max_tokens`, `stream`, sampling and `output_config.effort` are dropped, because the endpoint rejects them and they do not change the input. |
| Responses | `model`, `input`, `instructions`, `tools`, `tool_choice`, `parallel_tool_calls`, `reasoning`, `text`. |
| Gemini | `{"generateContentRequest": {...}}` with `model` set to `models/{upstreamModel}`, so system instructions and tools are counted, not only `contents`. The request keeps `contents`, `systemInstruction`, `tools` and `toolConfig`; `generationConfig` keeps only the response schema fields (`responseMimeType`, `responseSchema`, `responseJsonSchema`), and generation controls such as `maxOutputTokens`, sampling and `thinkingConfig` are dropped. The model also travels in `Payload.Metadata` for the URL. |

Native requests keep their bytes; only the model changes. For a native Gemini body in the `generateContentRequest` form, the inner `model` is pointed at the upstream model as well.

The count is the upstream's, in its own tokenizer. When a request routes to another vendor, the count is accurate for the model that would serve it there, not for the model the client named.

## Replies

| Upstream reply | Count read |
| --- | --- |
| Messages, Responses | `input_tokens`, a non-negative integer |
| Gemini | `totalTokens` (or `total_tokens`), which proto JSON may write as a string; a missing count is 0, because proto JSON omits zero |

Native replies pass through unchanged once the count is checked. Converted replies carry only the count, so Gemini's `cachedContentTokenCount` and `promptTokensDetails` do not reach other protocols. A reply without a valid count is a `ClassUpstream` error, which the gateway treats as a retryable upstream fault. Error statuses are classified like generation errors, and `Handler` renders them in the client protocol's envelope.

Count exchanges do not implement `apix.UsageReporter`: counting is not billed usage.

## Options

The count codecs define no `Ext` options and reject them. `LossPolicy` and `Output` work as for other Kinds. A `Reasoning` override is validated and then ignored, because it does not change the input; with `gateway.Config.ReasoningSuffix`, a `model(high)` count routes by `model`.

## Gateway setup

Each endpoint speaks one protocol, so count endpoints are configured next to the generation endpoints of the same upstream:

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

Gemini's generation and count operations share the `/v1beta/models/` prefix. Dispatch on the action before choosing a handler:

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

Routing follows the usual rules: native endpoints first, then convertible ones. Requests with `previous_response_id` or `conversation` are `Stateful` and stay on Responses endpoints. Features detected in the content, such as image input, filter endpoints as they do for generation.
