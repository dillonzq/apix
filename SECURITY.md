# Security policy

## Supported versions

Security fixes land on `main` and in the latest release. Older tags are not maintained.

| Version | Supported |
| --- | --- |
| `main` | ✅ |
| Latest release | ✅ |
| Older releases | ❌ |

## Reporting a vulnerability

Use [private vulnerability reporting](https://github.com/dillonzq/apix/security/advisories/new). Do not open a public issue for a suspected vulnerability.

A useful report contains:

- the affected version or commit;
- the entry point involved (an `Exchange` method, `Registry.Inspect`, `route.Router`, `gateway.Gateway.Do` or `Handler`, `sse.Decoder`);
- the input bytes or SSE frames that trigger it, reduced as far as you can;
- the impact you observed or expect.

Redact credentials before sending anything. The library's redaction helpers are covered by tests, but they are not a substitute for removing real secrets from a report.

## What counts as a security issue here

The core packages perform no network calls, hold no credentials, and write no files. The optional `gateway` package sends requests with the API keys it is configured with. The issues that matter are the ones that leak or corrupt data crossing a trust boundary:

- secrets echoed into an error message, a diagnostic, or stream output, for example an upstream body that repeats an `Authorization` header;
- malformed upstream or client input that makes a converter panic, hang, or emit data belonging to another request;
- conversion that silently rewrites or drops caller-visible protocol data in a way a hostile upstream can steer, because callers act on what this library reports;
- the gateway forwarding client credentials upstream, sending an upstream's API key to a different upstream, or retrying after output reached the client.

Out of scope: TLS and proxy configuration of the `http.Client` you supply, authentication of your own clients in front of `gateway.Handler`, provider policy, model capability checks, and model-name validation. Those belong to the caller.

## What to expect

Acknowledgement within a few days. Fixes land on `main` and ship as a patch release when the report concerns released behavior. The advisory is published once a fix is available, crediting the reporter unless they prefer otherwise.
