# Adding protocols and kinds

English | [简体中文](extending.zh-CN.md) · [Back to README](../README.md)

The apix core is protocol neutral. Adding a protocol, or a new Kind such as image generation or speech, means implementing `apix.Codec`; `route` and `gateway` need no changes.

## Where code goes

- Protocols are organized by vendor under `protocol/<vendor>/`. For example, OpenAI image generation goes in `protocol/openai/images.go`, and Gemini's APIs go under `protocol/gemini/`.
- Protocol IDs are namespaced as `<vendor>.<api>`, for example `openai.images.generations` or `gemini.generate-content`.
- Conversion code shared by several vendors of one Kind goes in `internal/<kind>/`, for example `internal/image/`. The LLM implementation lives in `internal/llm/`.
- Every vendor package provides `Codecs() []apix.Codec`. Callers register codecs explicitly with `apix.NewRegistry(slices.Concat(...))`.

## What to implement

```go
type Codec interface {
    Spec() apix.ProtocolSpec
    Inspect(req apix.Payload) (apix.RequestInfo, error)
    EncodeError(err error) apix.Payload
    Accepts(p apix.Protocol) bool
    NewExchange(client apix.Protocol, opts apix.ExchangeOptions) (apix.Exchange, error)
}
```

| Method | Notes |
| --- | --- |
| `Spec` | Fill in `ID`, `Kind`, `Method`, `Path`, the default `Auth` scheme, fixed `Headers`, and `Stream` framing. `Path` receives the upstream model, for protocols that carry it in the URL. |
| `Inspect` | Read `Model`, `Stream`, the `Features` the request needs, and any `Affinity` keys and `Stateful` flag for upstream-owned state it refers to. Bodies are not always JSON; use `Payload.ContentType` for multipart forms. `Registry.Inspect` overlays `Payload.Metadata` afterwards. |
| `EncodeError` | Render an error in the protocol's envelope; `gateway.Handler` replies with it. |
| `Accepts` | List the protocols with a **direct** path to and from this one: requests convert in from them, and responses convert out to them. Always true for the protocol itself. |
| `NewExchange` | Create state for one execution. Convert responses to `opts.Output` (the client protocol when empty). Read the options this protocol's package defines from `opts.Ext`, and return `ClassUnsupported` for option types it does not know. |

Return `*apix.Error` values as the `error`, and return a literal `nil` on success: a nil `*apix.Error` stored in an `error` is not nil.

Two optional extensions plug into routing:

- `apix.OptionsValidator` on the codec: `ValidateOptions(opts)` lets routers reject bad `Ext` values, loss policies or reasoning overrides when endpoints are configured. `NewExchange` must still validate.
- `apix.StateReporter` on the exchange: `CreatedState()` returns the affinity keys of state the upstream stored, such as `response:<id>`, so follow-up requests reach the same endpoint.

An `Exchange` must follow these rules:

- `ConvertRequest(req, upstreamModel)` keeps the client's model when `upstreamModel` is empty.
- `ConvertResponse(status, resp)` returns the classified upstream error for statuses of 400 and above. The gateway uses that classification to decide whether to fail over.
- `NewStreamConverter()` returns `ClassUnsupported` when the protocol does not stream; the spec should then declare `Stream: apix.FramingNone`.
- Stream converters emit byte events. SSE protocols emit complete SSE frames; binary streams, such as audio, emit raw chunks.

## Example: the image Kind

`apix.KindImage` is implemented this way; read it as a worked example of a Kind outside `internal/llm`. See [image protocol conversion](protocols/image.md) for its behavior.

- `internal/image/codec.go` holds one `Codec` type for all four image protocols. Their differences, such as path, authentication, error envelope, streaming and accepted protocols, are rows of a `protocols` table. Error envelopes come from `internal/vendorerr`, which the LLM codecs share.
- `openai.go`, `gemini.go` and `imagen.go` hold each wire format: request decoding (JSON and multipart forms), response types, and the native request rewrite.
- `exchange.go` and `response.go` hold the direct converters, one function per source and target, with no intermediate representation. `stream.go` validates native streams; cross-protocol streams are emulated in `ConvertResponse`.
- `protocol/openai/images.go` and `protocol/gemini/images.go` export the IDs and constructors, and each vendor's `Codecs()` includes them. `gemini.GenerateContentImageOptions` is converted to `image.Options` by an `ExtFunc` passed to `image.New`, the same pattern the LLM codecs use.

```go
func (c *Codec) Spec() apix.ProtocolSpec {
    spec := apix.ProtocolSpec{ID: c.id, Kind: apix.KindImage, Method: "POST", Auth: c.proto.auth, Stream: c.proto.stream}
    // Path and ParsePath depend on the protocol family.
    return spec
}

// Accepts declares only the direct paths that exist.
func (c *Codec) Accepts(p apix.Protocol) bool { return p == c.id || slices.Contains(c.proto.accepts, p) }
```

Once registered, image requests route by the same rules as LLM requests: native image endpoints first, then convertible ones. LLM endpoints belong to a different Kind and are never chosen.

```go
reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), gemini.Codecs())...)
```

## Testing

- **Exchange tests**: table-driven tests of the request, response, and stream (if supported) for every `Accepts` direction.
- **Snapshots**: add reviewed snapshots once the output is confirmed.
- **Hostile upstream output**: cover wrong field types, truncated streams, and EOF without a terminal event.
- **Gateway integration**: at least one `httptest` upstream case that checks the spec's path, authentication, and `EncodeError`.
- **Dependencies**: keep the standard-library-only rule.
