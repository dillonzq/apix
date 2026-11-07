# Gateway

English | [简体中文](gateway.zh-CN.md) · [Back to README](../README.md)

Package `gateway` is the optional executor. It runs the whole flow over HTTP: inspect, route, convert, send, convert back, and fail over. It is the only package in the module that makes network calls. The routing decisions it follows are described in [routing](routing.md).

## Setup

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

An `Upstream` is a `route.Endpoint` plus what is needed to reach it:

| Field | Effect |
| --- | --- |
| `BaseURL` | Scheme and host, plus any path prefix; the protocol's request path is appended to it |
| `Path` | Replaces the protocol's default path. Rejected for protocols whose path depends on the model or the streaming mode, such as GenerateContent; put a prefix in `BaseURL` instead |
| `APIKey`, `Auth` | The key, sent with `Auth` or the protocol's default scheme |
| `Header` | Extra headers for every request to this upstream |

`Handler(p)` serves clients of protocol `p`: it reads the body, calls `Do`, and writes the committed response, flushing after every read so streamed events reach the client promptly. Errors before the commit point are written in `p`'s error envelope; after it, a failure ends the response early. Callers that need their own HTTP layer call `Do` with a `gateway.Request` directly.

## Configuration

| `gateway.Config` field | Effect |
| --- | --- |
| `Client` | The `http.Client` for upstream requests. The default has no timeout, so streams are not cut off, and follows redirects only within the original origin; see [Redirects](#redirects) |
| `MaxAttempts` | How many candidates one request tries; 0 tries them all |
| `CommitTimeout` | Cancel an attempt that has not reached the commit point in time, and fail over; 0 means no limit |
| `MaxRequestBytes` | Limit request bodies read by `Handler` (default 32 MiB) |
| `MaxResponseBytes` | Limit ordinary response bodies (default 64 MiB); a larger one fails over |
| `MaxStreamFrameBytes` | Limit each upstream SSE frame (default 64 MiB), not the whole stream; see [Stream frame limits](#stream-frame-limits) |
| `ForwardHeaders` | Upstream response headers to pass to clients, such as `"X-Request-Id"` or `"X-Ratelimit-*"` |
| `ReasoningSuffix` | Route `model(high)` by `model` and apply the suffix as `Request.Reasoning`. Off by default because model IDs may contain parentheses |
| `RouterOptions` | Options for the underlying `route.Router`, such as `route.WithStrategy` or `route.WithAffinity` |
| `OnAttempt` | Observe every attempt, for logs and metrics |
| `OnUsage` | Receive the committed attempt's [token usage](../README.md#token-usage); see [State and usage](#state-and-usage) |

## Commit point and failover

`Do` tries candidates in the order the router returns them, and returns at the commit point:

- for an ordinary response, after it converts successfully;
- for a stream, after the first converted events arrive.

Before the commit point:

| Situation | Handling |
| --- | --- |
| Request conversion failed (e.g. the loss policy rejected it) | Next candidate; nothing was sent, so health is not affected |
| The endpoint is half-open with a trial in flight, so `Router.Begin` refused | Next candidate; nothing was sent |
| Network error, commit timeout, 401, 402, 403, 429, 5xx, or another `Retryable` error | Reported to health tracking; next candidate |
| 404, classified as `ClassInvalidModel` | Next candidate; reported, but not counted against the endpoint |
| 400 or another non-retryable error | Stop and return it in the client's protocol |
| 2xx, but the response or stream does not decode or convert, or exceeds `MaxResponseBytes` | Reclassified as a retryable `ClassUpstream` error; next candidate |
| Request canceled | Stop immediately; health is not affected. The error matches `errors.Is(err, context.Canceled)` |

A 404 is retryable because model catalogs and stored state differ between upstreams. It describes the request rather than the endpoint, so the breaker ignores it; see [Health](routing.md#health). On `CommitTimeout` expiry the attempt is canceled and counts as a retryable network error; a stream that has already committed is never cut off by it.

After the commit point nothing is retried, because the client may already have output. A failure surfaces from `Response.Body.Read` as an `*apix.Error`. A stream reports one health outcome when it ends, fails, or is closed, and only full completion counts as success. Caller cancellation and early `Body.Close` are inconclusive: they release any half-open trial, and neither clear prior failures nor penalize the endpoint. Cancellation errors preserve both `context.Canceled` or `context.DeadlineExceeded` and a custom cancellation cause through `errors.Is`.

## Requests and headers

Authentication follows the protocol's default scheme: `Authorization: Bearer <key>` for OpenAI, `x-api-key: <key>` plus `anthropic-version` for Anthropic, and `x-goog-api-key` for Gemini. `Upstream.Auth` overrides it. The upstream path is built from the converted payload's metadata, so the model and streaming mode in the URL always match what the exchange produced. Headers are layered in this order, later ones replacing earlier ones:

1. protocol headers;
2. `Request.Header`;
3. `Upstream.Header`;
4. the authentication header.

Client credentials are never forwarded. Upstream response headers reach clients only when listed in `Config.ForwardHeaders` (a trailing `*` matches a prefix); framing headers such as `Content-Type` and `Content-Length` are never copied. `Response.UpstreamHeader` holds all of them for `Do` callers. An upstream `Retry-After` reaches the error as `RetryAfter`, pauses that endpoint, and is relayed by `Handler` when the request fails.

`Request.Output` converts the response to another protocol of the same Kind; empty means the request's protocol. Routing still prefers endpoints that speak the request's protocol, because that is what decides whether the request needs conversion. `Request.Reasoning` overrides the reasoning control for every upstream; with `Config.ReasoningSuffix`, `Do` sets it from a `model(high)` suffix and routes by the model without the suffix.

## Model-dependent paths

Gemini's GenerateContent, GenerateContentImage, and countTokens protocols carry the model, and for generation the streaming mode, in the URL rather than the body. Their `ProtocolSpec.ParsePath` reads `/v1beta/models/{model}:{action}` (`/v1/models/` is also accepted) into `Payload.Metadata`, where the action is `generateContent` or `streamGenerateContent` for generation and `countTokens` for counting; other actions are rejected. `Handler` calls it automatically. Mount these handlers under `/v1beta/models/`. Generation and counting share that prefix, so dispatch on the action before choosing a handler, as shown in [token counting](protocols/tokens.md#gateway-setup).

Outbound, the path is built from the converted payload's metadata: `/v1beta/models/{upstreamModel}:generateContent`, or `:streamGenerateContent?alt=sse` for streams. `Upstream.Path` cannot replace a path that depends on the model or the streaming mode; put any prefix in `BaseURL`. Callers that convert GenerateContent bodies without the gateway pass the metadata themselves; see [GenerateContent URLs](protocols/llm.md#generatecontent-urls).

## Stream frame limits

`Config.MaxStreamFrameBytes` limits each upstream SSE frame to 64 MiB by default, counting comments and wire line separators but not separators supplied at EOF. It does not limit the stream's total length. `route.EndpointOptions.MaxStreamFrameBytes` overrides it per endpoint, and direct codec users set `apix.ExchangeOptions.MaxStreamFrameBytes`. A negative value explicitly disables the limit for trusted upstreams; raise it explicitly for large image frames.

An oversized frame fails over before the commit point and becomes a terminal read error after it, preserving valid earlier events. It matches `errors.Is(err, sse.ErrFrameTooLarge)`.

## Redirects

The default HTTP client follows at most ten requests in a redirect chain and permits only the original scheme, hostname, and effective port. Cross-origin redirects, including HTTPS downgrades, fail before another host can receive credentials. A caller supplying `Config.Client` is responsible for its redirect policy, especially for custom authentication headers.

## State and usage

Exchanges that report created state, such as stored Responses or Interactions, pass it to `Router.Remember` at the commit point and again at the end of a stream, so follow-up turns return to the same endpoint. See [upstream-owned state](routing.md#upstream-owned-state).

`Config.OnUsage` runs only when the upstream reported usage. For an ordinary response it runs when `Do` returns. For a stream it runs once, at EOF, at a failure, or at `Body.Close`, whichever comes first, with the counts seen so far. It also works with `Handler`, and `Response.Usage()` returns the same value; for streams, call it after the body is drained or closed.

## Updates

`Gateway.Update(upstreams)` replaces the upstream set atomically under the rules of [`Router.Update`](routing.md#updates). Requests in flight keep the URLs, credentials, and protocols they started with through failover. The gateway copies upstream headers and authentication schemes. Use `Gateway.Update` to change gateway configuration; `Gateway.Router()` is for inspection, health reports, and `Remember`.
