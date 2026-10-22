# apix

<div align="center">

<img src="assets/logo.svg" alt="apix logo" width="144" height="144">

**Convert and route API calls across providers. One Go library.**

**English** · [简体中文](README.zh-CN.md)

[Install](#installation) · [Quick start](#quick-start) · [Routing](#routing) · [Gateway](#gateway) · [Docs](#documentation)

[![License: MIT](https://img.shields.io/badge/license-MIT-64748b?style=flat-square)](LICENSE)
[![Release: v0.1.0](https://img.shields.io/badge/release-v0.1.0-6366f1?style=flat-square)](https://github.com/dillonzq/apix/releases/tag/v0.1.0)
[![Go: 1.26.7+](https://img.shields.io/badge/Go-1.26.7%2B-0891b2?style=flat-square&logo=go&logoColor=white)](go.mod)
[![Dependencies: standard library](https://img.shields.io/badge/dependencies-standard_library-0d9488?style=flat-square)](go.mod)

</div>

---

apix converts requests, responses, and streams between provider API protocols, and routes each request to the best available provider: one that speaks the client's own protocol first, then one it can convert to. It uses only the Go standard library and requires Go **1.26.7+**.

The library is layered. The core converts and decides routes without any network I/O; an optional gateway executes requests over HTTP with failover.

| Package | Role |
| --- | --- |
| `apix` | Protocol-neutral core: `Kind`, `Protocol`, `Codec`, `Exchange`, `Registry`, errors, diagnostics |
| `protocol/openai`, `protocol/anthropic` | Codecs for each vendor's protocols |
| `route` | Native-first endpoint selection, priorities, weights, health tracking |
| `gateway` | Optional HTTP executor and `http.Handler` with failover |
| `sse` | Server-Sent Events framing and decoding |
| `thinking` | Optional LLM reasoning controls and `model(high)` suffixes |

## Supported protocols

| Kind | Protocol ID | Constant | Endpoint |
| --- | --- | --- | --- |
| `llm` | `openai.chat-completions` | `openai.ChatCompletions` | `POST /v1/chat/completions` |
| `llm` | `openai.responses` | `openai.Responses` | `POST /v1/responses` |
| `llm` | `anthropic.messages` | `anthropic.Messages` | `POST /v1/messages` |

The three LLM protocols convert directly to one another in all six directions; same-protocol requests only rewrite the model. Conversion covers text, image/file/audio/video input, function and custom tools, local shell adaptation, reasoning and its replay history, usage, structured output, cache hints, and representable citations. See [LLM protocol conversion](docs/protocols/llm.md) for mappings and limits.

Image generation, speech, and other kinds plug in through the same interfaces; see [adding protocols and kinds](docs/extending.md).

## Installation

```sh
go get github.com/dillonzq/apix@v0.1.0
```

## Quick start

Register the codecs you use, then create one `Exchange` per request. This converts an Anthropic Messages request for an OpenAI Chat Completions upstream and converts the reply back:

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
// Send upstream.Body with your HTTP client, then:
reply, aerr := ex.ConvertResponse(status, apix.Payload{Body: upstreamBody})
```

An empty upstream model keeps the client's model. For streams, convert the upstream body chunk by chunk:

```go
stream, aerr := ex.NewStreamConverter()
events, done, aerr := stream.Feed(networkChunk) // write events in order
tail, aerr := stream.Finish()                   // at upstream EOF
```

`Feed` accepts arbitrary chunks and returns complete client events ready to write. A call may return events together with an error: write the events, then stop. `Finish` fails a stream that ended without a valid terminal event, and a failed converter keeps failing.

An `Exchange` holds one execution's state, such as tool identities restored in responses and recorded diagnostics. Use a new one per request, sequentially. Codecs and registries are safe for concurrent use.

### Exchange options

`apix.ExchangeOptions` carries a loss policy and protocol-specific options for the upstream codec:

| Option | Upstream | Effect |
| --- | --- | --- |
| `Output: openai.Responses` | any | Convert responses and streams to another protocol of the same Kind instead of the client's; for hosts that choose the response format separately |
| `LossPolicy: apix.LossPolicySafe` | any | Reject request conversions that lose semantics |
| `LossPolicy: apix.LossPolicyStrict` | any | Reject every reported request loss |
| `Ext: anthropic.MessagesOptions{AllowUnsignedThinking: true}` | Messages | Replay cross-protocol reasoning as unsigned thinking blocks |
| `Ext: openai.ResponsesOptions{NormalizeTools: true}` | Responses | Flatten namespaces, wrap custom tools, and adapt local shell for upstreams that only accept function tools |

`ex.Diagnostics()` lists fields that conversion omitted or approximated, with `Code`, `Path`, `Message`, `Semantic`, `From`, and `To`. Responses and streams report losses but are never rejected because of the policy.

## Routing

`route.Router` orders the endpoints that can serve a request. Endpoints that speak the client's protocol come first (zero conversion); endpoints of the same kind whose protocol the request converts to follow. Within each tier, lower `Priority` wins and equal priorities are shuffled by `Weight`. Endpoints that keep failing are skipped for a cooldown.

```go
r, err := route.New(reg, []route.Endpoint{
    {ID: "openai", Protocol: openai.ChatCompletions, Models: map[string]string{"smart": "gpt-4.1"}},
    {ID: "anthropic", Protocol: anthropic.Messages, Priority: 1, Models: map[string]string{"smart": "claude-sonnet-4-5"}},
})
info, aerr := reg.Inspect(anthropic.Messages, apix.Payload{Body: clientBody})
candidates, aerr := r.Route(info) // anthropic first: it is native for a Messages client
// Try candidates in order and report every attempt so health tracking works:
r.Report(candidates[0].Endpoint.ID, route.Outcome{Err: attemptErr, Latency: elapsed})
```

`Models` maps client model names or aliases to upstream names; `"*"` matches any model. `Update` replaces the endpoint set at runtime. See [routing](docs/routing.md) for filters, the breaker, and custom strategies.

## Gateway

`gateway.Gateway` runs the whole flow over HTTP: inspect, route, convert, send, convert back, and fail over.

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

`Do` returns at the commit point: once an ordinary response converted, or once a stream produced its first event. Before that point, retryable failures (rate limits, server errors, network errors, unusable upstream output) and rejected conversions move to the next candidate. After it, nothing is retried, because the client may already have output; a failure surfaces from `Response.Body.Read`. Authentication headers follow each protocol's scheme (`Authorization: Bearer` for OpenAI, `x-api-key` plus `anthropic-version` for Anthropic). Client credentials are never forwarded.

## Errors

Errors are `*apix.Error` with `Class`, `Message`, `StatusCode`, and `Retryable`. Messages are redacted; use `apix.Redact` for your own text. `apix.HTTPStatus` picks a response status, and `Registry.EncodeError` renders an error in the client protocol's envelope. `FromNetwork` and `PostFirstByteNetwork` classify transport failures before and after the first upstream byte.

## SSE framing for hosts

Converters emit fully framed SSE. A host that adds its own `data:` framing can unframe output with a zero-value `sse.Decoder`:

```go
var decoder sse.Decoder
for _, frame := range events {
    for _, event := range decoder.Feed(frame) {
        if event.Done {
            continue // Chat Completions [DONE]; the host emits its own.
        }
        // Use event.Event and event.Data.
    }
}
decoder.Finish() // after all converter output
```

## Documentation

- [Architecture](docs/architecture.md): layers, dispatch, and state contracts
- [Routing](docs/routing.md): endpoint selection, health, and the gateway's failover rules
- [Adding protocols and kinds](docs/extending.md)
- [LLM protocol conversion](docs/protocols/llm.md): field mappings, tools, reasoning, streams, and limits
- [CONTRIBUTING.md](CONTRIBUTING.md): checks, design rules, and releases

## Development

```sh
make check # gofmt, vet, module and dependency checks, build, race tests
make fuzz  # stream fuzz targets
```

Conversion behavior is pinned by 27 reviewed snapshots under `internal/llm/translate/testdata/conversion`; update them only after reviewing the output change:

```sh
go test ./internal/llm/translate -run TestProtocolGolden -update
```

Licensed under [MIT](LICENSE).
