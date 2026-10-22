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
    Inspect(req apix.Payload) (apix.RequestInfo, *apix.Error)
    EncodeError(err *apix.Error) apix.Payload
    Accepts(p apix.Protocol) bool
    NewExchange(client apix.Protocol, opts apix.ExchangeOptions) (apix.Exchange, *apix.Error)
}
```

| Method | Notes |
| --- | --- |
| `Spec` | Fill in `ID`, `Kind`, `Method`, `Path`, the default `Auth` scheme, fixed `Headers`, and `Stream` framing. `Path` receives the upstream model, for protocols that carry it in the URL. |
| `Inspect` | Read `Model`, `Stream`, and the `Features` the request needs. Bodies are not always JSON; use `Payload.ContentType` for multipart forms. |
| `EncodeError` | Render an error in the protocol's envelope; `gateway.Handler` replies with it. |
| `Accepts` | List the protocols with a **direct** path to and from this one: requests convert in from them, and responses convert out to them. Always true for the protocol itself. |
| `NewExchange` | Create state for one execution. Convert responses to `opts.Output` (the client protocol when empty). Read the options this protocol's package defines from `opts.Ext`, and return `ClassUnsupported` for option types it does not know. |

An `Exchange` must follow these rules:

- `ConvertRequest(req, upstreamModel)` keeps the client's model when `upstreamModel` is empty.
- `ConvertResponse(status, resp)` returns the classified upstream error for statuses of 400 and above. The gateway uses that classification to decide whether to fail over.
- `NewStreamConverter()` returns `ClassUnsupported` when the protocol does not stream; the spec should then declare `Stream: apix.FramingNone`.
- Stream converters emit byte events. SSE protocols emit complete SSE frames; binary streams, such as audio, emit raw chunks.

## Example: an image protocol (interface sketch)

```go
package openai

const ImagesGenerations apix.Protocol = "openai.images.generations"

type imagesCodec struct{}

func (imagesCodec) Spec() apix.ProtocolSpec {
    return apix.ProtocolSpec{
        ID: ImagesGenerations, Kind: apix.KindImage, Method: "POST",
        Path:   func(apix.RequestInfo, string) string { return "/v1/images/generations" },
        Auth:   apix.AuthBearer,
        Stream: apix.FramingNone,
    }
}

func (imagesCodec) Inspect(req apix.Payload) (apix.RequestInfo, *apix.Error) {
    var doc struct{ Model string `json:"model"` }
    if err := json.Unmarshal(req.Body, &doc); err != nil {
        return apix.RequestInfo{}, apix.Translation("malformed images request: " + err.Error())
    }
    return apix.RequestInfo{Model: doc.Model}, nil // the Registry fills in Protocol and Kind
}

// Declare only the direct paths that exist, such as a future gemini.images.
func (imagesCodec) Accepts(p apix.Protocol) bool {
    return p == ImagesGenerations || p == "gemini.images"
}

func (imagesCodec) NewExchange(client apix.Protocol, opts apix.ExchangeOptions) (apix.Exchange, *apix.Error) {
    return image.NewExchange(client, ImagesGenerations, opts) // shared code lives in internal/image
}
```

Once the codec is registered, image requests route by the same rules: native image endpoints first, then convertible ones. LLM endpoints belong to a different Kind and are never chosen.

```go
reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), gemini.Codecs())...)
```

## Testing

- **Exchange tests**: table-driven tests of the request, response, and stream (if supported) for every `Accepts` direction.
- **Snapshots**: add reviewed snapshots once the output is confirmed.
- **Hostile upstream output**: cover wrong field types, truncated streams, and EOF without a terminal event.
- **Gateway integration**: at least one `httptest` upstream case that checks the spec's path, authentication, and `EncodeError`.
- **Dependencies**: keep the standard-library-only rule.
