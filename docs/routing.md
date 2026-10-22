# Routing and failover

English | [简体中文](routing.zh-CN.md) · [Back to README](../README.md)

Package `route` decides which endpoints a request tries, in order, and performs no I/O. Package `gateway` executes that order and reports every attempt back to the router.

## Endpoints

```go
route.Endpoint{
    ID:       "anthropic-main",     // unique; health is tracked per ID
    Provider: "anthropic",          // label for logs and metrics
    Protocol: anthropic.Messages,   // the protocol the endpoint speaks
    Models:   map[string]string{"smart": "claude-sonnet-4-5", "*": ""},
    Priority: 0,                    // lower is tried first
    Weight:   3,                    // weighted shuffle within a priority
    Features: []string{apix.FeatureStream, apix.FeatureTools},
    Options:  apix.ExchangeOptions{LossPolicy: apix.LossPolicySafe},
}
```

- **Models** maps client model names or aliases to upstream names:
  - an exact match wins;
  - `"*"` matches any other model;
  - an empty value keeps the client's model;
  - an empty map serves every model unchanged.
- **Features** lists the capabilities an endpoint supports; empty means unrestricted. A codec's `Inspect` sets the features a request needs, such as `stream` for streaming and `tools` for tool use.
- **Options** are passed to the upstream codec for each exchange, for example `anthropic.MessagesOptions`.
- **Disabled** removes an endpoint from routing.

## Ordering

`Router.Route(info)` first filters, then orders.

It keeps endpoints that:

1. are not disabled;
2. speak a protocol of the request's Kind;
3. serve the request's model;
4. support every feature the request needs;
5. satisfy `Registry.CanConvert(request protocol, endpoint protocol)`.

The default `NativeFirst` strategy orders the remaining endpoints:

1. **Native first**: endpoints that speak the client's protocol (`Candidate.Native`) need no conversion, only a model rewrite.
2. **Convertible next**: endpoints with other protocols of the same Kind, reached through direct conversion.
3. **Priority within a tier**: lower `Priority` first.
4. **Weight within a priority**: a weighted shuffle without replacement; weights below 1 count as 1.

Native endpoints always precede convertible ones, whatever their priority. Supply a custom `Strategy` with `route.WithStrategy` for other policies.

When nothing qualifies, `Route` fails:

- with `ClassInvalidModel` if no endpoint serves the model;
- with `ClassUnsupported` if some endpoints serve the model but none can take the request.

## Health

`Router.Report(id, Outcome)` records an attempt. The default `Breaker` works as follows:

- **Open**: after `Threshold` consecutive failures (default 3), the endpoint is skipped for `Cooldown` (default 30 seconds).
- **Half-open**: once the cooldown ends, the endpoint receives traffic again. A single further failure reopens it; a success closes it.
- **What counts**: authentication, unknown model, rate limit, quota, billing, upstream server, and network errors. Translation and unsupported-request errors describe the request, not the endpoint, and are ignored.
- **All open**: when every eligible endpoint is open, the router still returns them all in normal order rather than refusing service.

Health is per process. To share it across instances, implement `route.Health`.

## Updates

`Router.Update(endpoints)`, or `Gateway.Update(upstreams)`, replaces the endpoint set atomically:

- requests already routing keep the old set;
- gateway requests keep the matching URLs, credentials, and protocols through failover;
- invalid updates leave the active configuration unchanged;
- health is kept per endpoint ID;
- the router copies the maps and slices it receives, so later changes by the caller do not affect it.

The gateway also copies upstream headers and authentication schemes. Use `Gateway.Update` to change gateway configuration; `Gateway.Router()` is for inspection and health reports.

## Gateway execution

`gateway.Do` tries candidates in order. `MaxAttempts` of 0 tries them all.

**Commit point**: `Do` returns only after an ordinary response converts successfully, or after a stream yields its first converted events.

Before the commit point:

| Situation | Handling |
| --- | --- |
| Request conversion failed (e.g. the loss policy rejected it) | Next candidate; nothing was sent, so health is not affected |
| Network error, 429, 5xx, or another `Retryable` error | Recorded as a failure; next candidate |
| 400 or another non-retryable error | Stop and return it in the client's protocol |
| 2xx, but the response or stream does not decode or convert | Treated as a retryable upstream error; next candidate |
| Request canceled | Stop immediately; health is not affected |

After the commit point nothing is retried, because the client may already have output. A failure surfaces from `Response.Body.Read` as an `*apix.Error` and is reported to health tracking.

Authentication follows the protocol's default scheme: `Authorization: Bearer <key>` for OpenAI, and `x-api-key: <key>` plus `anthropic-version` for Anthropic. `Upstream.Auth` overrides the scheme and `Path` the default path. Headers are layered in this order:

1. protocol headers;
2. `Request.Header`;
3. `Upstream.Header`;
4. the authentication header.

Client credentials are never forwarded.

`Request.Output` converts the response to another protocol of the same Kind; empty means the request's protocol. Routing still prefers endpoints that speak the request's protocol, because that is what decides whether the request needs conversion.
