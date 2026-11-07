# Routing

English | [简体中文](routing.zh-CN.md) · [Back to README](../README.md)

Package `route` decides which endpoints a request tries, in order, and performs no I/O. Package `gateway` executes that order and reports every attempt back to the router; its failover rules are in [gateway](gateway.md).

## Endpoints

```go
route.Endpoint{
    ID:          "anthropic-main",     // unique; health is tracked per ID
    Provider:    "anthropic",          // label for logs and metrics
    Protocol:    anthropic.Messages,   // the protocol the endpoint speaks
    Models:      map[string]string{"smart": "claude-sonnet-4-5", "*": ""},
    Priority:    0,                    // lower is tried first
    Weight:      3,                    // weighted shuffle within a priority
    Unsupported: []string{apix.FeatureAudioInput},
    Options:     route.EndpointOptions{LossPolicy: apix.LossPolicySafe},
}
```

- **Models** maps client model names or aliases to upstream names:
  - an exact match wins;
  - `"*"` matches any other model;
  - an empty value keeps the client's model;
  - an empty map serves every model unchanged.
- **Unsupported** lists capabilities the endpoint lacks; requests that need any of them skip it. A codec's `Inspect` sets the features a request needs from known protocol fields: `stream`, `tools`, `image-input`, `file-input`, `audio-input`, `video-input`, `image-output`, `audio-output`, `video-output`, `structured-output` and `web-search` (`apix.Feature*` constants). Tool schemas, arguments and results are never scanned. Capabilities not listed are assumed supported, so a feature that a later version learns to detect never excludes an existing endpoint.
- **Options** are the endpoint's exchange options: a loss policy and the protocol package's `Ext`, for example `anthropic.MessagesOptions`. `New` and `Update` validate them against the endpoint's codec through `Registry.ValidateOptions`, so a misconfigured endpoint fails at configuration time rather than being skipped on every request. The response protocol and reasoning overrides belong to each request, not to the endpoint.
- **Disabled** removes an endpoint from routing.

## Ordering

`Router.Route(info)` first filters, then orders.

It keeps endpoints that:

1. are not disabled;
2. speak a protocol of the request's Kind;
3. serve the request's model;
4. support every feature the request needs;
5. satisfy `Registry.CanConvert(request protocol, endpoint protocol)`;
6. speak the client's protocol, when the request is `Stateful`.

The default `NativeFirst` strategy orders the remaining endpoints:

1. **Native first**: endpoints that speak the client's protocol (`Candidate.Native`) preserve the request protocol, applying model rewriting and any explicitly configured overrides or normalization.
2. **Convertible next**: endpoints with other protocols of the same Kind, reached through direct conversion.
3. **Priority within a tier**: lower `Priority` first.
4. **Weight within a priority**: a weighted shuffle without replacement; weights below 1 count as 1.

Native endpoints always precede convertible ones, whatever their priority. Supply a custom `Strategy` with `route.WithStrategy` for other policies; affinity is applied after the strategy.

When nothing qualifies, `Route` fails:

- with `ClassInvalidModel` if no endpoint serves the model;
- with `ClassUnsupported` if some endpoints serve the model but none can take the request.

## Upstream-owned state

`Inspect` reports the upstream state a request refers to in `RequestInfo.Affinity`, as keys such as `response:resp_1`, `conversation:conv_1`, `interaction:i1`, `file:file-1` or `cached-content:cachedContents/c1`. References to context that conversion cannot carry (`previous_response_id`, `conversation`, `prompt` and `item_reference` in Responses, `previous_interaction_id`, Messages `container`, Gemini `cachedContent`) also set `RequestInfo.Stateful`.

- **Native only**: a `Stateful` request only goes to endpoints of the client's protocol. Converting it would silently drop the context.
- **Owner first**: `route.Affinity` remembers which endpoint owns a key. The owner of the first known key moves to the front. For a `Stateful` request it is returned even while unhealthy, because no other endpoint can serve it.
- **Learning owners**: exchanges that implement `apix.StateReporter` report the state their upstream created; the gateway passes it to `Router.Remember`. Native Responses exchanges report stored response IDs and Interactions exchanges report interaction IDs. Call `Router.Remember` yourself for state created elsewhere, such as uploaded files.

The default `AffinityCache` keeps the 65536 most recently used keys in process. Replace it with `route.WithAffinity` to share affinity across instances, or pass `nil` to disable it.

## Health

`Router.Report(id, Outcome)` records an attempt. The default `Breaker` works as follows:

- **Open**: after `Threshold` consecutive failures (default 3), the endpoint is skipped for `Cooldown` (default 30 seconds). A failure that carries `apix.Error.RetryAfter` (from an upstream `Retry-After` header) pauses it for at least that long, even below the threshold.
- **Half-open**: once the pause ends, the endpoint takes one trial request at a time. Executors call `Router.Begin(candidate)` right before sending; it admits the trial and refuses concurrent ones. A success closes the breaker and a further failure reopens it. A trial whose outcome is never reported frees its slot after `Cooldown`.
- **What counts**: authentication, rate limit, quota, billing, upstream server, and network errors. Unknown models, translation and unsupported-request errors describe the request, which a client controls, and are ignored; a client cannot push a healthy endpoint out of rotation by sending bad model names to a `"*"` mapping.
- **All open**: when every eligible endpoint is unhealthy, the router still returns them all in normal order rather than refusing service, and `Begin` admits them.

Health is per process. To share it across instances, implement `route.Health`, and optionally `route.Prober` for trial gating.

## Updates

`Router.Update(endpoints)` replaces the endpoint set atomically:

- requests already routing keep the old set;
- invalid updates, including options a codec rejects, leave the active configuration unchanged;
- health is kept per endpoint ID;
- the router copies the maps and slices it receives, so later changes by the caller do not affect it.

`Gateway.Update` applies the same rules to gateway upstreams; see [gateway](gateway.md#updates).

## Executing candidates

An executor tries the candidates in order:

1. call `Router.Begin(candidate)` right before sending, and skip the candidate when it returns false (a half-open trial is already in flight);
2. convert the request to `Endpoint.Protocol` with `Candidate.UpstreamModel` and `Endpoint.Options`, then send it;
3. call `Router.Report(endpointID, Outcome)` for every attempt that was sent, so health tracking sees it;
4. call `Router.Remember(key, endpointID)` for state the upstream created.

Package `gateway` does all of this, and decides which failures move to the next candidate; see [gateway](gateway.md#commit-point-and-failover). Executors of your own follow the same steps.
